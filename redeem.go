package govoucherpool

import "time"

// RedeemRule 是批次的兑换核销规则。规则版本随配置更新单调递增，
// 核销请求必须携带当前版本，过期版本的请求直接失败。
type RedeemRule struct {
	// Version 规则版本，必须为正且大于已配置版本。
	Version int64
	// MinOrderAmount 订单金额下限（最小货币单位），低于该值规则不匹配。
	MinOrderAmount int64
	// MaxRefundValue 单笔核销允许累计返还的权益上限（按面值计，
	// 不超过券面值）。0 表示该规则下不允许任何退款返还。
	MaxRefundValue int64
}

// RedeemRequest 是一笔兑换核销请求。核销号 RedeemNo 是幂等键：
// 同一核销号重复提交且载荷完全一致时返回原结果；
// 用户、券码、订单、规则版本或订单金额任一不同即构成幂等冲突。
type RedeemRequest struct {
	RedeemNo    string
	UserID      string
	OrderID     string
	Code        string
	RuleVersion int64
	OrderAmount int64
	// 规则匹配维度，与批次 Scope 对应；全空表示不声明维度。
	Categories []string
	Stores     []string
	SKUs       []string
}

// RedeemRecord 是一笔已确认的兑换核销结果，与券状态翻转、库存扣减
// 在同一临界区内写入。订单金额随记录固化，订单改价后旧请求无法复用。
type RedeemRecord struct {
	RedeemNo      string
	UserID        string
	OrderID       string
	VoucherID     string
	BatchID       string
	RuleVersion   int64
	OrderAmount   int64
	FaceValue     int64
	RefundedValue int64 // 已累计返还的权益
	At            time.Time

	maxRefund int64 // 核销时规则允许的累计返还上限
}

// RemainingRefundable 返回该笔核销按规则还可返还的权益额度。
func (r RedeemRecord) RemainingRefundable() int64 {
	return r.maxRefund - r.RefundedValue
}

// RefundRecord 是一条退款返还记录，始终关联原核销（RedeemNo）。
// 部分退款只冲减权益额度，不会把券恢复为可用。
type RefundRecord struct {
	RefundNo  string
	RedeemNo  string
	VoucherID string
	OrderID   string
	Value     int64
	At        time.Time
}

// redeemEntry 是核销号索引的内部条目，含完整载荷用于幂等比对。
type redeemEntry struct {
	record     RedeemRecord
	maxRefund  int64
	codeDigest string
}

// ConfigureRedeem 为批次配置兑换规则与可核销库存。
// 规则版本必须大于已配置版本；库存为剩余可核销笔数，必须非负。
func (s *Service) ConfigureRedeem(batchID string, rule RedeemRule, stock int64) error {
	if rule.Version <= 0 || rule.MinOrderAmount < 0 || rule.MaxRefundValue < 0 || stock < 0 {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return ErrBatchNotFound
	}
	if cur, ok := s.redeemRules[batchID]; ok && rule.Version <= cur.Version {
		return ErrRuleVersionStale
	}
	if rule.MaxRefundValue > b.FaceValue {
		return ErrInvalidArgument
	}
	s.redeemRules[batchID] = rule
	s.redeemStock[batchID] = stock
	return nil
}

// Redeem 执行一笔兑换核销。全部校验（券状态、批次窗口、规则版本、
// 订单金额、适用范围、池库存）通过后才在同一临界区内扣库存、翻转券
// 状态并写入核销记录；任一校验失败整笔失败，不会先扣库存再报错。
func (s *Service) Redeem(in RedeemRequest) (*RedeemRecord, error) {
	if in.RedeemNo == "" || in.UserID == "" || in.OrderID == "" || in.Code == "" || in.OrderAmount < 0 {
		return nil, ErrInvalidArgument
	}
	digest := s.Digest(in.Code)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 幂等：同核销号同载荷返回原结果；载荷任一关键字段不同即冲突。
	if e, ok := s.redeemIdx[in.RedeemNo]; ok {
		if e.record.UserID == in.UserID &&
			e.record.OrderID == in.OrderID &&
			e.codeDigest == digest &&
			e.record.RuleVersion == in.RuleVersion &&
			e.record.OrderAmount == in.OrderAmount {
			cp := e.record
			return &cp, nil
		}
		return nil, ErrRedeemConflict
	}

	now := s.now()
	v, err := s.lookupLocked(digest)
	if err != nil {
		return nil, err
	}
	switch v.state {
	case StateRedeemed:
		return nil, ErrVoucherRedeemed
	case StateVoided:
		return nil, ErrVoucherVoided
	case StateHeld:
		// 本订单持有的未到期预占可直接核销；其余占用一律拒绝。
		if v.heldBy != in.OrderID || !now.Before(v.expiresAt) {
			return nil, ErrVoucherHeld
		}
	}
	b := s.batches[v.BatchID]
	if b.Voided {
		return nil, ErrBatchVoided
	}
	if now.Before(b.EffectiveAt) || !now.Before(b.ExpiresAt) {
		return nil, ErrBatchNotInEffect
	}

	rule, ok := s.redeemRules[v.BatchID]
	if !ok || in.RuleVersion != rule.Version {
		return nil, ErrRuleMismatch
	}
	if in.OrderAmount < rule.MinOrderAmount {
		return nil, ErrRuleMismatch
	}
	if !v.scope.Matches(in.Categories, in.Stores, in.SKUs) {
		return nil, ErrRuleMismatch
	}
	if s.redeemStock[v.BatchID] <= 0 {
		return nil, ErrPoolStockExhausted
	}

	// 校验全部通过：同一临界区内扣库存、翻状态、写记录。
	s.redeemStock[v.BatchID]--
	v.state = StateRedeemed
	v.redeemedAt = now
	v.redeemOrder = in.OrderID
	v.heldBy = ""
	v.heldAt = time.Time{}
	v.expiresAt = time.Time{}

	rec := RedeemRecord{
		RedeemNo:    in.RedeemNo,
		UserID:      in.UserID,
		OrderID:     in.OrderID,
		VoucherID:   v.ID,
		BatchID:     v.BatchID,
		RuleVersion: in.RuleVersion,
		OrderAmount: in.OrderAmount,
		FaceValue:   v.face,
		At:          now,
	}
	s.redeemIdx[in.RedeemNo] = redeemEntry{record: rec, maxRefund: rule.MaxRefundValue, codeDigest: digest}
	if s.redeemByVoucher[v.ID] == nil {
		s.redeemByVoucher[v.ID] = make(map[string]struct{})
	}
	s.redeemByVoucher[v.ID][in.RedeemNo] = struct{}{}
	cp := rec
	return &cp, nil
}

// Refund 对已确认的核销做退款返还。refundNo 为退款幂等号：
// 重复提交返回原记录；同一退款号关联不同核销或金额即冲突。
// 返还额度受核销时规则上限约束，且只冲减权益，不把券恢复为可用。
func (s *Service) Refund(redeemNo, refundNo string, value int64) (*RefundRecord, error) {
	if redeemNo == "" || refundNo == "" || value <= 0 {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if fr, ok := s.refundIdx[refundNo]; ok {
		if fr.RedeemNo == redeemNo && fr.Value == value {
			cp := fr
			return &cp, nil
		}
		return nil, ErrRefundConflict
	}

	e, ok := s.redeemIdx[redeemNo]
	if !ok {
		return nil, ErrRedeemNotFound
	}
	if e.record.RefundedValue+value > e.maxRefund {
		return nil, ErrRefundExceedsRule
	}

	now := s.now()
	e.record.RefundedValue += value
	s.redeemIdx[redeemNo] = e
	fr := RefundRecord{
		RefundNo:  refundNo,
		RedeemNo:  redeemNo,
		VoucherID: e.record.VoucherID,
		OrderID:   e.record.OrderID,
		Value:     value,
		At:        now,
	}
	s.refundIdx[refundNo] = fr
	cp := fr
	return &cp, nil
}

// GetRedeem 按核销号查询核销记录副本。
func (s *Service) GetRedeem(redeemNo string) (*RedeemRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.redeemIdx[redeemNo]
	if !ok {
		return nil, ErrRedeemNotFound
	}
	rec := e.record
	rec.maxRefund = e.maxRefund
	return &rec, nil
}

// RedeemStock 返回批次剩余可核销库存。
func (s *Service) RedeemStock(batchID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.batches[batchID]; !ok {
		return 0, ErrBatchNotFound
	}
	return s.redeemStock[batchID], nil
}

// SweepExpired 过期扫描：把批次已失效但仍处于可用态的券置为作废。
// 已核销的券是终态，迟到的扫描不会撤销已确认订单；预占未到期的券
// 由预占自身的到期时间与版本栅栏保护，同样不受影响。
func (s *Service) SweepExpired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	voided := make([]string, 0)
	for _, v := range s.vouchers {
		if v.state != StateAvailable {
			continue
		}
		b := s.batches[v.BatchID]
		if now.Before(b.ExpiresAt) {
			continue
		}
		v.state = StateVoided
		v.voidedAt = now
		v.voidReason = "expired"
		voided = append(voided, v.ID)
	}
	return voided
}
