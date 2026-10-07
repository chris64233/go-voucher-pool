package govoucherpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type redeemFixture struct {
	svc     *Service
	clock   *fakeClock
	batchID string
}

func newRedeemFixture(t *testing.T, stock int64, policy RefundPolicy) *redeemFixture {
	t.Helper()
	clock := newFakeClock()
	svc, err := NewService(clock.now)
	if err != nil {
		t.Fatal(err)
	}
	start := clock.now()
	b, err := svc.CreateBatch(CreateBatchInput{
		Name:           "满100减20",
		FaceValue:      2000,
		Scope:          Scope{SKUs: []string{"sku-1"}, Stores: []string{"store-1"}},
		EffectiveAt:    start.Add(-time.Hour),
		ExpiresAt:      start.Add(24 * time.Hour),
		RuleVersion:    7,
		MinOrderAmount: 10000,
		RefundPolicy:   policy,
		Stock:          stock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &redeemFixture{svc: svc, clock: clock, batchID: b.ID}
}

func (f *redeemFixture) register(t *testing.T, code string) string {
	t.Helper()
	id, err := f.svc.RegisterVoucher(f.batchID, code)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *redeemFixture) voucherID(t *testing.T, code string) string {
	t.Helper()
	id, err := f.svc.voucherIDByCodeForTest(code)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func baseRedeemReq(key, code string) RedeemRequest {
	return RedeemRequest{
		Key:         key,
		UserID:      "user-1",
		OrderID:     "order-1",
		Code:        code,
		RuleVersion: 7,
		OrderAmount: 12000,
		Stores:      []string{"store-1"},
		SKUs:        []string{"sku-1"},
	}
}

// voucherIDByCodeForTest 仅供测试通过券码定位券 ID。
func (s *Service) voucherIDByCodeForTest(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.digestIdx[s.Digest(code)]
	if !ok {
		return "", ErrCodeNotFound
	}
	return v.ID, nil
}

func TestRedeem_SuccessAndIdempotent(t *testing.T) {
	f := newRedeemFixture(t, 5, RefundPolicy{})
	id := f.register(t, "RC-1")

	rec1, err := f.svc.Redeem(baseRedeemReq("rk-1", "RC-1"))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if rec1.VoucherID != id || rec1.FaceValue != 2000 || rec1.RuleVersion != 7 {
		t.Fatalf("bad record: %+v", rec1)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 4 {
		t.Fatalf("remaining = %d, want 4", b.RemainingStock())
	}

	// 同核销号重复提交返回原结果，库存不重复扣减。
	rec2, err := f.svc.Redeem(baseRedeemReq("rk-1", "RC-1"))
	if err != nil {
		t.Fatalf("duplicate redeem: %v", err)
	}
	if rec2.At != rec1.At || rec2.VoucherID != rec1.VoucherID {
		t.Fatalf("not idempotent: %+v vs %+v", rec1, rec2)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 4 {
		t.Fatalf("duplicate redeem must not deduct stock: %d", b.RemainingStock())
	}

	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateRedeemed || snap.RedeemOrder != "order-1" {
		t.Fatalf("snapshot: %+v", snap)
	}
	if rs := f.svc.OrderRedeems("order-1"); len(rs) != 1 || rs[0].Key != "rk-1" {
		t.Fatalf("order redeems: %+v", rs)
	}
	if got, err := f.svc.GetRedeem("rk-1"); err != nil || got.FaceValue != 2000 {
		t.Fatalf("GetRedeem: %+v %v", got, err)
	}

	// 券已核销，用别的核销号再次核销必须失败。
	if _, err := f.svc.Redeem(baseRedeemReq("rk-2", "RC-1")); !errors.Is(err, ErrVoucherRedeemed) {
		t.Fatalf("redeem again: want ErrVoucherRedeemed, got %v", err)
	}
}

func TestRedeem_IdempotencyConflicts(t *testing.T) {
	f := newRedeemFixture(t, 10, RefundPolicy{})
	f.register(t, "RC-2")
	f.register(t, "RC-OTHER")

	if _, err := f.svc.Redeem(baseRedeemReq("rk-x", "RC-2")); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(RedeemRequest) RedeemRequest{
		"different user": func(r RedeemRequest) RedeemRequest {
			r.UserID = "user-2"
			return r
		},
		"different order": func(r RedeemRequest) RedeemRequest {
			r.OrderID = "order-2"
			return r
		},
		"different code": func(r RedeemRequest) RedeemRequest {
			r.Code = "RC-OTHER"
			return r
		},
		"different rule version": func(r RedeemRequest) RedeemRequest {
			r.RuleVersion = 8
			return r
		},
		"order amount changed": func(r RedeemRequest) RedeemRequest {
			// 订单金额变化：旧请求不能继续使用。
			r.OrderAmount = 13000
			return r
		},
	}
	for name, mutate := range cases {
		req := mutate(baseRedeemReq("rk-x", "RC-2"))
		if _, err := f.svc.Redeem(req); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("%s: want ErrIdempotencyConflict, got %v", name, err)
		}
	}

	// 冲突不影响原结果，库存只扣过一次；冲突请求里的另一张券仍可用。
	rec, err := f.svc.GetRedeem("rk-x")
	if err != nil || rec.OrderAmount != 12000 {
		t.Fatalf("original result altered: %+v %v", rec, err)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 9 {
		t.Fatalf("stock = %d, want 9", b.RemainingStock())
	}
	if snap, _ := f.svc.GetVoucher(f.voucherID(t, "RC-OTHER")); snap.State != StateAvailable {
		t.Fatalf("conflicting code must stay available: %s", snap.State)
	}
}

func TestRedeem_RuleAndStockAtomicity(t *testing.T) {
	t.Run("rule version mismatch", func(t *testing.T) {
		f := newRedeemFixture(t, 10, RefundPolicy{})
		f.register(t, "RC-V")
		req := baseRedeemReq("rk-v", "RC-V")
		req.RuleVersion = 6
		if _, err := f.svc.Redeem(req); !errors.Is(err, ErrRuleVersionMismatch) {
			t.Fatalf("want ErrRuleVersionMismatch, got %v", err)
		}
		if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 10 {
			t.Fatalf("stock changed on failure: %d", b.RemainingStock())
		}
	})

	t.Run("below min amount", func(t *testing.T) {
		f := newRedeemFixture(t, 10, RefundPolicy{})
		f.register(t, "RC-A")
		req := baseRedeemReq("rk-a", "RC-A")
		req.OrderAmount = 9999
		if _, err := f.svc.Redeem(req); !errors.Is(err, ErrRuleMismatch) {
			t.Fatalf("want ErrRuleMismatch, got %v", err)
		}
	})

	t.Run("scope mismatch", func(t *testing.T) {
		f := newRedeemFixture(t, 10, RefundPolicy{})
		f.register(t, "RC-S")
		req := baseRedeemReq("rk-s", "RC-S")
		req.SKUs = []string{"sku-9"}
		if _, err := f.svc.Redeem(req); !errors.Is(err, ErrRuleMismatch) {
			t.Fatalf("want ErrRuleMismatch, got %v", err)
		}
	})

	t.Run("pool exhausted", func(t *testing.T) {
		f := newRedeemFixture(t, 1, RefundPolicy{})
		f.register(t, "RC-E1")
		f.register(t, "RC-E2")
		if _, err := f.svc.Redeem(baseRedeemReq("rk-e1", "RC-E1")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Redeem(baseRedeemReq("rk-e2", "RC-E2")); !errors.Is(err, ErrPoolExhausted) {
			t.Fatalf("want ErrPoolExhausted, got %v", err)
		}
		if snap, _ := f.svc.GetVoucher(f.voucherID(t, "RC-E2")); snap.State != StateAvailable {
			t.Fatalf("voucher must stay available: %s", snap.State)
		}
		if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 0 {
			t.Fatalf("stock = %d, want 0", b.RemainingStock())
		}
	})

	t.Run("unlimited stock", func(t *testing.T) {
		f := newRedeemFixture(t, 0, RefundPolicy{})
		f.register(t, "RC-U")
		if _, err := f.svc.Redeem(baseRedeemReq("rk-u", "RC-U")); err != nil {
			t.Fatal(err)
		}
		if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != -1 {
			t.Fatalf("unlimited stock indicator = %d", b.RemainingStock())
		}
	})
}

func TestRedeem_ExpiryBoundary(t *testing.T) {
	f := newRedeemFixture(t, 10, RefundPolicy{})
	f.register(t, "RC-B")
	batch, _ := f.svc.GetBatch(f.batchID)

	// 失效前 1ns：核销成功。
	f.clock.t = batch.ExpiresAt.Add(-time.Nanosecond)
	f.register(t, "RC-B2")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-b2", "RC-B2")); err != nil {
		t.Fatalf("redeem 1ns before expiry should succeed: %v", err)
	}

	// 失效时刻（不含边界）：核销失败且状态不变。
	f.register(t, "RC-B3")
	f.clock.t = batch.ExpiresAt
	if _, err := f.svc.Redeem(baseRedeemReq("rk-b3", "RC-B3")); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("redeem at expiry: want ErrBatchNotInEffect, got %v", err)
	}
	if snap, _ := f.svc.GetVoucher(f.voucherID(t, "RC-B3")); snap.State != StateAvailable {
		t.Fatalf("expired redeem must not flip state: %s", snap.State)
	}
}

func TestRedeem_LateExpireScanCannotUndoConfirmed(t *testing.T) {
	f := newRedeemFixture(t, 10, RefundPolicy{})
	id := f.register(t, "RC-L")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-l", "RC-L")); err != nil {
		t.Fatal(err)
	}
	// 时钟大幅推进后再跑过期扫描和批次作废：已确认订单绝不能被撤销。
	f.clock.advance(48 * time.Hour)
	if reclaimed := f.svc.ExpireHolds(); len(reclaimed) != 0 {
		t.Fatalf("redeemed voucher must never be reclaimed: %v", reclaimed)
	}
	if _, err := f.svc.VoidBatch(f.batchID, "迟到作废"); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateRedeemed || snap.RedeemOrder != "order-1" {
		t.Fatalf("confirmed redemption must survive: %+v", snap)
	}
}

func TestRedeem_HeldVoucherContention(t *testing.T) {
	f := newRedeemFixture(t, 10, RefundPolicy{})
	id := f.register(t, "RC-H")
	// 旧预占链路有效持有该券：直接核销应被拒绝。
	if _, err := f.svc.Reserve("order-hold", "RC-H", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(baseRedeemReq("rk-h", "RC-H")); !errors.Is(err, ErrVoucherHeld) {
		t.Fatalf("want ErrVoucherHeld, got %v", err)
	}
	// 预占到期后，Redeem 当场回收并核销成功（不依赖扫描时序）。
	f.clock.advance(6 * time.Minute)
	if _, err := f.svc.Redeem(baseRedeemReq("rk-h", "RC-H")); err != nil {
		t.Fatalf("redeem after hold expiry: %v", err)
	}
	if snap, _ := f.svc.GetVoucher(id); snap.State != StateRedeemed {
		t.Fatalf("state = %s", snap.State)
	}
}

func TestConcurrent_RedeemOnlyOneResult(t *testing.T) {
	svc, _ := NewService(nil)
	now := time.Now()
	b, _ := svc.CreateBatch(CreateBatchInput{
		Name: "c", FaceValue: 100,
		EffectiveAt: now.Add(-time.Hour),
		ExpiresAt:   now.Add(24 * time.Hour),
		RuleVersion: 1,
		Stock:       1,
	})
	if _, err := svc.RegisterVoucher(b.ID, "RACE-CODE"); err != nil {
		t.Fatal(err)
	}

	const n = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	successKeys := map[string]struct{}{}
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			req := RedeemRequest{
				Key:         fmt.Sprintf("rk-%02d", i),
				UserID:      fmt.Sprintf("user-%02d", i),
				OrderID:     fmt.Sprintf("order-%02d", i),
				Code:        "RACE-CODE",
				RuleVersion: 1,
				OrderAmount: 1000,
			}
			if rec, err := svc.Redeem(req); err == nil {
				mu.Lock()
				successKeys[rec.Key] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(successKeys) != 1 {
		t.Fatalf("want exactly 1 successful redeem, got %d: %v", len(successKeys), successKeys)
	}
	if batch, _ := svc.GetBatch(b.ID); batch.RemainingStock() != 0 {
		t.Fatalf("stock = %d, want 0", batch.RemainingStock())
	}
	id, _ := svc.voucherIDByCodeForTest("RACE-CODE")
	if snap, _ := svc.GetVoucher(id); snap.State != StateRedeemed {
		t.Fatalf("state = %s", snap.State)
	}
}

// TestConcurrent_RedeemSameKeyDuplicates 同一核销号大量并发重复提交：
// 全部拿到同一条原结果，库存与面值只产生一次有效使用结果。
func TestConcurrent_RedeemSameKeyDuplicates(t *testing.T) {
	svc, _ := NewService(nil)
	now := time.Now()
	b, _ := svc.CreateBatch(CreateBatchInput{
		Name: "c", FaceValue: 100,
		EffectiveAt: now.Add(-time.Hour),
		ExpiresAt:   now.Add(24 * time.Hour),
		Stock:       5,
	})
	if _, err := svc.RegisterVoucher(b.ID, "DUP-RACE"); err != nil {
		t.Fatal(err)
	}

	const n = 128
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			req := RedeemRequest{
				Key: "same-key", UserID: "u", OrderID: "o",
				Code: "DUP-RACE", RuleVersion: 1, OrderAmount: 1000,
			}
			rec, err := svc.Redeem(req)
			if err != nil {
				t.Errorf("duplicate redeem failed: %v", err)
				return
			}
			if rec.Key != "same-key" || rec.FaceValue != 100 {
				t.Errorf("bad record: %+v", rec)
			}
		}()
	}
	close(start)
	wg.Wait()

	if batch, _ := svc.GetBatch(b.ID); batch.RemainingStock() != 4 {
		t.Fatalf("stock = %d, want 4", batch.RemainingStock())
	}
	if rs := svc.OrderRedeems("o"); len(rs) != 1 {
		t.Fatalf("redeems = %+v", rs)
	}
}

// TestConcurrent_RedeemVsExpireScan 并发核销与过期扫描：
// 已确认的核销永远不会被迟到扫描撤销。
func TestConcurrent_RedeemVsExpireScan(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		clock := newFakeClock()
		svc, _ := NewService(clock.now)
		start := clock.now()
		b, _ := svc.CreateBatch(CreateBatchInput{
			Name: "c", FaceValue: 100,
			EffectiveAt: start.Add(-time.Hour),
			ExpiresAt:   start.Add(24 * time.Hour),
		})
		code := fmt.Sprintf("EX-%d", iter)
		id, _ := svc.RegisterVoucher(b.ID, code)

		var wg sync.WaitGroup
		begin := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-begin
			_, _ = svc.Redeem(RedeemRequest{
				Key: "k", UserID: "u", OrderID: "o",
				Code: code, RuleVersion: 1, OrderAmount: 1000,
			})
		}()
		go func() {
			defer wg.Done()
			<-begin
			// 与核销并发反复扫描；不越过批次失效点，
			// 验证已确认核销不会被迟到的过期任务撤销。
			for j := 0; j < 100; j++ {
				_ = svc.ExpireHolds()
			}
		}()
		close(begin)
		wg.Wait()

		snap, _ := svc.GetVoucher(id)
		if snap.State != StateRedeemed {
			t.Fatalf("iter %d: voucher must be redeemed, got %s", iter, snap.State)
		}
		if rs := svc.OrderRedeems("o"); len(rs) != 1 || rs[0].FaceValue != 100 {
			t.Fatalf("iter %d: redeems = %+v", iter, rs)
		}
	}
}

func TestRefund_PartialReturnsBenefitOnly(t *testing.T) {
	// 允许部分返还；全额退款不恢复整券。
	policy := RefundPolicy{AllowPartial: true, RestoreOnFullRefund: false}
	f := newRedeemFixture(t, 3, policy)
	id := f.register(t, "RF-1")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-r1", "RF-1")); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 2 {
		t.Fatalf("stock after redeem = %d", b.RemainingStock())
	}

	// 退一半金额（6000/12000）：返还一半权益 1000，券仍为 redeemed。
	rec, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-1", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 6000,
	})
	if err != nil {
		t.Fatalf("partial refund: %v", err)
	}
	if rec.ReturnedBenefit != 1000 || rec.Restored || rec.FullRefund {
		t.Fatalf("bad refund record: %+v", rec)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateRedeemed {
		t.Fatalf("partial refund must not restore voucher: %s", snap.State)
	}
	if snap.ReturnedBenefit != 1000 || snap.RefundedAmount != 6000 {
		t.Fatalf("snapshot counters: %+v", snap)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 2 {
		t.Fatalf("partial refund must not restore stock: %d", b.RemainingStock())
	}
	// 返还记录关联原核销号。
	rs := f.svc.RefundsByRedeem("rk-r1")
	if len(rs) != 1 || rs[0].RefundID != "rf-1" || rs[0].VoucherID != id {
		t.Fatalf("refunds = %+v", rs)
	}

	// 退款幂等：同退款单号返回原结果。
	rec2, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-1", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 6000,
	})
	if err != nil {
		t.Fatalf("idempotent refund: %v", err)
	}
	if rec2.At != rec.At || rec2.ReturnedBenefit != 1000 {
		t.Fatalf("not idempotent: %+v vs %+v", rec, rec2)
	}
	if rs := f.svc.RefundsByRedeem("rk-r1"); len(rs) != 1 {
		t.Fatalf("duplicate refund appended a record: %+v", rs)
	}

	// 同退款单号但金额不同：幂等冲突。
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-1", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 100,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}

	// 再退 6001：累计超过原订单金额，拒绝。
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-2", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 6001,
	}); !errors.Is(err, ErrRefundExceeded) {
		t.Fatalf("want ErrRefundExceeded, got %v", err)
	}

	// 再退 6000 全额（full=false）：累计恰好到顶，权益再返 1000。
	rec3, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-3", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 6000,
	})
	if err != nil {
		t.Fatalf("second partial refund: %v", err)
	}
	if rec3.ReturnedBenefit != 1000 {
		t.Fatalf("benefit = %d, want 1000", rec3.ReturnedBenefit)
	}
	// 此后任何退款都超额。
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-4", OrderID: "order-1", RedeemKey: "rk-r1",
		RefundAmount: 1,
	}); !errors.Is(err, ErrRefundExceeded) {
		t.Fatalf("over-refund: want ErrRefundExceeded, got %v", err)
	}
}

func TestRefund_PartialNotAllowedByRule(t *testing.T) {
	f := newRedeemFixture(t, 3, RefundPolicy{}) // 两种返还都不允许
	f.register(t, "RF-2")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-r2", "RF-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-x", OrderID: "order-1", RedeemKey: "rk-r2",
		RefundAmount: 100,
	}); !errors.Is(err, ErrPartialRefundNotAllowed) {
		t.Fatalf("want ErrPartialRefundNotAllowed, got %v", err)
	}
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-y", OrderID: "order-1", RedeemKey: "rk-r2",
		RefundAmount: 12000, Full: true,
	}); !errors.Is(err, ErrPartialRefundNotAllowed) {
		t.Fatalf("full refund without restore policy: want ErrPartialRefundNotAllowed, got %v", err)
	}
}

func TestRefund_FullRefundRestoresVoucherAndStock(t *testing.T) {
	policy := RefundPolicy{AllowPartial: true, RestoreOnFullRefund: true}
	f := newRedeemFixture(t, 2, policy)
	id := f.register(t, "RF-3")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-r3", "RF-3")); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 1 {
		t.Fatalf("stock = %d, want 1", b.RemainingStock())
	}

	rec, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-full", OrderID: "order-1", RedeemKey: "rk-r3",
		RefundAmount: 12000, Full: true,
	})
	if err != nil {
		t.Fatalf("full refund: %v", err)
	}
	if !rec.Restored || rec.ReturnedBenefit != 2000 {
		t.Fatalf("bad record: %+v", rec)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateAvailable || snap.RedeemOrder != "" || !snap.FullyRefunded {
		t.Fatalf("voucher should be reusable and marked fully refunded: %+v", snap)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 2 {
		t.Fatalf("stock should be restored: %d", b.RemainingStock())
	}
	if rs := f.svc.OrderRedeems("order-1"); len(rs) != 0 {
		t.Fatalf("restored voucher must leave order ledger: %+v", rs)
	}

	// 恢复后的券可被另一订单再次核销。
	req2 := baseRedeemReq("rk-r3-again", "RF-3")
	req2.UserID = "user-2"
	req2.OrderID = "order-2"
	if _, err := f.svc.Redeem(req2); err != nil {
		t.Fatalf("redeem restored voucher: %v", err)
	}
	if snap, _ := f.svc.GetVoucher(id); snap.State != StateRedeemed || snap.RedeemOrder != "order-2" {
		t.Fatalf("voucher should be redeemed again: %+v", snap)
	}

	// 对原核销的迟到/重复全额退款不能再次回补库存或产生第二个有效结果。
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rf-full-2", OrderID: "order-1", RedeemKey: "rk-r3",
		RefundAmount: 12000, Full: true,
	}); err == nil {
		t.Fatal("late duplicate full refund must fail")
	}
}

func TestRefund_FullRefundIdempotentAndValidation(t *testing.T) {
	policy := RefundPolicy{RestoreOnFullRefund: true}
	f := newRedeemFixture(t, 2, policy)
	f.register(t, "RF-4")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-r4", "RF-4")); err != nil {
		t.Fatal(err)
	}

	fullReq := func(id string, amount int64) RefundRequest {
		return RefundRequest{
			RefundID: id, OrderID: "order-1", RedeemKey: "rk-r4",
			RefundAmount: amount, Full: true,
		}
	}
	r1, err := f.svc.Refund(fullReq("rf-f4", 12000))
	if err != nil {
		t.Fatal(err)
	}
	// 同退款单号重复（即使券已再次流转）也返回原结果。
	r2, err := f.svc.Refund(fullReq("rf-f4", 12000))
	if err != nil {
		t.Fatalf("idempotent full refund: %v", err)
	}
	if r2.At != r1.At || !r2.Restored {
		t.Fatalf("not idempotent: %+v vs %+v", r1, r2)
	}

	// 退款必须关联存在的原核销。
	missing := fullReq("rf-missing", 1)
	missing.RedeemKey = "rk-does-not-exist"
	if _, err := f.svc.Refund(missing); !errors.Is(err, ErrRedeemNotFound) {
		t.Fatalf("want ErrRedeemNotFound, got %v", err)
	}
	// 订单与原核销不一致：幂等冲突。
	bad := fullReq("rf-order", 1)
	bad.OrderID = "order-other"
	if _, err := f.svc.Refund(bad); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
	// 参数校验。
	if _, err := f.svc.Refund(RefundRequest{RefundID: "x"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if _, err := f.svc.Refund(fullReq("rf-zero", 0)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero amount: want ErrInvalidArgument, got %v", err)
	}
}

func TestRefund_PartialThenFullRestore(t *testing.T) {
	policy := RefundPolicy{AllowPartial: true, RestoreOnFullRefund: true}
	f := newRedeemFixture(t, 2, policy)
	id := f.register(t, "RF-5")
	if _, err := f.svc.Redeem(baseRedeemReq("rk-r5", "RF-5")); err != nil {
		t.Fatal(err)
	}
	// 先部分退款（权益 500），再全额退款恢复整券。
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rp-1", OrderID: "order-1", RedeemKey: "rk-r5",
		RefundAmount: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(RefundRequest{
		RefundID: "rp-2", OrderID: "order-1", RedeemKey: "rk-r5",
		RefundAmount: 9000, Full: true,
	}); err != nil {
		t.Fatalf("full refund after partial: %v", err)
	}
	snap, _ := f.svc.GetVoucher(id)
	if snap.State != StateAvailable {
		t.Fatalf("state = %s, want available", snap.State)
	}
	if b, _ := f.svc.GetBatch(f.batchID); b.RemainingStock() != 2 {
		t.Fatalf("stock = %d, want 2", b.RemainingStock())
	}
}
