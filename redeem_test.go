package govoucherpool

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var redeemBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newRedeemFixture(t *testing.T, now *time.Time, face, minAmount, maxRefund, stock int64) (*Service, string, string) {
	t.Helper()
	s, err := NewService(func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBatch(CreateBatchInput{
		Name:        "redeem",
		FaceValue:   face,
		EffectiveAt: redeemBase,
		ExpiresAt:   redeemBase.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureRedeem(b.ID, RedeemRule{Version: 1, MinOrderAmount: minAmount, MaxRefundValue: maxRefund}, stock); err != nil {
		t.Fatal(err)
	}
	vid, err := s.RegisterVoucher(b.ID, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	return s, b.ID, vid
}

func registerCode(t *testing.T, s *Service, batchID, code string) string {
	t.Helper()
	id, err := s.RegisterVoucher(batchID, code)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRedeemIdempotencyAndConflict(t *testing.T) {
	now := redeemBase.Add(time.Minute)
	s, _, _ := newRedeemFixture(t, &now, 100, 50, 100, 10)

	req := RedeemRequest{RedeemNo: "r1", UserID: "u1", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 200}
	rec, err := s.Redeem(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Redeem(req)
	if err != nil {
		t.Fatal(err)
	}
	if *again != *rec {
		t.Fatalf("idempotent replay mismatch: %+v vs %+v", again, rec)
	}
	changed := req
	changed.OrderAmount = 300
	if _, err := s.Redeem(changed); !errors.Is(err, ErrRedeemConflict) {
		t.Fatalf("amount change: got %v", err)
	}
	for _, mutate := range []func(*RedeemRequest){
		func(r *RedeemRequest) { r.UserID = "u2" },
		func(r *RedeemRequest) { r.OrderID = "o2" },
		func(r *RedeemRequest) { r.Code = "code-2" },
		func(r *RedeemRequest) { r.RuleVersion = 2 },
	} {
		c := req
		mutate(&c)
		if _, err := s.Redeem(c); !errors.Is(err, ErrRedeemConflict) {
			t.Fatalf("conflict case: got %v", err)
		}
	}
}

func TestRedeemConcurrentSingleWinner(t *testing.T) {
	now := redeemBase.Add(time.Minute)
	s, _, vid := newRedeemFixture(t, &now, 100, 0, 0, 100)

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Redeem(RedeemRequest{
				RedeemNo:    "r-" + itoa(uint64(i)),
				UserID:      "u",
				OrderID:     "o-" + itoa(uint64(i)),
				Code:        "code-1",
				RuleVersion: 1,
				OrderAmount: 100,
			})
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrVoucherRedeemed) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins)
	}
	snap, err := s.GetVoucher(vid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != StateRedeemed {
		t.Fatalf("state = %v", snap.State)
	}
}

func TestRedeemStockAtomicOnFailure(t *testing.T) {
	now := redeemBase.Add(time.Minute)
	s, batchID, _ := newRedeemFixture(t, &now, 100, 50, 0, 1)
	v2 := registerCode(t, s, batchID, "code-2")

	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r1", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 10}); !errors.Is(err, ErrRuleMismatch) {
		t.Fatalf("rule mismatch: got %v", err)
	}
	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r2", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 9, OrderAmount: 100}); !errors.Is(err, ErrRuleMismatch) {
		t.Fatalf("version mismatch: got %v", err)
	}
	stock, _ := s.RedeemStock(batchID)
	if stock != 1 {
		t.Fatalf("stock after failed redeems = %d, want 1", stock)
	}

	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r3", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r4", UserID: "u", OrderID: "o2", Code: "code-2", RuleVersion: 1, OrderAmount: 100}); !errors.Is(err, ErrPoolStockExhausted) {
		t.Fatalf("stock exhausted: got %v", err)
	}
	snap, _ := s.GetVoucher(v2)
	if snap.State != StateAvailable {
		t.Fatalf("voucher2 state = %v, want available", snap.State)
	}
}

func TestRedeemExpiryBoundaryAndLateSweep(t *testing.T) {
	now := redeemBase.Add(time.Hour - time.Second)
	s, batchID, vid := newRedeemFixture(t, &now, 100, 0, 0, 10)

	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r1", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 100}); err != nil {
		t.Fatal(err)
	}
	now = redeemBase.Add(time.Hour)
	registerCode(t, s, batchID, "code-2")
	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r2", UserID: "u", OrderID: "o2", Code: "code-2", RuleVersion: 1, OrderAmount: 100}); !errors.Is(err, ErrBatchNotInEffect) {
		t.Fatalf("at expiry: got %v", err)
	}
	voided := s.SweepExpired()
	if len(voided) != 1 {
		t.Fatalf("swept %d vouchers, want 1", len(voided))
	}
	snap, _ := s.GetVoucher(vid)
	if snap.State != StateRedeemed || snap.RedeemOrder != "o1" {
		t.Fatalf("confirmed redemption revoked by late sweep: %+v", snap)
	}
}

func TestPartialRefundWithinRuleAllowance(t *testing.T) {
	now := redeemBase.Add(time.Minute)
	s, _, vid := newRedeemFixture(t, &now, 100, 0, 40, 10)

	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r1", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 100}); err != nil {
		t.Fatal(err)
	}
	fr, err := s.Refund("r1", "rf1", 30)
	if err != nil {
		t.Fatal(err)
	}
	if fr.RedeemNo != "r1" || fr.VoucherID != vid {
		t.Fatalf("refund not linked to original redeem: %+v", fr)
	}
	again, err := s.Refund("r1", "rf1", 30)
	if err != nil || *again != *fr {
		t.Fatalf("refund replay: %v %+v", err, again)
	}
	if _, err := s.Refund("r1", "rf1", 10); !errors.Is(err, ErrRefundConflict) {
		t.Fatalf("refund conflict: got %v", err)
	}
	if _, err := s.Refund("r1", "rf2", 20); !errors.Is(err, ErrRefundExceedsRule) {
		t.Fatalf("exceeds rule: got %v", err)
	}
	if _, err := s.Refund("r1", "rf3", 10); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetRedeem("r1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.RefundedValue != 40 || rec.RemainingRefundable() != 0 {
		t.Fatalf("refunded = %d remaining = %d", rec.RefundedValue, rec.RemainingRefundable())
	}
	snap, _ := s.GetVoucher(vid)
	if snap.State != StateRedeemed {
		t.Fatalf("voucher restored after partial refund: %v", snap.State)
	}
}

func TestRefundNotAllowedByRule(t *testing.T) {
	now := redeemBase.Add(time.Minute)
	s, _, _ := newRedeemFixture(t, &now, 100, 0, 0, 10)
	if _, err := s.Redeem(RedeemRequest{RedeemNo: "r1", UserID: "u", OrderID: "o1", Code: "code-1", RuleVersion: 1, OrderAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refund("r1", "rf1", 1); !errors.Is(err, ErrRefundExceedsRule) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Refund("missing", "rf2", 1); !errors.Is(err, ErrRedeemNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestConfigureRedeemVersionMonotonic(t *testing.T) {
	now := redeemBase
	s, batchID, _ := newRedeemFixture(t, &now, 100, 0, 0, 1)
	if err := s.ConfigureRedeem(batchID, RedeemRule{Version: 1}, 1); !errors.Is(err, ErrRuleVersionStale) {
		t.Fatalf("got %v", err)
	}
	if err := s.ConfigureRedeem(batchID, RedeemRule{Version: 2, MaxRefundValue: 101}, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
}
