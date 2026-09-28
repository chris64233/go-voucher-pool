// Package govoucherpool 实现一套支持“预占—确认”两阶段结算的兑换券核销服务。
//
// 核心模型：
//
//   - 券批次（Batch）记录适用范围、面值、生效/失效时间；批次内每张券的券码
//     只以“批次盐 + 全局胡椒 + SHA-256”的安全摘要形式保存，任何接口、错误
//     信息与查询视图都不会出现券码明文。
//   - 每张券在任一时刻只处于 可用 / 短期预占 / 已核销 / 已作废 四种状态之一，
//     已核销与已作废是终态。
//   - 预占（Reserve）按“订单号 + 券”幂等：同一订单重复申请返回原预占；
//     不同订单争用同一张券时只有一个成功。预占有 TTL，到期可被释放或被
//     其他订单重新预占；预占版本（version）随每次状态流转递增，旧订单的
//     迟到确认无法命中新版本，因此不可能核销后来重新预占的券。
//   - 确认（Confirm）必须携带当前预占版本；核销状态与核销记录在同一把锁内
//     原子写入，面值最多计入订单一次。确认、主动释放、超时推进并发执行，
//     终态唯一。
//   - 批次作废（VoidBatch）把批次内所有未终态券整体置为已作废；与预占、确认
//     竞争时，已经写入的核销记录保留，预占被作废后版本失效，无法再确认。
package govoucherpool

import (
	"context"
	"encoding/hex"
	"sync"
	"time"
)

// Store 是并发安全的内存券池存储。零值不可用，请使用 New 创建。
type Store struct {
	mu sync.RWMutex

	pepper []byte

	batches     map[string]*batch
	vouchers    map[string]*voucher
	byDigest    map[string]string // 摘要 hex -> 券 ID
	redemptions map[string]Redemption
	orderRedeem map[string][]string // 订单 ID -> 已核销券 ID
	orderTotal  map[string]int64    // 订单 ID -> 已核销面值合计
	orderIndex  map[string]map[string]struct{}

	clock func() time.Time
}

// Option 配置 Store。
type Option func(*Store)

// WithClock 注入自定义时钟，主要用于测试过期与并发时序。
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.clock = now
		}
	}
}

// New 创建一个空的券池存储。
func New(opts ...Option) (*Store, error) {
	pepper, err := newPepper()
	if err != nil {
		return nil, err
	}
	s := &Store{
		pepper:      pepper,
		batches:     make(map[string]*batch),
		vouchers:    make(map[string]*voucher),
		byDigest:    make(map[string]string),
		redemptions: make(map[string]Redemption),
		orderRedeem: make(map[string][]string),
		orderTotal:  make(map[string]int64),
		orderIndex:  make(map[string]map[string]struct{}),
		clock:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Now 返回存储当前使用的时钟时间。
func (s *Store) Now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clock()
}

// CreateBatchInput 是创建券批次的入参。
type CreateBatchInput struct {
	Scope       Scope
	FaceValue   int64 // 面值（最小货币单位），必须为正
	EffectiveAt time.Time
	ExpiresAt   time.Time // 必须严格晚于 EffectiveAt
}

// CreateBatch 创建券批次并返回批次 ID。
func (s *Store) CreateBatch(ctx context.Context, in CreateBatchInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if in.FaceValue <= 0 {
		return "", ErrInvalidArgument
	}
	if !in.ExpiresAt.After(in.EffectiveAt) {
		return "", ErrInvalidArgument
	}

	salt, err := newSalt()
	if err != nil {
		return "", err
	}
	id, err := newID("bat_")
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b := &batch{
		id:          id,
		scope:       cloneScope(in.Scope),
		faceValue:   in.FaceValue,
		effectiveAt: in.EffectiveAt,
		expiresAt:   in.ExpiresAt,
		salt:        salt,
	}
	s.batches[id] = b
	return id, nil
}

// RegisterVouchers 在指定批次内登记一批券码。
// 入参是券码明文，但只把摘要写入存储；返回与入参顺序对齐的券 ID。
// 同一批次内重复登记（或本次入参自带重复券码）返回 ErrCodeAlreadyRegistered。
// 不同批次使用不同批次盐，同一明文在不同批次中是各自独立的券。
func (s *Store) RegisterVouchers(ctx context.Context, batchID string, codes []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchID == "" || len(codes) == 0 {
		return nil, ErrInvalidArgument
	}
	for _, c := range codes {
		if c == "" {
			return nil, ErrInvalidArgument
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	if !b.voidedAt.IsZero() {
		return nil, ErrBatchVoided
	}

	type pending struct {
		id     string
		digest []byte
	}
	pendingList := make([]pending, 0, len(codes))
	seenReq := make(map[string]struct{}, len(codes))

	for _, code := range codes {
		d := digestCode(s.pepper, b.salt, code)
		key := hex.EncodeToString(d)
		if _, dup := seenReq[key]; dup {
			return nil, ErrCodeAlreadyRegistered
		}
		seenReq[key] = struct{}{}
		if _, exists := s.byDigest[key]; exists {
			return nil, ErrCodeAlreadyRegistered
		}
		id, err := newID("vch_")
		if err != nil {
			return nil, err
		}
		pendingList = append(pendingList, pending{id: id, digest: d})
	}

	ids := make([]string, 0, len(pendingList))
	for _, p := range pendingList {
		s.byDigest[hex.EncodeToString(p.digest)] = p.id
		s.vouchers[p.id] = &voucher{
			id:         p.id,
			batchID:    batchID,
			faceValue:  b.faceValue,
			codeDigest: p.digest,
			status:     StatusAvailable,
		}
		ids = append(ids, p.id)
	}
	return ids, nil
}

// ReserveInput 是预占入参。TTL 是预占时长；实际到期时间会被截断到批次失效时间。
type ReserveInput struct {
	BatchID string
	OrderID string
	Codes   []string
	TTL     time.Duration
}

// Reserve 按订单预占一张或多张券（整批原子：任一券不可预占则整单失败）。
//
// 语义：
//   - 同一订单对同一张券重复申请：返回原预占（原版本、原到期时间），不续期、
//     不换版本，Reused=true；
//   - 同一订单重新申请“已被自己预占但已到期”的券：产生新预占与新版本；
//   - 不同订单争用：券被未到期的其他订单持有时返回 ErrVoucherBusy；
//   - 其他订单的预占已到期时，本订单可以接管，接管产生新版本，旧版本确认失效。
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.BatchID == "" || in.OrderID == "" || len(in.Codes) == 0 || in.TTL <= 0 {
		return nil, ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.batches[in.BatchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	if !b.voidedAt.IsZero() {
		return nil, ErrBatchVoided
	}
	now := s.clock()
	if !b.inEffect(now) {
		return nil, ErrBatchNotInEffect
	}
	expireAt := now.Add(in.TTL)
	if expireAt.After(b.expiresAt) {
		expireAt = b.expiresAt
	}

	type resolved struct {
		v      *voucher
		reused bool
		take   bool // 需要新建立预占（可用券 / 接管到期预占）
	}
	entries := make([]resolved, 0, len(in.Codes))
	seen := make(map[string]struct{}, len(in.Codes))

	for _, code := range in.Codes {
		if code == "" {
			return nil, ErrInvalidArgument
		}
		v, err := s.lookupLocked(b, code)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[v.id]; dup {
			return nil, ErrInvalidArgument
		}
		seen[v.id] = struct{}{}

		switch v.status {
		case StatusAvailable:
			entries = append(entries, resolved{v: v, take: true})
		case StatusReserved:
			expired := !now.Before(v.reserveExpireAt)
			switch {
			case !expired && v.orderID == in.OrderID:
				entries = append(entries, resolved{v: v, reused: true})
			case !expired:
				return nil, ErrVoucherBusy
			default:
				// 已到期（无论原属主是谁）：本订单接管
				entries = append(entries, resolved{v: v, take: true})
			}
		case StatusRedeemed:
			return nil, ErrVoucherRedeemed
		case StatusVoided:
			return nil, ErrVoucherVoided
		default:
			return nil, ErrInvalidArgument
		}
	}

	res := &Reservation{OrderID: in.OrderID, Items: make([]ReserveResultItem, 0, len(entries))}
	for i, e := range entries {
		if e.take {
			if e.v.status == StatusReserved {
				// 接管其他订单（或本订单）的到期预占，先摘除旧属主索引。
				s.removeOrderIndex(e.v.orderID, e.v.id)
			}
			e.v.status = StatusReserved
			e.v.orderID = in.OrderID
			e.v.reserveExpireAt = expireAt
			e.v.version++
			s.addOrderIndex(in.OrderID, e.v.id)
		}
		res.Items = append(res.Items, ReserveResultItem{
			CodeIndex: i,
			VoucherID: e.v.id,
			Version:   e.v.version,
			ExpiresAt: e.v.reserveExpireAt,
			Reused:    e.reused,
		})
		if len(entries) == 1 {
			res.Version = e.v.version
		}
	}
	return res, nil
}

// Confirm 确认核销单张券。必须携带预占时返回的当前版本号。
// 核销状态变更与核销记录写入在同一临界区完成，返回的 Redemption 即入账记录。
//
// 可能返回：
//   - ErrWrongOrder        预占属于其他订单；
//   - ErrReservationExpired 预占已到期（本方法会顺带把它释放回可用池）；
//   - ErrVersionMismatch   版本过期（旧订单迟到确认的典型情形）；
//   - ErrVoucherVoided/ErrBatchVoided 券或批次已作废；
//   - ErrVoucherRedeemed   券已核销（重复确认不重复入账）。
func (s *Store) Confirm(ctx context.Context, batchID, orderID, code string, version int64) (*Redemption, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchID == "" || orderID == "" || code == "" {
		return nil, ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, v, err := s.latchVoucher(batchID, code)
	if err != nil {
		return nil, err
	}
	now := s.clock()

	switch v.status {
	case StatusRedeemed:
		return nil, ErrVoucherRedeemed
	case StatusVoided:
		return nil, ErrVoucherVoided
	case StatusAvailable:
		return nil, ErrVoucherNotReserved
	case StatusReserved:
		// 继续往下
	default:
		return nil, ErrInvalidArgument
	}
	if v.orderID != orderID {
		return nil, ErrWrongOrder
	}
	if !b.voidedAt.IsZero() {
		return nil, ErrBatchVoided
	}
	if !now.Before(v.reserveExpireAt) {
		// 预占到期：释放回可用池并推进版本，迟到确认到此为止。
		s.releaseLocked(v)
		return nil, ErrReservationExpired
	}
	if v.version != version {
		// 旧版本：可能是调用方重试旧预占，或预占曾被释放/接管。
		return nil, ErrVersionMismatch
	}

	v.status = StatusRedeemed
	v.redeemedAt = now
	v.version++
	rec := v.redemptionView(now)
	s.redemptions[v.id] = rec
	s.orderRedeem[orderID] = append(s.orderRedeem[orderID], v.id)
	s.orderTotal[orderID] += v.faceValue
	return &rec, nil
}

// Release 主动释放订单对单张券的预占。
// 释放自己（含已到期）的预占成功；释放其他订单的预占返回 ErrWrongOrder；
// 对已核销/已作废/可用的券分别返回对应错误，且不会改变其状态。
func (s *Store) Release(ctx context.Context, batchID, orderID, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batchID == "" || orderID == "" || code == "" {
		return ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, v, err := s.latchVoucher(batchID, code)
	if err != nil {
		return err
	}
	switch v.status {
	case StatusRedeemed:
		return ErrVoucherRedeemed
	case StatusVoided:
		return ErrVoucherVoided
	case StatusAvailable:
		return ErrVoucherNotReserved
	case StatusReserved:
		// 继续
	default:
		return ErrInvalidArgument
	}
	if v.orderID != orderID {
		return ErrWrongOrder
	}
	s.releaseLocked(v)
	return nil
}

// AdvanceExpiry 把所有已经到期的预占推进回可用状态，返回释放数量。
// 幂等：重复调用只处理当前真正到期的预占。
func (s *Store) AdvanceExpiry(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	n := 0
	for _, v := range s.vouchers {
		if v.status == StatusReserved && !now.Before(v.reserveExpireAt) {
			s.releaseLocked(v)
			n++
		}
	}
	return n, nil
}

// VoidBatch 整体作废一个批次：批次本身标记作废，批次内所有尚未核销的券
// （含可用与预占中）立即转入已作废；已核销的券及其核销记录原样保留。
// 重复作废返回 ErrBatchVoided。返回的 VoidedCount 是本次新作废的券数。
func (s *Store) VoidBatch(ctx context.Context, batchID string) (*VoidResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchID == "" {
		return nil, ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	if !b.voidedAt.IsZero() {
		return nil, ErrBatchVoided
	}
	now := s.clock()
	b.voidedAt = now

	count := 0
	for _, v := range s.vouchers {
		if v.batchID != batchID {
			continue
		}
		if v.status.isTerminal() {
			continue
		}
		if v.status == StatusReserved {
			s.removeOrderIndex(v.orderID, v.id)
		}
		v.status = StatusVoided
		v.voidedAt = now
		v.orderID = ""
		v.reserveExpireAt = time.Time{}
		v.version++
		count++
	}
	return &VoidResult{BatchID: batchID, VoidedAt: now, VoidedCount: count}, nil
}

// ---- 查询 ----

// GetBatch 查询批次视图；不存在返回 ErrBatchNotFound。
func (s *Store) GetBatch(ctx context.Context, batchID string) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	view := b.view()
	return &view, nil
}

// GetVoucher 按券码查询券状态。入参是明文，但仅用于计算摘要，
// 返回视图不包含券码；任何错误信息也不包含券码。
func (s *Store) GetVoucher(ctx context.Context, batchID, code string) (*Voucher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchID == "" || code == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	v, err := s.lookupLocked(b, code)
	if err != nil {
		return nil, err
	}
	view := v.view()
	return &view, nil
}

// ListVouchers 列出批次内全部券的状态视图（管理用途），不含券码。
func (s *Store) ListVouchers(ctx context.Context, batchID string) ([]Voucher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.batches[batchID]; !ok {
		return nil, ErrBatchNotFound
	}
	out := make([]Voucher, 0, len(s.vouchers))
	for _, v := range s.vouchers {
		if v.batchID == batchID {
			out = append(out, v.view())
		}
	}
	return out, nil
}

// GetOrder 查询订单维度状态：未到期预占、已核销券、面值合计与核销记录。
// 订单从未出现（无预占、无核销）时返回 ErrOrderNotFound。
func (s *Store) GetOrder(ctx context.Context, orderID string) (*OrderStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if orderID == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, known := s.orderIndex[orderID]
	redeemed := s.orderRedeem[orderID]
	if !known && len(redeemed) == 0 {
		return nil, ErrOrderNotFound
	}
	now := s.clock()
	st := &OrderStatus{
		OrderID:            orderID,
		RedeemedVoucherIDs: append([]string(nil), redeemed...),
		TotalFaceValue:     s.orderTotal[orderID],
	}
	for id := range ids {
		v := s.vouchers[id]
		if v.status == StatusReserved && now.Before(v.reserveExpireAt) {
			st.ActiveReservations = append(st.ActiveReservations, v.view())
		}
	}
	for _, vid := range redeemed {
		if rec, ok := s.redemptions[vid]; ok {
			st.Redemptions = append(st.Redemptions, rec)
		}
	}
	return st, nil
}

// GetRedemption 按券 ID 查询核销记录；未核销或不存在返回 ErrRedemptionNotFound。
func (s *Store) GetRedemption(ctx context.Context, voucherID string) (*Redemption, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if voucherID == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.redemptions[voucherID]
	if !ok {
		return nil, ErrRedemptionNotFound
	}
	out := rec
	return &out, nil
}

// ---- 内部辅助（调用方必须持锁）----

// latchVoucher 取批次与券，批次不存在与券不存在返回不同错误，
// 但两者的错误文本都不携带券码。
func (s *Store) latchVoucher(batchID, code string) (*batch, *voucher, error) {
	b, ok := s.batches[batchID]
	if !ok {
		return nil, nil, ErrBatchNotFound
	}
	v, err := s.lookupLocked(b, code)
	if err != nil {
		return nil, nil, err
	}
	return b, v, nil
}

// lookupLocked 用券码明文现场计算摘要并查找券；明文不落任何存储字段。
func (s *Store) lookupLocked(b *batch, code string) (*voucher, error) {
	d := digestCode(s.pepper, b.salt, code)
	id, ok := s.byDigest[hex.EncodeToString(d)]
	if !ok {
		return nil, ErrVoucherNotFound
	}
	return s.vouchers[id], nil
}

// releaseLocked 把预占中的券释放回可用池并推进版本。
func (s *Store) releaseLocked(v *voucher) {
	s.removeOrderIndex(v.orderID, v.id)
	v.status = StatusAvailable
	v.orderID = ""
	v.reserveExpireAt = time.Time{}
	v.version++
}

func (s *Store) addOrderIndex(orderID, voucherID string) {
	set := s.orderIndex[orderID]
	if set == nil {
		set = make(map[string]struct{})
		s.orderIndex[orderID] = set
	}
	set[voucherID] = struct{}{}
}

func (s *Store) removeOrderIndex(orderID, voucherID string) {
	if set := s.orderIndex[orderID]; set != nil {
		delete(set, voucherID)
		if len(set) == 0 {
			delete(s.orderIndex, orderID)
		}
	}
}

func cloneScope(in Scope) Scope {
	out := Scope{
		CategoryIDs: append([]string(nil), in.CategoryIDs...),
		ShopIDs:     append([]string(nil), in.ShopIDs...),
	}
	return out
}
