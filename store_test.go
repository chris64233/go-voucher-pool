package govoucherpool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 是可手动推进的时钟。
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

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	s, err := New(WithClock(clk.now))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, clk
}

// setupBatch 创建一个面值 1000、有效期覆盖当前时刻的批次并登记 n 张券。
func setupBatch(t *testing.T, s *Store, clk *fakeClock, n int) (string, []string) {
	t.Helper()
	base := clk.now()
	batchID, err := s.CreateBatch(context.Background(), CreateBatchInput{
		Scope:       Scope{CategoryIDs: []string{"cat-1"}, ShopIDs: []string{"shop-1"}},
		FaceValue:   1000,
		EffectiveAt: base.Add(-time.Hour),
		ExpiresAt:   base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	codes := make([]string, n)
	for i := range codes {
		c, err := GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		codes[i] = c
	}
	if _, err := s.RegisterVouchers(context.Background(), batchID, codes); err != nil {
		t.Fatalf("RegisterVouchers: %v", err)
	}
	return batchID, codes
}

func reserveOne(t *testing.T, s *Store, batchID, orderID, code string, ttl time.Duration) *Reservation {
	t.Helper()
	res, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: orderID, Codes: []string{code}, TTL: ttl,
	})
	if err != nil {
		t.Fatalf("Reserve order=%s: %v", orderID, err)
	}
	return res
}

func TestCreateBatchValidation(t *testing.T) {
	s, clk := newTestStore(t)
	now := clk.now()

	cases := []struct {
		name string
		in   CreateBatchInput
	}{
		{"zero face value", CreateBatchInput{FaceValue: 0, EffectiveAt: now, ExpiresAt: now.Add(time.Hour)}},
		{"negative face value", CreateBatchInput{FaceValue: -1, EffectiveAt: now, ExpiresAt: now.Add(time.Hour)}},
		{"expires before effective", CreateBatchInput{FaceValue: 100, EffectiveAt: now, ExpiresAt: now.Add(-time.Hour)}},
		{"expires equals effective", CreateBatchInput{FaceValue: 100, EffectiveAt: now, ExpiresAt: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateBatch(context.Background(), tc.in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}

	batchID, err := s.CreateBatch(context.Background(), CreateBatchInput{
		FaceValue: 500, EffectiveAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	got, err := s.GetBatch(context.Background(), batchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if got.FaceValue != 500 {
		t.Fatalf("FaceValue = %d, want 500", got.FaceValue)
	}
	if _, err := s.GetBatch(context.Background(), "bat_missing"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("want ErrBatchNotFound, got %v", err)
	}
}

func TestRegisterDuplicateAndUnknownCode(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 2)

	// 同批次重复登记。
	if _, err := s.RegisterVouchers(context.Background(), batchID, []string{codes[0]}); !errors.Is(err, ErrCodeAlreadyRegistered) {
		t.Fatalf("dup in same batch: want ErrCodeAlreadyRegistered, got %v", err)
	}
	// 入参自带重复。
	if _, err := s.RegisterVouchers(context.Background(), batchID, []string{"abc", "abc"}); !errors.Is(err, ErrCodeAlreadyRegistered) {
		t.Fatalf("dup in request: want ErrCodeAlreadyRegistered, got %v", err)
	}
	// 不存在的券码查询。
	if _, err := s.GetVoucher(context.Background(), batchID, "definitely-not-a-code"); !errors.Is(err, ErrVoucherNotFound) {
		t.Fatalf("want ErrVoucherNotFound, got %v", err)
	}
	// 未知批次。
	if _, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: "bat_x", OrderID: "o1", Codes: codes[:1], TTL: time.Minute,
	}); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("want ErrBatchNotFound, got %v", err)
	}
}

func TestReserveIdempotentSameOrder(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	r1 := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	r2 := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)

	if !r2.Items[0].Reused {
		t.Fatal("second reserve must report Reused=true")
	}
	if r1.Items[0].Version != r2.Items[0].Version {
		t.Fatalf("version changed on idempotent reserve: %d vs %d", r1.Items[0].Version, r2.Items[0].Version)
	}
	if !r1.Items[0].ExpiresAt.Equal(r2.Items[0].ExpiresAt) {
		t.Fatalf("expiry changed on idempotent reserve: %v vs %v", r1.Items[0].ExpiresAt, r2.Items[0].ExpiresAt)
	}

	v, err := s.GetVoucher(context.Background(), batchID, codes[0])
	if err != nil {
		t.Fatalf("GetVoucher: %v", err)
	}
	if v.Status != StatusReserved || v.OrderID != "order-A" {
		t.Fatalf("unexpected voucher: %+v", v)
	}
}

func TestReserveContentionDifferentOrders(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	_, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "order-B", Codes: []string{codes[0]}, TTL: time.Minute,
	})
	if !errors.Is(err, ErrVoucherBusy) {
		t.Fatalf("want ErrVoucherBusy, got %v", err)
	}

	// A 之外的订单也不能确认。
	if _, err := s.Confirm(context.Background(), batchID, "order-B", codes[0], 1); !errors.Is(err, ErrWrongOrder) {
		t.Fatalf("want ErrWrongOrder, got %v", err)
	}
}

func TestConfirmHappyPathWritesRedemption(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 2)

	r := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	rec, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], r.Items[0].Version)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if rec.FaceValue != 1000 || rec.OrderID != "order-A" {
		t.Fatalf("bad redemption: %+v", rec)
	}

	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v.Status != StatusRedeemed {
		t.Fatalf("status = %s, want redeemed", v.Status)
	}

	got, err := s.GetRedemption(context.Background(), v.ID)
	if err != nil {
		t.Fatalf("GetRedemption: %v", err)
	}
	if got.VoucherID != v.ID || got.FaceValue != 1000 {
		t.Fatalf("bad stored redemption: %+v", got)
	}

	// 订单状态：面值合计一次，券 ID 一次。
	st, err := s.GetOrder(context.Background(), "order-A")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if st.TotalFaceValue != 1000 || len(st.RedeemedVoucherIDs) != 1 || len(st.Redemptions) != 1 {
		t.Fatalf("bad order status: %+v", st)
	}
	if len(st.ActiveReservations) != 0 {
		t.Fatalf("redeemed voucher must not appear in active reservations: %+v", st.ActiveReservations)
	}

	// 另一张券仍可用。
	v2, _ := s.GetVoucher(context.Background(), batchID, codes[1])
	if v2.Status != StatusAvailable {
		t.Fatalf("voucher2 status = %s, want available", v2.Status)
	}
}

func TestConfirmVersionMismatch(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	r := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	// 携带错误版本。
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], r.Items[0].Version+99); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("want ErrVersionMismatch, got %v", err)
	}
	// 失败确认不得改变状态。
	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v.Status != StatusReserved {
		t.Fatalf("status = %s, want reserved", v.Status)
	}
}

func TestExpiryReleaseAndReReserve(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)

	// 未到期，推进器不动作。
	n, err := s.AdvanceExpiry(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("AdvanceExpiry before ttl = %d, %v", n, err)
	}

	clk.advance(61 * time.Second)
	n, err = s.AdvanceExpiry(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("AdvanceExpiry after ttl = %d, %v", n, err)
	}
	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v.Status != StatusAvailable || v.OrderID != "" {
		t.Fatalf("voucher not released: %+v", v)
	}

	// 再推进一次幂等。
	n, _ = s.AdvanceExpiry(context.Background())
	if n != 0 {
		t.Fatalf("idempotent AdvanceExpiry = %d, want 0", n)
	}

	// B 重新预占成功。
	rb := reserveOne(t, s, batchID, "order-B", codes[0], time.Minute)
	if rb.Items[0].Version <= 1 {
		t.Fatalf("new reservation should bump version, got %d", rb.Items[0].Version)
	}
}

func TestLateConfirmAfterExpiryCannotRedeem(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	// 场景一：到期后 A 用旧版本迟到确认（无人接管）。
	ra := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	clk.advance(2 * time.Minute)
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], ra.Items[0].Version); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("late confirm without takeover: want ErrReservationExpired, got %v", err)
	}
	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s, want available after expired confirm", v.Status)
	}

	// 场景二（关键）：B 在到期后接管，A 的迟到确认绝不能核销 B 持有的券。
	rb := reserveOne(t, s, batchID, "order-B", codes[0], time.Minute)
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], ra.Items[0].Version); !errors.Is(err, ErrWrongOrder) {
		t.Fatalf("late confirm by old owner: want ErrWrongOrder, got %v", err)
	}
	// A 就算伪造/猜成旧版本号也不行：版本与当前不符。
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], rb.Items[0].Version); !errors.Is(err, ErrWrongOrder) {
		t.Fatalf("old owner with new version: want ErrWrongOrder, got %v", err)
	}

	// B 用当前版本确认成功，面值计入 B。
	if _, err := s.Confirm(context.Background(), batchID, "order-B", codes[0], rb.Items[0].Version); err != nil {
		t.Fatalf("B confirm: %v", err)
	}
	stA, err := s.GetOrder(context.Background(), "order-A")
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("order A should have no bookings, got %+v err=%v", stA, err)
	}
	stB, _ := s.GetOrder(context.Background(), "order-B")
	if stB.TotalFaceValue != 1000 {
		t.Fatalf("order B total = %d, want 1000", stB.TotalFaceValue)
	}
}

func TestActiveRelease(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	if err := s.Release(context.Background(), batchID, "order-B", codes[0]); !errors.Is(err, ErrWrongOrder) {
		t.Fatalf("release by other order: want ErrWrongOrder, got %v", err)
	}
	if err := s.Release(context.Background(), batchID, "order-A", codes[0]); err != nil {
		t.Fatalf("Release: %v", err)
	}
	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v.Status != StatusAvailable {
		t.Fatalf("status = %s, want available", v.Status)
	}
	// 重复释放。
	if err := s.Release(context.Background(), batchID, "order-A", codes[0]); !errors.Is(err, ErrVoucherNotReserved) {
		t.Fatalf("double release: want ErrVoucherNotReserved, got %v", err)
	}
	// 释放后旧版本无法确认。
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], 1); !errors.Is(err, ErrVoucherNotReserved) {
		t.Fatalf("confirm after release: want ErrVoucherNotReserved, got %v", err)
	}
}

func TestDoubleConfirmCountsOnce(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)
	r := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)

	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], r.Items[0].Version); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	// 第二次确认（哪怕版本恰好递增后的值）不得再次入账。
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], r.Items[0].Version+1); !errors.Is(err, ErrVoucherRedeemed) {
		t.Fatalf("second confirm: want ErrVoucherRedeemed, got %v", err)
	}
	st, _ := s.GetOrder(context.Background(), "order-A")
	if st.TotalFaceValue != 1000 || len(st.RedeemedVoucherIDs) != 1 {
		t.Fatalf("double counting: %+v", st)
	}
	if err := s.Release(context.Background(), batchID, "order-A", codes[0]); !errors.Is(err, ErrVoucherRedeemed) {
		t.Fatalf("release redeemed: want ErrVoucherRedeemed, got %v", err)
	}
}

func TestVoidBatchPreservesRedeemed(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 3)

	r0 := reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], r0.Items[0].Version); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	// codes[1] 预占未确认，codes[2] 可用。
	reserveOne(t, s, batchID, "order-B", codes[1], time.Minute)

	res, err := s.VoidBatch(context.Background(), batchID)
	if err != nil {
		t.Fatalf("VoidBatch: %v", err)
	}
	if res.VoidedCount != 2 {
		t.Fatalf("VoidedCount = %d, want 2", res.VoidedCount)
	}

	// 已核销保留。
	v0, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v0.Status != StatusRedeemed {
		t.Fatalf("redeemed voucher changed: %s", v0.Status)
	}
	rec0, _ := s.GetRedemption(context.Background(), v0.ID)
	if rec0.OrderID != "order-A" {
		t.Fatalf("redemption lost: %+v", rec0)
	}
	stA, _ := s.GetOrder(context.Background(), "order-A")
	if stA.TotalFaceValue != 1000 {
		t.Fatalf("order A total changed after void: %d", stA.TotalFaceValue)
	}

	// 预占中与可用的都变作废，预占不能再确认。
	v1, _ := s.GetVoucher(context.Background(), batchID, codes[1])
	if v1.Status != StatusVoided {
		t.Fatalf("reserved voucher status = %s, want voided", v1.Status)
	}
	if _, err := s.Confirm(context.Background(), batchID, "order-B", codes[1], r0.Items[0].Version); !errors.Is(err, ErrVoucherVoided) {
		t.Fatalf("confirm voided reservation: want ErrVoucherVoided, got %v", err)
	}
	v2, _ := s.GetVoucher(context.Background(), batchID, codes[2])
	if v2.Status != StatusVoided {
		t.Fatalf("available voucher status = %s, want voided", v2.Status)
	}

	// 作废后不能再预占/登记/重复作废。
	if _, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "o", Codes: []string{codes[2]}, TTL: time.Minute,
	}); !errors.Is(err, ErrBatchVoided) {
		t.Fatalf("reserve after void: want ErrBatchVoided, got %v", err)
	}
	if _, err := s.RegisterVouchers(context.Background(), batchID, []string{"new-code"}); !errors.Is(err, ErrBatchVoided) {
		t.Fatalf("register after void: want ErrBatchVoided, got %v", err)
	}
	if _, err := s.VoidBatch(context.Background(), batchID); !errors.Is(err, ErrBatchVoided) {
		t.Fatalf("double void: want ErrBatchVoided, got %v", err)
	}
	// 未知批次作废。
	if _, err := s.VoidBatch(context.Background(), "bat_nope"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("void missing: want ErrBatchNotFound, got %v", err)
	}
}

func TestReserveNotInEffectWindow(t *testing.T) {
	s, clk := newTestStore(t)
	now := clk.now()

	before, err := s.CreateBatch(context.Background(), CreateBatchInput{
		FaceValue: 100, EffectiveAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := s.RegisterVouchers(context.Background(), before, []string{"c1"}); err != nil {
		t.Fatalf("RegisterVouchers: %v", err)
	}
	if _, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: before, OrderID: "o", Codes: []string{"c1"}, TTL: time.Minute,
	}); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("before effective: want ErrBatchNotInEffect, got %v", err)
	}

	after, err := s.CreateBatch(context.Background(), CreateBatchInput{
		FaceValue: 100, EffectiveAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := s.RegisterVouchers(context.Background(), after, []string{"c2"}); err != nil {
		t.Fatalf("RegisterVouchers: %v", err)
	}
	if _, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: after, OrderID: "o", Codes: []string{"c2"}, TTL: time.Minute,
	}); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("after expiry: want ErrBatchNotInEffect, got %v", err)
	}
}

func TestReserveExpiryClampedToBatchExpiry(t *testing.T) {
	s, clk := newTestStore(t)
	now := clk.now()
	batchID, err := s.CreateBatch(context.Background(), CreateBatchInput{
		FaceValue: 100, EffectiveAt: now.Add(-time.Hour), ExpiresAt: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := s.RegisterVouchers(context.Background(), batchID, []string{"c1"}); err != nil {
		t.Fatalf("RegisterVouchers: %v", err)
	}
	r, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "o", Codes: []string{"c1"}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	wantExpire := now.Add(30 * time.Second)
	if !r.Items[0].ExpiresAt.Equal(wantExpire) {
		t.Fatalf("expire clamped: got %v, want %v", r.Items[0].ExpiresAt, wantExpire)
	}
}

func TestBatchReserveAtomic(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 3)

	// 第二张被其他订单持有 -> 整批失败，第一张不得留下预占。
	reserveOne(t, s, batchID, "order-X", codes[1], time.Minute)
	_, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "order-A", Codes: codes, TTL: time.Minute,
	})
	if !errors.Is(err, ErrVoucherBusy) {
		t.Fatalf("want ErrVoucherBusy, got %v", err)
	}
	v0, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	if v0.Status != StatusAvailable {
		t.Fatalf("first code leaked reservation on failed batch: %s", v0.Status)
	}

	// 全部可用时整批成功，多券各自带回版本。
	res, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "order-A", Codes: []string{codes[0], codes[2]}, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(res.Items) != 2 || res.Items[0].VoucherID == res.Items[1].VoucherID {
		t.Fatalf("bad reservation items: %+v", res.Items)
	}
	// 重复券码入参拒绝。
	if _, err := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "order-A", Codes: []string{codes[0], codes[0]}, TTL: time.Minute,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup codes in request: want ErrInvalidArgument, got %v", err)
	}
}

func TestConcurrentReserveOnlyOneWinner(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	const n = 32
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Reserve(context.Background(), ReserveInput{
				BatchID: batchID,
				OrderID: fmt.Sprintf("order-%02d", i),
				Codes:   []string{codes[0]},
				TTL:     time.Minute,
			})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	winners, busy := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrVoucherBusy):
			busy++
		default:
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if winners != 1 || busy != n-1 {
		t.Fatalf("winners=%d busy=%d, want exactly 1 winner", winners, busy)
	}
}

func TestConcurrentTerminalOpsSingleTerminalState(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)

	const n = 60
	var wg sync.WaitGroup
	start := make(chan struct{})
	var outcomesMu sync.Mutex
	var confirmed, voided, otherFail int

	// 一半协程先预占再确认（订单各不相同，争同一张券），另一半协程作废批次。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				orderID := fmt.Sprintf("order-%02d", i)
				r, err := s.Reserve(context.Background(), ReserveInput{
					BatchID: batchID, OrderID: orderID, Codes: []string{codes[0]}, TTL: time.Minute,
				})
				if err != nil {
					outcomesMu.Lock()
					otherFail++
					outcomesMu.Unlock()
					return
				}
				_, err = s.Confirm(context.Background(), batchID, orderID, codes[0], r.Items[0].Version)
				outcomesMu.Lock()
				if err == nil {
					confirmed++
				} else {
					otherFail++
				}
				outcomesMu.Unlock()
			} else {
				_, err := s.VoidBatch(context.Background(), batchID)
				outcomesMu.Lock()
				if err == nil {
					voided++
				} else {
					otherFail++
				}
				outcomesMu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 券只能落入一个终态；若确认成功则恰好一张核销记录且面值恰好一次。
	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	switch v.Status {
	case StatusRedeemed:
		if confirmed != 1 {
			t.Fatalf("redeemed but confirmed count = %d", confirmed)
		}
		// 批次可以在核销之后仍被作废（VoidedCount=0），关键是核销保留、
		// 面值只计一次，且作废不可能把已核销券改写成已作废。
		rec, err := s.GetRedemption(context.Background(), v.ID)
		if err != nil {
			t.Fatalf("redemption missing after void race: %v", err)
		}
		st, _ := s.GetOrder(context.Background(), rec.OrderID)
		if st.TotalFaceValue != 1000 || len(st.RedeemedVoucherIDs) != 1 {
			t.Fatalf("face value counted %d times: %+v", len(st.RedeemedVoucherIDs), st)
		}
	case StatusVoided:
		if voided != 1 {
			t.Fatalf("voided but VoidBatch success count = %d", voided)
		}
		if confirmed != 0 {
			t.Fatalf("status voided but confirmed = %d", confirmed)
		}
		if _, err := s.GetRedemption(context.Background(), v.ID); !errors.Is(err, ErrRedemptionNotFound) {
			t.Fatalf("voided voucher must have no redemption, err=%v", err)
		}
	default:
		t.Fatalf("voucher did not reach a terminal state: %s", v.Status)
	}
}

func TestConcurrentConfirmReleaseExpiry(t *testing.T) {
	// 同一订单：确认 / 释放 / 超时推进 同时作用于一张已预占的券，
	// 终态唯一：要么已核销（恰好一次入账），要么回到可用（绝无核销记录）。
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)
	r := reserveOne(t, s, batchID, "order-A", codes[0], 50*time.Millisecond)
	version := r.Items[0].Version

	const n = 45
	var wg sync.WaitGroup
	start := make(chan struct{})
	var confirms atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 3 {
			case 0:
				if _, err := s.Confirm(context.Background(), batchID, "order-A", codes[0], version); err == nil {
					confirms.Add(1)
				}
			case 1:
				_ = s.Release(context.Background(), batchID, "order-A", codes[0])
			case 2:
				clk.advance(time.Millisecond)
				_, _ = s.AdvanceExpiry(context.Background())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	v, _ := s.GetVoucher(context.Background(), batchID, codes[0])
	switch v.Status {
	case StatusRedeemed:
		if confirms.Load() != 1 {
			t.Fatalf("redeemed but successful confirms = %d", confirms.Load())
		}
	case StatusAvailable:
		if confirms.Load() != 0 {
			t.Fatalf("available but successful confirms = %d", confirms.Load())
		}
	default:
		t.Fatalf("unexpected status: %s", v.Status)
	}
}

func TestNoPlaintextLeak(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 1)
	secret := codes[0]
	reserveOne(t, s, batchID, "order-secret", secret, time.Minute)

	// 1) 错误信息不得包含明文（错误券码场景尤其重要）。
	_, err := s.GetVoucher(context.Background(), batchID, secret)
	_ = err
	_, errNotFound := s.Reserve(context.Background(), ReserveInput{
		BatchID: batchID, OrderID: "o", Codes: []string{"WRONG-" + secret}, TTL: time.Minute,
	})
	if errNotFound == nil {
		t.Fatal("expected error for wrong code")
	}
	if strings.Contains(errNotFound.Error(), secret) {
		t.Fatalf("error leaks plaintext: %v", errNotFound)
	}

	// 2) 任何对外视图都不得包含明文。
	bv, _ := s.GetBatch(context.Background(), batchID)
	if strings.Contains(fmt.Sprintf("%+v", bv), secret) {
		t.Fatal("batch view leaks plaintext")
	}
	list, _ := s.ListVouchers(context.Background(), batchID)
	if strings.Contains(fmt.Sprintf("%+v", list), secret) {
		t.Fatal("voucher list leaks plaintext")
	}
	st, _ := s.GetOrder(context.Background(), "order-secret")
	if strings.Contains(fmt.Sprintf("%+v", st), secret) {
		t.Fatal("order view leaks plaintext")
	}

	// 3) 内部对象本身不保存明文，只保存摘要。
	s.mu.RLock()
	var internal *voucher
	for _, v := range s.vouchers {
		if v.batchID == batchID {
			internal = v
		}
	}
	s.mu.RUnlock()
	if internal == nil {
		t.Fatal("voucher not found internally")
	}
	if strings.Contains(fmt.Sprintf("%+v", internal), secret) {
		t.Fatal("internal voucher retains plaintext")
	}
	if len(internal.codeDigest) != digestSize {
		t.Fatalf("digest size = %d, want %d", len(internal.codeDigest), digestSize)
	}
}

func TestGetOrderReflectsReservations(t *testing.T) {
	s, clk := newTestStore(t)
	batchID, codes := setupBatch(t, s, clk, 2)

	reserveOne(t, s, batchID, "order-A", codes[0], time.Minute)
	reserveOne(t, s, batchID, "order-A", codes[1], time.Minute)
	st, err := s.GetOrder(context.Background(), "order-A")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(st.ActiveReservations) != 2 || st.TotalFaceValue != 0 {
		t.Fatalf("bad order status: %+v", st)
	}

	// 到期推进后预占不再出现在订单视图。
	clk.advance(2 * time.Minute)
	if _, err := s.AdvanceExpiry(context.Background()); err != nil {
		t.Fatalf("AdvanceExpiry: %v", err)
	}
	if _, err := s.GetOrder(context.Background(), "order-A"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("expired-only order: want ErrOrderNotFound, got %v", err)
	}
}
