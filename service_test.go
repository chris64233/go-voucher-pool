package govoucherpool

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可手动推进的时钟，避免测试依赖真实时间。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	svc     *Service
	clock   *fakeClock
	batchID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := newFakeClock()
	svc, err := NewService(clock.now)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	start := clock.now()
	b, err := svc.CreateBatch(CreateBatchInput{
		Name:        "满100减20券",
		FaceValue:   2000,
		Scope:       Scope{SKUs: []string{"sku-1", "sku-2"}, Stores: []string{"store-1"}},
		EffectiveAt: start.Add(-time.Hour),
		ExpiresAt:   start.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	return &fixture{svc: svc, clock: clock, batchID: b.ID}
}

func (f *fixture) register(t *testing.T, code string) string {
	t.Helper()
	id, err := f.svc.RegisterVoucher(f.batchID, code)
	if err != nil {
		t.Fatalf("RegisterVoucher: %v", err)
	}
	return id
}

func TestCreateBatch_Validation(t *testing.T) {
	f := newFixture(t)
	now := f.clock.now()

	cases := []CreateBatchInput{
		{Name: "zero", FaceValue: 0, EffectiveAt: now, ExpiresAt: now.Add(time.Hour)},
		{Name: "neg", FaceValue: -1, EffectiveAt: now, ExpiresAt: now.Add(time.Hour)},
		{Name: "bad-window", FaceValue: 100, EffectiveAt: now, ExpiresAt: now},
		{Name: "reversed", FaceValue: 100, EffectiveAt: now.Add(time.Hour), ExpiresAt: now},
	}
	for i, in := range cases {
		if _, err := f.svc.CreateBatch(in); !errors.Is(err, ErrInvalidBatch) {
			t.Fatalf("case %d: want ErrInvalidBatch, got %v", i, err)
		}
	}
	if _, err := f.svc.GetBatch("bat_missing"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("GetBatch missing: want ErrBatchNotFound, got %v", err)
	}
}

func TestRegisterVoucher_DuplicateAndUnknownBatch(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.RegisterVoucher("bat_missing", "code-x"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("unknown batch: want ErrBatchNotFound, got %v", err)
	}
	if _, err := f.svc.RegisterVoucher(f.batchID, ""); !errors.Is(err, ErrEmptyCode) {
		t.Fatalf("empty code: want ErrEmptyCode, got %v", err)
	}
	f.register(t, "CODE-A")
	if _, err := f.svc.RegisterVoucher(f.batchID, "CODE-A"); !errors.Is(err, ErrCodeAlreadyRegistered) {
		t.Fatalf("dup code: want ErrCodeAlreadyRegistered, got %v", err)
	}
}

func TestCodeNeverStoredOrLeaked(t *testing.T) {
	f := newFixture(t)
	const secret = "SUPER-SECRET-CODE-42"
	id := f.register(t, secret)

	snap, err := f.svc.GetVoucher(id)
	if err != nil {
		t.Fatalf("GetVoucher: %v", err)
	}
	if fmt.Sprintf("%+v", snap) == "" {
		t.Fatal("empty snapshot")
	}

	// 扫描内部状态：任何字段都不允许出现明文券码。
	f.svc.mu.Lock()
	v := f.svc.vouchers[id]
	flat := fmt.Sprintf("%+v", *v)
	f.svc.mu.Unlock()
	if strings.Contains(flat, secret) {
		t.Fatalf("plaintext code found in stored voucher: %q", flat)
	}
	if strings.Contains(v.CodeDigest, secret) {
		t.Fatal("digest contains plaintext")
	}
	if v.CodeDigest == f.svc.Digest("") || v.CodeDigest == secret {
		t.Fatal("digest looks invalid")
	}

	// 错误信息不得回显券码或摘要。
	_, err = f.svc.Reserve("ord-1", "WRONG-CODE", time.Minute)
	if !errors.Is(err, ErrCodeNotFound) {
		t.Fatalf("wrong code: want ErrCodeNotFound, got %v", err)
	}
	for _, msg := range []string{err.Error(), ErrCodeNotFound.Error()} {
		if strings.Contains(msg, "WRONG-CODE") {
			t.Fatalf("error leaks input code: %q", msg)
		}
	}
	_, err = f.svc.Confirm("ord-1", "WRONG-CODE", 1)
	if !errors.Is(err, ErrCodeNotFound) || strings.Contains(err.Error(), "WRONG-CODE") {
		t.Fatalf("confirm error leaks/not found: %v", err)
	}
	if err := f.svc.Release("ord-1", "WRONG-CODE", 1); !errors.Is(err, ErrCodeNotFound) {
		t.Fatalf("release wrong code: %v", err)
	}

	// Snapshot 不携带摘要。
	if snap.ID != id || snap.FaceValue != 2000 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestReserve_IdempotentSameOrder(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-1")

	r1, err := f.svc.Reserve("order-A", "CODE-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if r1.Version != 1 || r1.VoucherID != id {
		t.Fatalf("unexpected reserve: %+v", r1)
	}
	r2, err := f.svc.Reserve("order-A", "CODE-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if r2.Version != r1.Version || r2.OrderID != "order-A" {
		t.Fatalf("repeat reserve must return same hold: %+v vs %+v", r1, r2)
	}
	if !r2.ExpiresAt.Equal(r1.ExpiresAt) {
		t.Fatalf("repeat reserve must not extend/change expiry: %+v vs %+v", r1, r2)
	}

	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateHeld || snap.Version != 1 || snap.HeldBy != "order-A" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestReserve_ContentionDifferentOrders(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-2")

	if _, err := f.svc.Reserve("order-A", "CODE-2", 10*time.Minute); err != nil {
		t.Fatalf("reserve A: %v", err)
	}
	if _, err := f.svc.Reserve("order-B", "CODE-2", 10*time.Minute); !errors.Is(err, ErrVoucherHeld) {
		t.Fatalf("reserve B while held: want ErrVoucherHeld, got %v", err)
	}

	// 到期前推进时间后，B 可以抢占；版本必须递增。
	f.clock.advance(11 * time.Minute)
	r, err := f.svc.Reserve("order-B", "CODE-2", 10*time.Minute)
	if err != nil {
		t.Fatalf("reserve B after expiry: %v", err)
	}
	if r.Version != 2 {
		t.Fatalf("re-hold version must increment, got %d", r.Version)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.HeldBy != "order-B" || snap.Version != 2 {
		t.Fatalf("unexpected snapshot after re-hold: %+v", snap)
	}
}

func TestConfirm_LateConfirmFromOldOrderRejected(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-3")

	ra, err := f.svc.Reserve("order-A", "CODE-3", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Minute)
	// 超时推进把券回收。
	reclaimed := f.svc.ExpireHolds()
	if len(reclaimed) != 1 || reclaimed[0] != id {
		t.Fatalf("ExpireHolds = %v", reclaimed)
	}
	rb, err := f.svc.Reserve("order-B", "CODE-3", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Version != ra.Version+1 {
		t.Fatalf("version should advance: %d -> %d", ra.Version, rb.Version)
	}

	// 旧订单 A 用旧版本号迟到确认：必须失败。
	if _, err := f.svc.Confirm("order-A", "CODE-3", ra.Version); !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("late confirm A: want ErrHoldMismatch, got %v", err)
	}
	// 即使旧订单碰巧传了新版本号，也因订单不匹配失败。
	if _, err := f.svc.Confirm("order-A", "CODE-3", rb.Version); !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("late confirm A new version: want ErrHoldMismatch, got %v", err)
	}

	// B 正常确认成功。
	rec, err := f.svc.Confirm("order-B", "CODE-3", rb.Version)
	if err != nil {
		t.Fatalf("confirm B: %v", err)
	}
	if rec.FaceValue != 2000 || rec.OrderID != "order-B" {
		t.Fatalf("bad redemption: %+v", rec)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateRedeemed || snap.RedeemOrder != "order-B" {
		t.Fatalf("unexpected: %+v", snap)
	}

	// A 的任何后续确认都无法改变终态。
	if _, err := f.svc.Confirm("order-A", "CODE-3", rb.Version); !errors.Is(err, ErrVoucherRedeemed) {
		t.Fatalf("confirm A after redeem: want ErrVoucherRedeemed, got %v", err)
	}
}

func TestConfirm_ExpiredHoldCannotConfirm(t *testing.T) {
	f := newFixture(t)
	f.register(t, "CODE-4")
	r, _ := f.svc.Reserve("order-A", "CODE-4", time.Minute)
	f.clock.advance(2 * time.Minute)

	// 未做推进也没关系：到期的预占不允许确认。
	if _, err := f.svc.Confirm("order-A", "CODE-4", r.Version); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("expired confirm: want ErrHoldExpired, got %v", err)
	}
	// 推进后券可用，其他订单可预占。
	f.svc.ExpireHolds()
	if _, err := f.svc.Reserve("order-B", "CODE-4", time.Minute); err != nil {
		t.Fatalf("reserve after expiry: %v", err)
	}
}

func TestConfirm_IdempotentAndValueCountedOnce(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-5")
	r, _ := f.svc.Reserve("order-A", "CODE-5", 5*time.Minute)

	rec1, err := f.svc.Confirm("order-A", "CODE-5", r.Version)
	if err != nil {
		t.Fatal(err)
	}
	// 重复确认（同版本/旧版本号）幂等返回同一条记录。
	rec2, err := f.svc.Confirm("order-A", "CODE-5", r.Version)
	if err != nil {
		t.Fatalf("idempotent confirm: %v", err)
	}
	if rec1.At != rec2.At || rec1.VoucherID != rec2.VoucherID {
		t.Fatalf("confirm not idempotent: %+v vs %+v", rec1, rec2)
	}
	if got := f.svc.OrderRedeemedValue("order-A"); got != 2000 {
		t.Fatalf("order value = %d, want 2000", got)
	}
	rs := f.svc.OrderRedemptions("order-A")
	if len(rs) != 1 || rs[0].VoucherID != id {
		t.Fatalf("redemptions = %+v", rs)
	}

	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateRedeemed {
		t.Fatalf("state = %s", snap.State)
	}
	// 已核销不能释放。
	if err := f.svc.Release("order-A", "CODE-5", r.Version); !errors.Is(err, ErrVoucherRedeemed) {
		t.Fatalf("release redeemed: want ErrVoucherRedeemed, got %v", err)
	}
}

func TestConfirm_WrongVersionRejected(t *testing.T) {
	f := newFixture(t)
	f.register(t, "CODE-6")
	r, _ := f.svc.Reserve("order-A", "CODE-6", 5*time.Minute)
	if _, err := f.svc.Confirm("order-A", "CODE-6", r.Version+99); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("wrong version: want ErrVersionMismatch, got %v", err)
	}
}

func TestRelease_ReturnsVoucherAndAdvancesVersion(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-7")
	r, _ := f.svc.Reserve("order-A", "CODE-7", 5*time.Minute)

	// 其他订单不能释放。
	if err := f.svc.Release("order-B", "CODE-7", r.Version); !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("release by other: want ErrHoldMismatch, got %v", err)
	}
	// 错误版本不能释放。
	if err := f.svc.Release("order-A", "CODE-7", r.Version+1); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("release stale version: want ErrVersionMismatch, got %v", err)
	}
	if err := f.svc.Release("order-A", "CODE-7", r.Version); err != nil {
		t.Fatalf("release: %v", err)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateAvailable || snap.Version != 0 || snap.HeldBy != "" {
		t.Fatalf("after release: %+v", snap)
	}

	// B 重新预占成功，版本为 2。
	rb, err := f.svc.Reserve("order-B", "CODE-7", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Version != 2 {
		t.Fatalf("new hold version = %d, want 2", rb.Version)
	}
	// A 用旧版本迟到确认失败。
	if _, err := f.svc.Confirm("order-A", "CODE-7", r.Version); !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("late confirm after release: %v", err)
	}
	// 对已释放的版本重复释放：券已被 B 持有 -> 不匹配/版本过期。
	if err := f.svc.Release("order-A", "CODE-7", r.Version); !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("double release old version: want ErrHoldMismatch, got %v", err)
	}
}

func TestExpireHolds_BatchAndTTLClampedToBatchExpiry(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-8")

	// 批次在 24h 后失效；预占 TTL 48h 必须被截断到批次失效点。
	r, err := f.svc.Reserve("order-A", "CODE-8", 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := f.svc.GetBatch(f.batchID)
	if !r.ExpiresAt.Equal(batch.ExpiresAt) {
		t.Fatalf("hold expiry %v not clamped to batch expiry %v", r.ExpiresAt, batch.ExpiresAt)
	}

	f.clock.advance(23 * time.Hour)
	if reclaimed := f.svc.ExpireHolds(); len(reclaimed) != 0 {
		t.Fatalf("nothing should expire yet: %v", reclaimed)
	}
	f.clock.advance(2 * time.Hour) // 超过批次失效时间
	reclaimed := f.svc.ExpireHolds()
	if len(reclaimed) != 1 || reclaimed[0] != id {
		t.Fatalf("ExpireHolds = %v", reclaimed)
	}
	// 批次已过失效期，不能再预占。
	if _, err := f.svc.Reserve("order-A", "CODE-8", time.Minute); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("reserve after batch expiry: want ErrBatchNotInEffect, got %v", err)
	}
}

func TestVoidBatch_PreservesRedeemedBlocksPending(t *testing.T) {
	f := newFixture(t)
	idRedeemed := f.register(t, "CODE-R")
	idHeld := f.register(t, "CODE-H")
	idAvail := f.register(t, "CODE-V")

	// 一张券先核销。
	rr, _ := f.svc.Reserve("order-R", "CODE-R", 5*time.Minute)
	if _, err := f.svc.Confirm("order-R", "CODE-R", rr.Version); err != nil {
		t.Fatal(err)
	}
	// 一张券被预占但未确认。
	rh, _ := f.svc.Reserve("order-H", "CODE-H", 5*time.Minute)

	voided, err := f.svc.VoidBatch(f.batchID, "运营作废")
	if err != nil {
		t.Fatalf("VoidBatch: %v", err)
	}
	if len(voided) != 2 {
		t.Fatalf("voided = %v, want 2 vouchers", voided)
	}

	// 已核销保留。
	snapR, _ := f.svc.GetVoucher(idRedeemed)
	if snapR.State != StateRedeemed || snapR.RedeemOrder != "order-R" {
		t.Fatalf("redeemed must be preserved: %+v", snapR)
	}
	if f.svc.OrderRedeemedValue("order-R") != 2000 {
		t.Fatal("redeemed value must survive void")
	}
	// 预占中的券作废，迟到确认被拒绝。
	snapH, _ := f.svc.GetVoucher(idHeld)
	if snapH.State != StateVoided || snapH.HeldBy != "" {
		t.Fatalf("held voucher should be voided: %+v", snapH)
	}
	if _, err := f.svc.Confirm("order-H", "CODE-H", rh.Version); !errors.Is(err, ErrVoucherVoided) {
		t.Fatalf("confirm after void: want ErrVoucherVoided, got %v", err)
	}
	if err := f.svc.Release("order-H", "CODE-H", rh.Version); !errors.Is(err, ErrVoucherVoided) {
		t.Fatalf("release after void: want ErrVoucherVoided, got %v", err)
	}
	// 可用券也作废。
	if snap, _ := f.svc.GetVoucher(idAvail); snap.State != StateVoided {
		t.Fatalf("available voucher should be voided: %+v", snap)
	}
	// 批次作废后不能再预占（券已是作废终态，错误具体到券）。
	if _, err := f.svc.Reserve("order-X", "CODE-V", time.Minute); !errors.Is(err, ErrVoucherVoided) {
		t.Fatalf("reserve voided voucher: want ErrVoucherVoided, got %v", err)
	}
	// 重复作废幂等（没有新的券被作废，已核销与已作废均跳过）。
	voided2, err := f.svc.VoidBatch(f.batchID, "再次作废")
	if err != nil || len(voided2) != 0 {
		t.Fatalf("repeat void = %v, %v", voided2, err)
	}
	// 未知批次。
	if _, err := f.svc.VoidBatch("bat_missing", "x"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("void missing batch: %v", err)
	}
}

func TestVoidBatch_ExpiredHoldCannotCrossVoid(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, "CODE-EH")
	r, _ := f.svc.Reserve("order-A", "CODE-EH", time.Minute)
	f.clock.advance(2 * time.Minute) // 预占到期，但尚未推进

	if _, err := f.svc.VoidBatch(f.batchID, "作废"); err != nil {
		t.Fatal(err)
	}
	// 即便持有原版本，到期预占也不能越过作废确认。
	if _, err := f.svc.Confirm("order-A", "CODE-EH", r.Version); !errors.Is(err, ErrVoucherVoided) {
		t.Fatalf("confirm expired hold after void: want ErrVoucherVoided, got %v", err)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateVoided {
		t.Fatalf("state = %s", snap.State)
	}
}

func TestBatchNotInEffect(t *testing.T) {
	clock := newFakeClock()
	svc, _ := NewService(clock.now)
	start := clock.now()
	b, err := svc.CreateBatch(CreateBatchInput{
		Name:        "未来批次",
		FaceValue:   500,
		EffectiveAt: start.Add(time.Hour),
		ExpiresAt:   start.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.RegisterVoucher(b.ID, "FUTURE-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve("o", "FUTURE-1", time.Minute); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("reserve before effective: %v", err)
	}
	clock.advance(90 * time.Minute)
	r, err := svc.Reserve("o", "FUTURE-1", 20*time.Minute)
	if err != nil {
		t.Fatalf("reserve in window: %v", err)
	}
	clock.advance(25 * time.Minute) // 跨过失效点
	if _, err := svc.Confirm("o", "FUTURE-1", r.Version); !errors.Is(err, ErrHoldExpired) {
		// 预占先到期（TTL 20m），返回 ErrHoldExpired；面值窗口与 TTL 双重保护。
		t.Fatalf("confirm after both expiries: want ErrHoldExpired, got %v", err)
	}
	if snap, _ := svc.GetVoucher(id); snap.State != StateHeld {
		t.Fatalf("state = %s", snap.State)
	}
}

func TestScope_Matches(t *testing.T) {
	s := Scope{SKUs: []string{"sku-1", "sku-2"}, Stores: []string{"store-1"}}
	if !s.Matches(nil, []string{"store-1"}, []string{"sku-2"}) {
		t.Fatal("both dimensions in scope should match")
	}
	if s.Matches(nil, []string{"store-9"}, nil) {
		t.Fatal("store outside scope must not match")
	}
	if s.Matches(nil, []string{"store-1"}, []string{"sku-9"}) {
		t.Fatal("sku outside scope must not match")
	}
	empty := Scope{}
	if !empty.Matches(nil, []string{"anything"}, nil) {
		t.Fatal("empty scope should be universal")
	}
	if s.Matches(nil, nil, nil) {
		t.Fatal("no requested dimensions against restricted scope should not match")
	}
}
