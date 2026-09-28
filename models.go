package govoucherpool

import "time"

// Status 是券在其整个生命周期内唯一所处的状态。
type Status string

const (
	// StatusAvailable 可用：可被任意订单预占。
	StatusAvailable Status = "available"
	// StatusReserved 短期预占：被某个订单临时持有，等待确认或释放。
	StatusReserved Status = "reserved"
	// StatusRedeemed 已核销：终态，面值已计入对应订单。
	StatusRedeemed Status = "redeemed"
	// StatusVoided 已作废：终态（随批次整体作废或单独作废）。
	StatusVoided Status = "voided"
)

// isTerminal 报告状态是否为不可再迁移的终态。
func (s Status) isTerminal() bool {
	return s == StatusRedeemed || s == StatusVoided
}

// Scope 描述券批次的适用范围。
// ShopIDs 非空时仅这些门店可用，否则全门店通用。
type Scope struct {
	// CategoryIDs 适用品类 ID；为空表示不限制品类。
	CategoryIDs []string
	// ShopIDs 适用门店 ID；为空表示全门店通用。
	ShopIDs []string
}

// Batch 是券批次的对外只读视图。
type Batch struct {
	ID          string
	Scope       Scope
	FaceValue   int64 // 面值，以最小货币单位表示的正整数
	EffectiveAt time.Time
	ExpiresAt   time.Time
	VoidedAt    time.Time // 零值表示尚未作废
}

func (b *batch) view() Batch {
	return Batch{
		ID:          b.id,
		Scope:       b.scope,
		FaceValue:   b.faceValue,
		EffectiveAt: b.effectiveAt,
		ExpiresAt:   b.expiresAt,
		VoidedAt:    b.voidedAt,
	}
}

// batch 是批次的内部可变表示。
type batch struct {
	id          string
	scope       Scope
	faceValue   int64
	effectiveAt time.Time
	expiresAt   time.Time
	voidedAt    time.Time
	// salt 为该批次券码摘要使用的随机盐，仅存在内存中。
	salt []byte
}

// inEffect 报告批次在给定时刻是否处于生效窗口内。
func (b *batch) inEffect(now time.Time) bool {
	return !now.Before(b.effectiveAt) && now.Before(b.expiresAt)
}

// Voucher 是单张券的对外只读视图，任何字段都不包含券码明文。
type Voucher struct {
	ID              string
	BatchID         string
	FaceValue       int64
	Status          Status
	OrderID         string    // 仅在 StatusReserved/Redeemed 时有意义
	ReserveVersion  int64     // 当前预占版本；确认时必须回传
	ReserveExpireAt time.Time // 预占到期时间
	RedeemedAt      time.Time
	VoidedAt        time.Time
}

func (v *voucher) view() Voucher {
	return Voucher{
		ID:              v.id,
		BatchID:         v.batchID,
		FaceValue:       v.faceValue,
		Status:          v.status,
		OrderID:         v.orderID,
		ReserveVersion:  v.version,
		ReserveExpireAt: v.reserveExpireAt,
		RedeemedAt:      v.redeemedAt,
		VoidedAt:        v.voidedAt,
	}
}

// voucher 是单张券的内部可变表示。券码只以摘要形式存储（codeDigest）。
type voucher struct {
	id              string
	batchID         string
	faceValue       int64
	codeDigest      []byte // 券码安全摘要（批次盐 + 全局胡椒 + SHA-256）
	status          Status
	orderID         string
	version         int64 // 预占版本：每次进入/离开预占都递增
	reserveExpireAt time.Time
	redeemedAt      time.Time
	voidedAt        time.Time
}

// Redemption 是核销记录的对外只读视图。
// 核销与核销记录在同一个临界区内写入，保证“券终态唯一”与“面值只计一次”同源。
type Redemption struct {
	VoucherID  string
	BatchID    string
	OrderID    string
	FaceValue  int64
	RedeemedAt time.Time
}

func (v *voucher) redemptionView(at time.Time) Redemption {
	return Redemption{
		VoucherID:  v.id,
		BatchID:    v.batchID,
		OrderID:    v.orderID,
		FaceValue:  v.faceValue,
		RedeemedAt: at,
	}
}

// ReserveResultItem 是批量预占结果中单张券的信息。
type ReserveResultItem struct {
	// CodeIndex 对应该次预占入参 codes 中的下标。
	CodeIndex int
	VoucherID string
	Version   int64 // 当前预占版本，确认时必须原样携带
	ExpiresAt time.Time
	// Reused 为 true 表示该券原本就被同一订单预占，本次返回的是原预占（幂等命中）。
	Reused bool
}

// Reservation 是一次预占请求的整体结果。
type Reservation struct {
	OrderID string
	Items   []ReserveResultItem
	// Version 兼容单券场景，等于唯一一张券的预占版本。
	Version int64
}

// VoidResult 是批次作废的结果。
type VoidResult struct {
	BatchID     string
	VoidedAt    time.Time
	VoidedCount int // 本次实际转入已作废的券数（已核销的保留，不计入）
}

// OrderStatus 是订单维度的查询结果。
type OrderStatus struct {
	OrderID            string
	ActiveReservations []Voucher    // 当前仍归属于该订单且未到期的预占
	RedeemedVoucherIDs []string     // 该订单已核销的券 ID
	TotalFaceValue     int64        // 已计入订单的面值合计
	Redemptions        []Redemption // 核销记录列表
}
