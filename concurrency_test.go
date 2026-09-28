package govoucherpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrent_ReserveOnlyOneWinner(t *testing.T) {
	svc, err := NewService(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateBatch(CreateBatchInput{
		Name: "c", FaceValue: 100,
		EffectiveAt: time.Now().Add(-time.Hour),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterVoucher(b.ID, "CONTESTED-CODE"); err != nil {
		t.Fatal(err)
	}

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := map[string]int{}
	var winnerVersion int64
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		orderID := fmt.Sprintf("order-%02d", i)
		go func() {
			defer wg.Done()
			<-start
			r, err := svc.Reserve(orderID, "CONTESTED-CODE", time.Hour)
			if err == nil {
				mu.Lock()
				successes[orderID]++
				winnerVersion = r.Version
				mu.Unlock()
			} else if !errors.Is(err, ErrVoucherHeld) {
				t.Errorf("unexpected reserve error for %s: %v", orderID, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(successes) != 1 {
		t.Fatalf("want exactly 1 winning order, got %d: %v", len(successes), successes)
	}
	var winner string
	for o := range successes {
		winner = o
	}
	if winnerVersion != 1 {
		t.Fatalf("winner version = %d, want 1", winnerVersion)
	}
	// 赢家重复预占必须拿到同一个预占（版本、到期时间不变）。
	r2, err := svc.Reserve(winner, "CONTESTED-CODE", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Version != 1 {
		t.Fatalf("repeat reserve version = %d, want 1", r2.Version)
	}
}

// TestConcurrent_ConfirmVsRelease 反复制造“确认 vs 主动释放”竞争，
// 每轮只允许一个终态，面值要么全额计入一次，要么完全不计。
func TestConcurrent_ConfirmVsRelease(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		svc, _ := NewService(nil)
		b, _ := svc.CreateBatch(CreateBatchInput{
			Name: "c", FaceValue: 300,
			EffectiveAt: time.Now().Add(-time.Minute),
			ExpiresAt:   time.Now().Add(time.Hour),
		})
		code := fmt.Sprintf("CODE-%d", iter)
		id, _ := svc.RegisterVoucher(b.ID, code)
		r, err := svc.Reserve("order-X", code, time.Hour)
		if err != nil {
			t.Fatal(err)
		}

		const workers = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		var confirmOK, releaseOK int
		var mu sync.Mutex
		for i := 0; i < workers; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if _, err := svc.Confirm("order-X", code, r.Version); err == nil {
					mu.Lock()
					confirmOK++
					mu.Unlock()
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if err := svc.Release("order-X", code, r.Version); err == nil {
					mu.Lock()
					releaseOK++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()

		snap, err := svc.GetVoucher(id)
		if err != nil {
			t.Fatal(err)
		}
		reds := svc.OrderRedemptions("order-X")
		switch snap.State {
		case StateRedeemed:
			if releaseOK != 0 {
				t.Fatalf("iter %d: redeemed but release reported success %d times", iter, releaseOK)
			}
			if len(reds) != 1 || reds[0].FaceValue != 300 {
				t.Fatalf("iter %d: want exactly 1 redemption of 300, got %+v", iter, reds)
			}
			if got := svc.OrderRedeemedValue("order-X"); got != 300 {
				t.Fatalf("iter %d: value = %d", iter, got)
			}
		case StateAvailable:
			if confirmOK != 0 {
				t.Fatalf("iter %d: released but confirm reported success %d times", iter, confirmOK)
			}
			if len(reds) != 0 || svc.OrderRedeemedValue("order-X") != 0 {
				t.Fatalf("iter %d: released voucher must not be counted", iter)
			}
		default:
			t.Fatalf("iter %d: unexpected terminal state %s", iter, snap.State)
		}
	}
}

// TestConcurrent_ConfirmIdempotentUnderDuplicates 大量重复并发确认：
// 可以有多个调用拿到同一条核销记录，但面值只能计入一次。
func TestConcurrent_ConfirmIdempotentUnderDuplicates(t *testing.T) {
	svc, _ := NewService(nil)
	b, _ := svc.CreateBatch(CreateBatchInput{
		Name: "c", FaceValue: 500,
		EffectiveAt: time.Now().Add(-time.Minute),
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	id, _ := svc.RegisterVoucher(b.ID, "DUP-CONFIRM")
	r, _ := svc.Reserve("order-D", "DUP-CONFIRM", time.Hour)

	const n = 128
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Confirm("order-D", "DUP-CONFIRM", r.Version); err != nil {
				t.Errorf("duplicate confirm should be idempotent, got %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := svc.OrderRedeemedValue("order-D"); got != 500 {
		t.Fatalf("value = %d, want 500", got)
	}
	if rs := svc.OrderRedemptions("order-D"); len(rs) != 1 || rs[0].VoucherID != id {
		t.Fatalf("redemptions = %+v", rs)
	}
}

// TestConcurrent_VoidVsConfirmAndReserve 作废与预占、确认并发：
// 结束后每张券只能处于已核销或已作废，且二者计数守恒、面值与核销集一致。
func TestConcurrent_VoidVsConfirmAndReserve(t *testing.T) {
	svc, _ := NewService(nil)
	b, _ := svc.CreateBatch(CreateBatchInput{
		Name: "c", FaceValue: 250,
		EffectiveAt: time.Now().Add(-time.Minute),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	})

	const heldN, availN = 40, 40
	var held []struct {
		code    string
		orderID string
		version int64
	}
	for i := 0; i < heldN; i++ {
		code := fmt.Sprintf("HELD-%03d", i)
		if _, err := svc.RegisterVoucher(b.ID, code); err != nil {
			t.Fatal(err)
		}
		orderID := fmt.Sprintf("held-order-%03d", i)
		r, err := svc.Reserve(orderID, code, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, struct {
			code    string
			orderID string
			version int64
		}{code, orderID, r.Version})
	}
	for i := 0; i < availN; i++ {
		code := fmt.Sprintf("AVAIL-%03d", i)
		if _, err := svc.RegisterVoucher(b.ID, code); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup

	// 一半被预占的券尝试确认，与作废竞争。
	for i := range held {
		h := held[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = svc.Confirm(h.orderID, h.code, h.version)
		}()
	}
	// 另一半流量尝试对可用券预占（结果可能成功，也可能撞上作废）。
	for i := 0; i < availN; i++ {
		code := fmt.Sprintf("AVAIL-%03d", i)
		orderID := fmt.Sprintf("rush-order-%03d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := svc.Reserve(orderID, code, time.Hour)
			if err != nil {
				return
			}
			// 抢到后立刻尝试确认，同样与作废竞争。
			_, _ = svc.Confirm(orderID, code, r.Version)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _ = svc.VoidBatch(b.ID, "竞争作废")
	}()

	close(start)
	wg.Wait()

	// 二次作废用于稳定终态（重复作废不会改动任何券）。
	if _, err := svc.VoidBatch(b.ID, "收尾作废"); err != nil {
		t.Fatal(err)
	}

	svc.mu.Lock()
	var nRedeemed, nVoided, nOther int
	for _, v := range svc.vouchers {
		switch v.state {
		case StateRedeemed:
			nRedeemed++
		case StateVoided:
			nVoided++
		default:
			nOther++
		}
	}
	svc.mu.Unlock()

	if nOther != 0 {
		t.Fatalf("found %d vouchers not in a terminal state", nOther)
	}
	if nRedeemed+nVoided != heldN+availN {
		t.Fatalf("counts not conserved: redeemed=%d voided=%d total=%d", nRedeemed, nVoided, heldN+availN)
	}

	// 面值合计必须与实际核销集合一致，且每张券只计一次。
	var ledgerValue int64
	ledgerCount := 0
	for _, o := range append(orderIDs(held), rushOrderIDs(availN)...) {
		rs := svc.OrderRedemptions(o)
		for _, r := range rs {
			if r.FaceValue != 250 {
				t.Fatalf("unexpected face value in ledger: %+v", r)
			}
			ledgerValue += r.FaceValue
			ledgerCount++
		}
	}
	if ledgerCount != nRedeemed {
		t.Fatalf("ledger count %d != redeemed vouchers %d", ledgerCount, nRedeemed)
	}
	if ledgerValue != int64(nRedeemed)*250 {
		t.Fatalf("ledger value %d != %d", ledgerValue, int64(nRedeemed)*250)
	}
}

func orderIDs(held []struct {
	code    string
	orderID string
	version int64
}) []string {
	out := make([]string, 0, len(held))
	for _, h := range held {
		out = append(out, h.orderID)
	}
	return out
}

func rushOrderIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("rush-order-%03d", i))
	}
	return out
}

// TestConcurrent_VersionFenceAcrossReleaseCycle 模拟真实结算链路：
// A 释放（或超时）后 B 成功重占，然后 A 用旧版本号“迟到确认”无论多早/多晚、
// 无论重复多少次，都必须被版本/归属栅栏拒绝，面值绝不计入 A。
func TestConcurrent_VersionFenceAcrossReleaseCycle(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		svc, _ := NewService(nil)
		b, _ := svc.CreateBatch(CreateBatchInput{
			Name: "c", FaceValue: 100,
			EffectiveAt: time.Now().Add(-time.Minute),
			ExpiresAt:   time.Now().Add(time.Hour),
		})
		code := fmt.Sprintf("FENCE-%d", iter)
		id, err := svc.RegisterVoucher(b.ID, code)
		if err != nil {
			t.Fatal(err)
		}
		rA, _ := svc.Reserve("order-A", code, time.Hour)
		if err := svc.Release("order-A", code, rA.Version); err != nil {
			t.Fatal(err)
		}
		rB, err := svc.Reserve("order-B", code, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if rB.Version <= rA.Version {
			t.Fatalf("iter %d: new hold version %d must exceed old %d", iter, rB.Version, rA.Version)
		}

		// B 确认 与 A 的旧版本迟到确认 并发。
		const dup = 32
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < dup; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if rec, err := svc.Confirm("order-A", code, rA.Version); err == nil {
					t.Errorf("iter %d: stale confirm succeeded: %+v", iter, rec)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Confirm("order-B", code, rB.Version); err != nil {
				t.Errorf("iter %d: current holder confirm failed: %v", iter, err)
			}
		}()
		close(start)
		wg.Wait()

		if val := svc.OrderRedeemedValue("order-A"); val != 0 {
			t.Fatalf("iter %d: stale order A must never accrue value, got %d", iter, val)
		}
		if val := svc.OrderRedeemedValue("order-B"); val != 100 {
			t.Fatalf("iter %d: order B value = %d, want 100", iter, val)
		}
		snap, _ := svc.GetVoucher(id)
		if snap.State != StateRedeemed || snap.RedeemOrder != "order-B" {
			t.Fatalf("iter %d: voucher must be redeemed by B: %+v", iter, snap)
		}
	}
}

// TestConcurrent_ConfirmVsReleaseSameVersion 同一持有方、同一版本下
// 确认与释放竞争：两者只允许一个生效，终态唯一且面值最多计入一次。
func TestConcurrent_ConfirmVsReleaseSameVersion(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		svc, _ := NewService(nil)
		b, _ := svc.CreateBatch(CreateBatchInput{
			Name: "c", FaceValue: 100,
			EffectiveAt: time.Now().Add(-time.Minute),
			ExpiresAt:   time.Now().Add(time.Hour),
		})
		code := fmt.Sprintf("RACE-%d", iter)
		id, _ := svc.RegisterVoucher(b.ID, code)
		r, _ := svc.Reserve("order-A", code, time.Hour)

		const dup = 16
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < dup; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, _ = svc.Confirm("order-A", code, r.Version)
			}()
			go func() {
				defer wg.Done()
				<-start
				_ = svc.Release("order-A", code, r.Version)
			}()
		}
		close(start)
		wg.Wait()

		snap, _ := svc.GetVoucher(id)
		switch snap.State {
		case StateRedeemed:
			if v := svc.OrderRedeemedValue("order-A"); v != 100 {
				t.Fatalf("iter %d: redeemed but value = %d", iter, v)
			}
		case StateAvailable:
			if v := svc.OrderRedeemedValue("order-A"); v != 0 {
				t.Fatalf("iter %d: released but value = %d", iter, v)
			}
		default:
			t.Fatalf("iter %d: unexpected state %s", iter, snap.State)
		}
	}
}
