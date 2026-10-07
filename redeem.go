package govoucherpool

import "time"

// redeemFingerprint 记录某个核销号首次提交时的请求要素，用于识别
// 「同核销号但券码/用户/订单/规则版本/订单金额不同」的幂等冲突。
// 券码只以摘要形式保存，明文不落库。
type redeemFingerprint struct {
	codeDigest  string
	userID      string
	orderID     string
	ruleVersion int32
	orderAmount int64
}

// Redeem 执行兑换核销：固定券码、用户、订单与优惠规则版本，
// 原子完成「规则/有效期/库存校验 → 券翻转为已核销 → 扣减池库存 → 写核销记录」。
//
// 语义：
//   - 同一核销号重复提交且所有要素一致：幂等返回原结果。
//   - 同一核销号但用户、券码、订单、规则版本或订单金额不同：ErrIdempotencyConflict，
//     订单金额变化后的旧请求不能继续使用。
//   - 规则版本与批次当前版本不一致：ErrRuleVersionMismatch。
//   - 订单不满足适用范围或最低金额：ErrRuleMismatch。
//   - 池库存不足：ErrPoolExhausted。
//   - 任何校验失败都整笔失败，不会先扣库存、不会改变券状态（先验后改）。
//   - 并发核销、ExpireHolds 超时扫描与退款返还共用同一把写锁；
//     redeemed 为终态，迟到的过期/作废任务不能撤销已确认订单。
func (s *Service) Redeem(req RedeemRequest) (*RedeemRecord, error) {
	if req.Key == "" || req.UserID == "" || req.OrderID == "" || req.Code == "" {
		return nil, ErrInvalidArgument
	}
	if req.OrderAmount < 0 {
		return nil, ErrInvalidArgument
	}
	ruleVersion := req.RuleVersion
	if ruleVersion <= 0 {
		ruleVersion = 1
	}
	digest := s.Digest(req.Code)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// 1) 幂等：核销号已存在时，要么原样返回，要么判冲突。绝不重复扣库存/记账。
	if fp, seen := s.redeemFingerprints[req.Key]; seen {
		if fp != (redeemFingerprint{digest, req.UserID, req.OrderID, ruleVersion, req.OrderAmount}) {
			return nil, ErrIdempotencyConflict
		}
		rec := s.redeemByKey[req.Key]
		return &rec, nil
	}

	// 2) 定位券与批次。
	v, err := s.lookupLocked(digest)
	if err != nil {
		return nil, err
	}
	b := s.batches[v.BatchID]

	// 3) 终态与批次状态检查。
	switch v.state {
	case StateRedeemed:
		return nil, ErrVoucherRedeemed
	case StateVoided:
		return nil, ErrVoucherVoided
	}
	if b.Voided {
		return nil, ErrBatchVoided
	}
	// 处于 held（预占中）的券不能走直接核销链路，避免与预占订单争用。
	if v.state == StateHeld {
		if !now.Before(v.expiresAt) {
			// 预占已到期但尚未被扫描回收：当场回收，继续核销。
			s.clearHoldLocked(v)
		} else {
			return nil, ErrVoucherHeld
		}
	}

	// 4) 有效期（含批次失效边界：失效时刻及之后不允许核销）。
	if now.Before(b.EffectiveAt) || !now.Before(b.ExpiresAt) {
		return nil, ErrBatchNotInEffect
	}

	// 5) 规则版本与规则匹配。
	if b.RuleVersion != ruleVersion {
		return nil, ErrRuleVersionMismatch
	}
	if req.OrderAmount < b.MinOrderAmount {
		return nil, ErrRuleMismatch
	}
	if !b.Scope.Matches(req.Categories, req.Stores, req.SKUs) {
		return nil, ErrRuleMismatch
	}

	// 6) 池库存（限量批次）。所有校验通过后才读取并扣减，保证不会先扣再报错。
	if b.Stock > 0 && b.remaining <= 0 {
		return nil, ErrPoolExhausted
	}

	// 7) 全部校验通过，临界区内一次性落账。
	rec := RedeemRecord{
		Key:         req.Key,
		VoucherID:   v.ID,
		BatchID:     v.BatchID,
		UserID:      req.UserID,
		OrderID:     req.OrderID,
		FaceValue:   v.face,
		RuleVersion: ruleVersion,
		OrderAmount: req.OrderAmount,
		At:          now,
	}
	v.state = StateRedeemed
	v.redeemedAt = now
	v.redeemOrder = req.OrderID
	v.redeemUser = req.UserID
	v.redeemRuleVersion = ruleVersion
	v.redeemOrderAmount = req.OrderAmount
	if b.Stock > 0 {
		b.remaining--
	}
	s.redeemFingerprints[req.Key] = redeemFingerprint{
		codeDigest:  digest,
		userID:      req.UserID,
		orderID:     req.OrderID,
		ruleVersion: ruleVersion,
		orderAmount: req.OrderAmount,
	}
	s.redeemByKey[req.Key] = rec
	if s.redeemByOrder[req.OrderID] == nil {
		s.redeemByOrder[req.OrderID] = make(map[string]RedeemRecord)
	}
	s.redeemByOrder[req.OrderID][v.ID] = rec
	return rec.clone(), nil
}

// Refund 处理退款后的权益返还，必须关联原核销号。
//
// 语义：
//   - 同一退款单号重复提交：要素一致幂等返回原结果；要素不同返回幂等冲突。
//   - 累计退款金额不得超过原订单金额，否则 ErrRefundExceeded。
//   - 部分退款：规则允许时按「退款金额 / 原订单金额」比例返还权益，
//     只写返还记录，不恢复券码、不回补库存；规则不允许返回 ErrPartialRefundNotAllowed。
//   - 全额退款且规则 RestoreOnFullRefund：整券恢复为 available、回补池库存，
//     券可被再次核销；一张券最多恢复一次，迟到/重复的全额返还不能重复回补。
//   - 全额退款但规则不恢复：券保持 redeemed 终态，仅按部分退款规则返还权益。
//   - 券因作废等原因已不在 redeemed 时，拒绝返还，保证库存与用户权益一致。
func (s *Service) Refund(req RefundRequest) (*RefundRecord, error) {
	if req.RefundID == "" || req.OrderID == "" || req.RedeemKey == "" {
		return nil, ErrInvalidArgument
	}
	if req.RefundAmount <= 0 {
		return nil, ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// 1) 退款幂等。
	if fp, seen := s.refundFingerprints[req.RefundID]; seen {
		want := refundFingerprint{req.OrderID, req.RedeemKey, req.RefundAmount, req.Full}
		if fp != want {
			return nil, ErrIdempotencyConflict
		}
		rec := s.refundByID[req.RefundID]
		return &rec, nil
	}

	// 2) 关联原核销，且订单必须一致。
	orig, ok := s.redeemByKey[req.RedeemKey]
	if !ok {
		return nil, ErrRedeemNotFound
	}
	if orig.OrderID != req.OrderID {
		return nil, ErrIdempotencyConflict
	}
	v := s.vouchers[orig.VoucherID]
	b := s.batches[orig.BatchID]

	// 3) 累计退款金额守恒。
	var refundedSoFar int64
	for _, r := range s.refundsByRedeem[req.RedeemKey] {
		refundedSoFar += r.RefundAmount
	}
	if orig.OrderAmount > 0 && refundedSoFar+req.RefundAmount > orig.OrderAmount {
		return nil, ErrRefundExceeded
	}

	rec := RefundRecord{
		RefundID:     req.RefundID,
		RedeemKey:    req.RedeemKey,
		VoucherID:    orig.VoucherID,
		BatchID:      orig.BatchID,
		OrderID:      req.OrderID,
		RefundAmount: req.RefundAmount,
		FullRefund:   req.Full,
		At:           now,
	}

	switch {
	case req.Full:
		// 整券恢复只允许一次：已经全额退过则按幂等之外的重复请求拒绝。
		if v.fullyRefunded {
			return nil, ErrRefundExceeded
		}
		if b.RefundPolicy.RestoreOnFullRefund {
			// 只有仍处于本次核销的 redeemed 终态才能恢复，
			// 避免迟到任务撤销已变化的状态。
			if v.state != StateRedeemed || v.redeemOrder != req.OrderID {
				return nil, ErrVoucherRedeemed
			}
			rec.ReturnedBenefit = orig.FaceValue
			rec.Restored = true
			v.state = StateAvailable
			v.redeemedAt = time.Time{}
			v.redeemOrder = ""
			v.redeemUser = ""
			v.redeemRuleVersion = 0
			v.redeemOrderAmount = 0
			v.fullyRefunded = true
			if b.Stock > 0 {
				b.remaining++
			}
			// 从订单核销账本移除该券，订单权益随之一致回退。
			s.removeRedeemLocked(orig)
		} else {
			// 规则不恢复整券：仅在允许部分返还时返还比例/全额权益，券保持终态。
			if !b.RefundPolicy.AllowPartial {
				return nil, ErrPartialRefundNotAllowed
			}
			rec.ReturnedBenefit = proratedBenefit(orig.FaceValue, req.RefundAmount, orig.OrderAmount)
			v.fullyRefunded = true
		}
	default:
		if !b.RefundPolicy.AllowPartial {
			return nil, ErrPartialRefundNotAllowed
		}
		if v.fullyRefunded {
			return nil, ErrRefundExceeded
		}
		rec.ReturnedBenefit = proratedBenefit(orig.FaceValue, req.RefundAmount, orig.OrderAmount)
	}

	v.refundedAmount += req.RefundAmount
	v.returnedBenefit += rec.ReturnedBenefit

	s.refundFingerprints[req.RefundID] = refundFingerprint{
		orderID:      req.OrderID,
		redeemKey:    req.RedeemKey,
		refundAmount: req.RefundAmount,
		full:         req.Full,
	}
	s.refundByID[req.RefundID] = rec
	s.refundsByRedeem[req.RedeemKey] = append(s.refundsByRedeem[req.RedeemKey], rec)
	cp := rec
	return &cp, nil
}

type refundFingerprint struct {
	orderID      string
	redeemKey    string
	refundAmount int64
	full         bool
}

// proratedBenefit 按退款金额占原订单金额的比例返还权益，四舍五入，
// 且不超过券面值。原订单金额为 0（无金额订单）时不做比例返还。
func proratedBenefit(faceValue, refundAmount, orderAmount int64) int64 {
	if orderAmount <= 0 || refundAmount <= 0 {
		return 0
	}
	benefit := (faceValue*refundAmount + orderAmount/2) / orderAmount
	if benefit > faceValue {
		benefit = faceValue
	}
	if benefit < 0 {
		benefit = 0
	}
	return benefit
}

func (s *Service) removeRedeemLocked(rec RedeemRecord) {
	if byVoucher := s.redeemByOrder[rec.OrderID]; byVoucher != nil {
		delete(byVoucher, rec.VoucherID)
		if len(byVoucher) == 0 {
			delete(s.redeemByOrder, rec.OrderID)
		}
	}
}

// GetRedeem 按核销号查询原核销结果。
func (s *Service) GetRedeem(key string) (RedeemRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.redeemByKey[key]
	if !ok {
		return RedeemRecord{}, ErrRedeemNotFound
	}
	return rec, nil
}

// GetRefund 按退款单号查询退款返还记录。
func (s *Service) GetRefund(refundID string) (RefundRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.refundByID[refundID]
	if !ok {
		return RefundRecord{}, ErrRefundNotFound
	}
	return rec, nil
}

// OrderRedeems 返回订单下全部兑换核销记录，按券 ID 升序。
func (s *Service) OrderRedeems(orderID string) []RedeemRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	byVoucher := s.redeemByOrder[orderID]
	if len(byVoucher) == 0 {
		return nil
	}
	out := make([]RedeemRecord, 0, len(byVoucher))
	for _, r := range byVoucher {
		out = append(out, r)
	}
	sortRedeemRecords(out)
	return out
}

// RefundsByRedeem 返回某次核销关联的全部退款返还记录，按时间顺序。
func (s *Service) RefundsByRedeem(redeemKey string) []RefundRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.refundsByRedeem[redeemKey]
	if len(src) == 0 {
		return nil
	}
	out := make([]RefundRecord, len(src))
	copy(out, src)
	return out
}
