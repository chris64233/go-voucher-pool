package govoucherpool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Clock 抽象时间来源，生产环境用 time.Now，测试可注入固定时钟。
type Clock func() time.Time

// CreateBatchInput 创建批次的入参。
type CreateBatchInput struct {
	Name        string
	FaceValue   int64 // 面值，最小货币单位，必须 > 0
	Scope       Scope
	EffectiveAt time.Time
	ExpiresAt   time.Time

	// RuleVersion 优惠规则版本，0 视为 1。
	RuleVersion int32
	// MinOrderAmount 最低订单金额（含），0 表示无门槛。
	MinOrderAmount int64
	// RefundPolicy 退款返还规则。
	RefundPolicy RefundPolicy
	// Stock 发行库存，<=0 表示不限量。
	Stock int64
}

// ReserveResult 预占结果。
type ReserveResult struct {
	VoucherID string
	OrderID   string
	Version   int64
	HeldAt    time.Time
	ExpiresAt time.Time
}

// Service 是兑换券核销服务，所有方法对并发调用安全。
//
// 券码安全：服务启动时生成随机盐，落库的只有 sha256(salt||code) 摘要，
// 明文不进入任何结构体、日志或错误信息。
type Service struct {
	mu sync.Mutex

	now       Clock
	salt      []byte
	idSeq     uint64
	batches   map[string]*Batch
	vouchers  map[string]*Voucher // voucherID -> voucher
	digestIdx map[string]*Voucher // code digest -> voucher
	// 订单核销账本：orderID -> voucherID -> Redemption。
	// 同一券只会在状态翻转的临界区内写入一次，面值因此不会被重复计入。
	redemptions map[string]map[string]Redemption

	// 兑换核销链路索引。
	redeemByKey        map[string]RedeemRecord            // 核销号 -> 原结果（幂等返回）
	redeemByOrder      map[string]map[string]RedeemRecord // 订单 -> 券 -> 核销记录
	refundByID         map[string]RefundRecord            // 退款单号 -> 原结果
	refundsByRedeem    map[string][]RefundRecord          // 核销号 -> 退款返还记录
	redeemFingerprints map[string]redeemFingerprint       // 核销号 -> 首次请求要素
	refundFingerprints map[string]refundFingerprint       // 退款单号 -> 首次请求要素
}

// NewService 创建服务。clock 为 nil 时使用 time.Now。
func NewService(clock Clock) (*Service, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		now:                clock,
		salt:               salt,
		batches:            make(map[string]*Batch),
		vouchers:           make(map[string]*Voucher),
		digestIdx:          make(map[string]*Voucher),
		redemptions:        make(map[string]map[string]Redemption),
		redeemByKey:        make(map[string]RedeemRecord),
		redeemByOrder:      make(map[string]map[string]RedeemRecord),
		refundByID:         make(map[string]RefundRecord),
		refundsByRedeem:    make(map[string][]RefundRecord),
		redeemFingerprints: make(map[string]redeemFingerprint),
		refundFingerprints: make(map[string]refundFingerprint),
	}, nil
}

func (s *Service) nextID(prefix string) string {
	s.idSeq++
	return prefix + itoa(s.idSeq)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Digest 返回券码在本服务内的存储摘要。仅供登记/预占/确认等内部链路使用，
// 调用方不应把它展示给终端用户；摘要不可逆，但稳定摘要可用于查重，
// 因此也不应出现在 URL、错误信息或日志中。
func (s *Service) Digest(code string) string {
	sum := sha256.Sum256(append(append([]byte(nil), s.salt...), []byte(code)...))
	return hex.EncodeToString(sum[:])
}

// CreateBatch 创建券批次。
func (s *Service) CreateBatch(in CreateBatchInput) (*Batch, error) {
	if in.FaceValue <= 0 {
		return nil, ErrInvalidBatch
	}
	if !in.EffectiveAt.Before(in.ExpiresAt) {
		return nil, ErrInvalidBatch
	}
	if in.MinOrderAmount < 0 {
		return nil, ErrInvalidBatch
	}
	ruleVersion := in.RuleVersion
	if ruleVersion <= 0 {
		ruleVersion = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &Batch{
		ID:             s.nextID("bat_"),
		Name:           in.Name,
		FaceValue:      in.FaceValue,
		Scope:          in.Scope.clone(),
		EffectiveAt:    in.EffectiveAt,
		ExpiresAt:      in.ExpiresAt,
		CreatedAt:      s.now(),
		RuleVersion:    ruleVersion,
		MinOrderAmount: in.MinOrderAmount,
		RefundPolicy:   in.RefundPolicy,
		Stock:          in.Stock,
		remaining:      in.Stock,
	}
	s.batches[b.ID] = b
	return b.clone(), nil
}

// GetBatch 查询批次（副本）。
func (s *Service) GetBatch(batchID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	return b.clone(), nil
}

func (b *Batch) clone() *Batch {
	cp := *b
	cp.Scope = b.Scope.clone()
	return &cp
}

// RegisterVoucher 在指定批次登记一张券。code 仅用于当场计算摘要，
// 方法返回后服务内不再保留明文。返回可安全暴露的券 ID。
func (s *Service) RegisterVoucher(batchID, code string) (string, error) {
	if code == "" {
		return "", ErrEmptyCode
	}
	digest := s.Digest(code)

	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return "", ErrBatchNotFound
	}
	if _, dup := s.digestIdx[digest]; dup {
		return "", ErrCodeAlreadyRegistered
	}
	v := &Voucher{
		ID:         s.nextID("vch_"),
		BatchID:    batchID,
		CodeDigest: digest,
		state:      StateAvailable,
		face:       b.FaceValue,
		scope:      b.Scope.clone(),
		version:    0,
	}
	s.vouchers[v.ID] = v
	s.digestIdx[digest] = v
	return v.ID, nil
}

// Reserve 按订单号预占一张券（以券码定位）。
//
// 语义：
//   - 同一订单对同一券重复申请，且预占仍有效：返回原预占（版本不变）。
//   - 同一订单的旧预占已到期：重新预占，版本递增。
//   - 不同订单争用：当前有效预占持有者获胜，其余返回 ErrVoucherHeld；
//     预占到期后其他订单可抢占，旧版本确认将因版本过期失败。
//   - ttl 为预占时长，实际到期时间会被截断在批次失效时间之内。
func (s *Service) Reserve(orderID, code string, ttl time.Duration) (*ReserveResult, error) {
	if orderID == "" || code == "" || ttl <= 0 {
		return nil, ErrInvalidArgument
	}
	digest := s.Digest(code)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	v, err := s.lookupLocked(digest)
	if err != nil {
		return nil, err
	}
	// 先判券自身终态，错误更具体，也不依赖批次标志。
	switch v.state {
	case StateRedeemed:
		return nil, ErrVoucherRedeemed
	case StateVoided:
		return nil, ErrVoucherVoided
	}
	b := s.batches[v.BatchID]
	if b.Voided {
		return nil, ErrBatchVoided
	}
	if now.Before(b.EffectiveAt) || !now.Before(b.ExpiresAt) {
		return nil, ErrBatchNotInEffect
	}

	switch v.state {
	case StateAvailable:
		// 正常预占。
	case StateHeld:
		if now.Before(v.expiresAt) {
			if v.heldBy == orderID {
				// 同单同券重复申请：幂等返回原预占。
				return holdResult(v), nil
			}
			return nil, ErrVoucherHeld
		}
		// 旧预占已到期，落入下面的重新预占流程。
	default:
		return nil, ErrVoucherUnavailable
	}

	s.placeHoldLocked(v, orderID, now, ttl, b.ExpiresAt)
	return holdResult(v), nil
}

func (s *Service) placeHoldLocked(v *Voucher, orderID string, now time.Time, ttl time.Duration, batchExpiry time.Time) {
	v.state = StateHeld
	v.heldBy = orderID
	v.version++
	v.heldAt = now
	v.expiresAt = now.Add(ttl)
	if v.expiresAt.After(batchExpiry) {
		v.expiresAt = batchExpiry
	}
}

func holdResult(v *Voucher) *ReserveResult {
	return &ReserveResult{
		VoucherID: v.ID,
		OrderID:   v.heldBy,
		Version:   v.version,
		HeldAt:    v.heldAt,
		ExpiresAt: v.expiresAt,
	}
}

// Confirm 携带当前预占版本确认核销，并在同一临界区内写入核销记录、
// 计入订单面值。重复确认（同单同版本）幂等返回已存在的核销记录，
// 不会重复计账。
func (s *Service) Confirm(orderID, code string, version int64) (*Redemption, error) {
	if orderID == "" || code == "" {
		return nil, ErrInvalidArgument
	}
	digest := s.Digest(code)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	v, err := s.lookupLocked(digest)
	if err != nil {
		return nil, err
	}
	b := s.batches[v.BatchID]

	// 幂等：已由该订单核销（旧版本号迟到确认也算同一终态）。
	if v.state == StateRedeemed {
		if v.redeemOrder == orderID {
			if r, ok := s.redemptions[orderID][v.ID]; ok {
				return r.clone(), nil
			}
		}
		return nil, ErrVoucherRedeemed
	}
	if v.state == StateVoided {
		return nil, ErrVoucherVoided
	}
	if b.Voided {
		return nil, ErrBatchVoided
	}

	// state == StateHeld
	if v.heldBy != orderID {
		// 典型场景：旧订单的迟到确认，券已释放并被其他订单重新预占。
		return nil, ErrHoldMismatch
	}
	if v.version != version {
		// 旧预占的迟到确认：版本已因释放/超时重占而推进。
		return nil, ErrVersionMismatch
	}
	if !now.Before(v.expiresAt) {
		// 预占到期但尚未被推进/抢占：旧订单的迟到确认同样不允许核销。
		return nil, ErrHoldExpired
	}
	if !now.Before(b.ExpiresAt) {
		return nil, ErrBatchNotInEffect
	}

	r := Redemption{
		VoucherID: v.ID,
		BatchID:   v.BatchID,
		OrderID:   orderID,
		FaceValue: v.face,
		Version:   version,
		At:        now,
	}
	v.state = StateRedeemed
	v.redeemedAt = now
	v.redeemOrder = orderID
	if s.redemptions[orderID] == nil {
		s.redemptions[orderID] = make(map[string]Redemption)
	}
	s.redemptions[orderID][v.ID] = r
	return r.clone(), nil
}

// Release 由预占订单主动释放，必须携带当前预占版本。
// 释放后券回到可用，一旦被重新预占即产生新版本号，
// 任何持旧版本的迟到确认/释放都会被版本或归属栅栏拒绝。
// 对已到期（已被 ExpireHolds 回收）的同单预占调用视为幂等成功。
func (s *Service) Release(orderID, code string, version int64) error {
	if orderID == "" || code == "" {
		return ErrInvalidArgument
	}
	digest := s.Digest(code)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	v, err := s.lookupLocked(digest)
	if err != nil {
		return err
	}

	switch v.state {
	case StateRedeemed:
		// 终态：任何订单都不能再释放。
		return ErrVoucherRedeemed
	case StateVoided:
		return ErrVoucherVoided
	case StateAvailable:
		// 已被超时推进回收，释放视为幂等成功。
		return nil
	}

	if v.heldBy != orderID {
		return ErrHoldMismatch
	}
	if v.version != version {
		// 旧版本调用：券要么早已释放，要么已被重新预占。
		// 若券恰好仍由本单持有一个更新的到期预占，也算版本过期。
		return ErrVersionMismatch
	}
	if !now.Before(v.expiresAt) {
		// 到期预占同样回收；版本推进使迟到确认被拒绝。
	}
	s.clearHoldLocked(v)
	return nil
}

// clearHoldLocked 把券回收为可用，但不推进版本：版本只在发放新预占
// （placeHoldLocked）时递增。这样“释放 → 被其他订单重新预占”恰好推进一次，
// 旧版本的迟到确认/释放必然被版本或订单归属拦下。
func (s *Service) clearHoldLocked(v *Voucher) {
	v.state = StateAvailable
	v.heldBy = ""
	v.heldAt = time.Time{}
	v.expiresAt = time.Time{}
}

// ExpireHolds 超时推进：把所有已到期但仍处于预占态的券回收为可用，
// 并推进其预占版本。返回本次被回收的券 ID（不含券码）。
func (s *Service) ExpireHolds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireHoldsLocked(s.now())
}

func (s *Service) expireHoldsLocked(now time.Time) []string {
	reclaimed := make([]string, 0)
	for _, v := range s.vouchers {
		if v.state == StateHeld && !now.Before(v.expiresAt) {
			s.clearHoldLocked(v)
			reclaimed = append(reclaimed, v.ID)
		}
	}
	return reclaimed
}

// VoidBatch 整体作废批次：已核销的券保留，其余（可用/预占中/预占已到期）
// 一律翻转为已作废并推进版本，使尚未完成的预占无法再确认。
// 返回被作废的券 ID 列表；已作废批次重复调用返回该列表为空且不报错。
func (s *Service) VoidBatch(batchID, reason string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	now := s.now()
	// 先把到期预占推进掉，保证判断一致。
	s.expireHoldsLocked(now)

	voided := make([]string, 0)
	if !b.Voided {
		b.Voided = true
	}
	for _, v := range s.vouchers {
		if v.BatchID != batchID {
			continue
		}
		switch v.state {
		case StateRedeemed, StateVoided:
			// 已确认的核销保留；终态不动。
			continue
		case StateHeld:
			// 抢占尚未完成的预占；版本字段保留已无意义，终态检查会先行拒绝。
			v.heldBy = ""
			v.heldAt = time.Time{}
			v.expiresAt = time.Time{}
		}
		v.state = StateVoided
		v.voidedAt = now
		v.voidReason = reason
		voided = append(voided, v.ID)
	}
	return voided, nil
}

// GetVoucher 按券 ID 查询脱敏状态，返回的 Snapshot 不含券码明文或摘要。
func (s *Service) GetVoucher(voucherID string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vouchers[voucherID]
	if !ok {
		return Snapshot{}, ErrVoucherNotFound
	}
	return v.snapshot(), nil
}

// OrderRedemptions 查询订单已写入的核销记录副本，按券 ID 升序返回稳定序列。
func (s *Service) OrderRedemptions(orderID string) []Redemption {
	s.mu.Lock()
	defer s.mu.Unlock()
	byVoucher := s.redemptions[orderID]
	if len(byVoucher) == 0 {
		return nil
	}
	out := make([]Redemption, 0, len(byVoucher))
	for _, r := range byVoucher {
		out = append(out, r)
	}
	sortRedemptions(out)
	return out
}

// OrderRedeemedValue 返回订单已核销面值合计（最小货币单位）。
// 只统计真正写入核销记录的券，重复确认不会重复计入。
func (s *Service) OrderRedeemedValue(orderID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.redemptions[orderID] {
		total += r.FaceValue
	}
	return total
}

// lookupLocked 以券码摘要定位券。找不到时返回不含任何入参内容的错误。
func (s *Service) lookupLocked(digest string) (*Voucher, error) {
	v, ok := s.digestIdx[digest]
	if !ok {
		return nil, ErrCodeNotFound
	}
	return v, nil
}

func (r Redemption) clone() *Redemption {
	cp := r
	return &cp
}
