package govoucherpool

import "time"

// State 是一张券生命周期内唯一所处的状态。
type State uint8

const (
	// StateAvailable 可用，可被预占。
	StateAvailable State = iota + 1
	// StateHeld 短期预占，已归属某个订单但尚未核销。
	StateHeld
	// StateRedeemed 已核销（终态）。
	StateRedeemed
	// StateVoided 已作废（终态，可由批次整体作废或单券作废产生）。
	StateVoided
)

// String 仅返回状态名，不包含任何券码信息。
func (s State) String() string {
	switch s {
	case StateAvailable:
		return "available"
	case StateHeld:
		return "held"
	case StateRedeemed:
		return "redeemed"
	case StateVoided:
		return "voided"
	default:
		return "unknown"
	}
}

// Scope 描述券批次的适用范围。取值由调用方解释，
// 例如适用商品/门店/类目 ID 的集合；为空表示全场通用。
type Scope struct {
	// Categories 适用类目。
	Categories []string
	// Stores 适用门店。
	Stores []string
	// SKUs 适用商品。为空且其余字段也为空时视为全场通用。
	SKUs []string
}

func (s Scope) clone() Scope {
	return Scope{
		Categories: append([]string(nil), s.Categories...),
		Stores:     append([]string(nil), s.Stores...),
		SKUs:       append([]string(nil), s.SKUs...),
	}
}

// Matches 判断给定条件（类目/门店/商品）是否落在适用范围内。
// 批次范围为空时全场通用；否则请求的三个维度只要在批次中有任一交集即可，
// 但请求中显式给出的每一个维度都必须能在范围内匹配到。
func (s Scope) Matches(categories, stores, skus []string) bool {
	if len(s.Categories) == 0 && len(s.Stores) == 0 && len(s.SKUs) == 0 {
		return true
	}
	matchedAny := false
	if len(categories) > 0 {
		if !allIn(categories, s.Categories) {
			return false
		}
		matchedAny = true
	}
	if len(stores) > 0 {
		if !allIn(stores, s.Stores) {
			return false
		}
		matchedAny = true
	}
	if len(skus) > 0 {
		if !allIn(skus, s.SKUs) {
			return false
		}
		matchedAny = true
	}
	return matchedAny
}

func allIn(want, allowed []string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, v := range allowed {
		set[v] = struct{}{}
	}
	for _, v := range want {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}

// Batch 是一个券批次。面值以最小货币单位的整数表示，避免浮点误差。
type Batch struct {
	// ID 服务端生成的批次 ID。
	ID string
	// Name 批次名称，仅用于展示。
	Name string
	// FaceValue 面值（最小货币单位，必须为正）。
	FaceValue int64
	// Scope 适用范围。
	Scope Scope
	// EffectiveAt 生效时间（含）。
	EffectiveAt time.Time
	// ExpiresAt 失效时间（不含）。
	ExpiresAt time.Time
	// Voided 是否已整体作废。
	Voided bool
	// CreatedAt 创建时间。
	CreatedAt time.Time

	// RuleVersion 优惠规则版本，核销请求必须固定同一版本。
	// 0 视为版本 1。
	RuleVersion int32
	// MinOrderAmount 最低订单金额（含，最小货币单位）；0 表示无门槛。
	MinOrderAmount int64
	// RefundPolicy 退款返还规则。
	RefundPolicy RefundPolicy
	// Stock 发行库存量；<=0 表示不限量。
	Stock int64
	// remaining 当前可核销库存，仅在 Stock > 0 时有效。
	remaining int64
}

// RefundPolicy 描述一张券（批次）在订单退款时的权益返还规则。
type RefundPolicy struct {
	// AllowPartial 允许部分退款：按退款金额占原订单金额的比例返还权益，
	// 返还只记账，不恢复券码、不回补池库存。
	AllowPartial bool
	// RestoreOnFullRefund 全额退款时把整张券恢复为可用并回补池库存；
	// 为 false 时全额退款只返还权益（受 AllowPartial 控制），券保持终态。
	RestoreOnFullRefund bool
}

// RemainingStock 返回剩余可核销库存；不限量批次返回 -1。
func (b *Batch) RemainingStock() int64 {
	if b.Stock <= 0 {
		return -1
	}
	return b.remaining
}

// Voucher 是一张券的内部记录。注意：券码明文永不保存在此结构中，
// 只有不可逆的 CodeDigest 与服务端盐值配合使用。
type Voucher struct {
	// ID 服务端生成的券 ID，可安全暴露给业务方用于状态查询。
	ID string
	// BatchID 所属批次。
	BatchID string
	// CodeDigest 券码摘要（salt 随机、不入库明文）。
	// 该字段不允许出现在任何错误信息中；对外查询时同样脱敏。
	CodeDigest string

	state State
	face  int64
	scope Scope

	// 当前预占信息，仅在 state == StateHeld 且未到期时有效。
	heldBy    string // 预占订单号
	version   int64  // 当前预占版本，每次（重新）预占递增
	heldAt    time.Time
	expiresAt time.Time

	// 终态信息。
	redeemedAt  time.Time
	redeemOrder string // 核销归属订单
	voidedAt    time.Time
	voidReason  string

	// 直接核销（Redeem）写入的请求要素，用于退款返还与幂等校验。
	redeemUser        string
	redeemRuleVersion int32
	redeemOrderAmount int64

	// 退款返还累计（最小货币单位）。
	refundedAmount  int64
	returnedBenefit int64
	fullyRefunded   bool
}

// Snapshot 是券状态的对外只读视图，不含券码明文或摘要。
type Snapshot struct {
	ID              string
	BatchID         string
	State           State
	FaceValue       int64
	Scope           Scope
	Version         int64 // 当前预占版本；非预占态返回 0
	HeldBy          string
	HoldExpires     time.Time
	RedeemedAt      time.Time
	RedeemOrder     string
	VoidedAt        time.Time
	VoidReason      string
	RefundedAmount  int64 // 累计退款金额
	ReturnedBenefit int64 // 累计返还权益
	FullyRefunded   bool  // 是否已全额退款返还
}

func (v *Voucher) snapshot() Snapshot {
	snap := Snapshot{
		ID:              v.ID,
		BatchID:         v.BatchID,
		State:           v.state,
		FaceValue:       v.face,
		Scope:           v.scope.clone(),
		Version:         v.version,
		RedeemedAt:      v.redeemedAt,
		RedeemOrder:     v.redeemOrder,
		VoidedAt:        v.voidedAt,
		VoidReason:      v.voidReason,
		RefundedAmount:  v.refundedAmount,
		ReturnedBenefit: v.returnedBenefit,
		FullyRefunded:   v.fullyRefunded,
	}
	if v.state == StateHeld {
		snap.HeldBy = v.heldBy
		snap.HoldExpires = v.expiresAt
	} else {
		snap.Version = 0
	}
	return snap
}

// Redemption 是一条核销记录，与券状态翻转在同一临界区内写入。
type Redemption struct {
	VoucherID string
	BatchID   string
	OrderID   string
	FaceValue int64
	Version   int64 // 核销时使用的预占版本
	At        time.Time
}

// RedeemRequest 是一次兑换核销请求的固定要素。请求要素与核销号绑定：
// 同一核销号重复提交必须携带完全相同的券码、用户、订单、规则版本与订单金额，
// 否则返回 ErrIdempotencyConflict；订单金额变化后旧请求不能继续使用。
type RedeemRequest struct {
	// Key 业务侧核销号（幂等键），必填。
	Key string
	// UserID 核销用户，必填。
	UserID string
	// OrderID 归属订单，必填。
	OrderID string
	// Code 券码明文，仅当场用于摘要计算，不落库。
	Code string
	// RuleVersion 请求固定的优惠规则版本，必须与批次当前版本一致。
	RuleVersion int32
	// OrderAmount 下单时的订单金额（最小货币单位），用于门槛与退款比例计算。
	OrderAmount int64
	// Categories/Stores/SKUs 订单命中的适用范围维度，用于规则匹配。
	Categories []string
	Stores     []string
	SKUs       []string
}

// RedeemRecord 是一次成功兑换核销的结果记录。
type RedeemRecord struct {
	Key         string
	VoucherID   string
	BatchID     string
	UserID      string
	OrderID     string
	FaceValue   int64
	RuleVersion int32
	OrderAmount int64
	At          time.Time
}

func (r RedeemRecord) clone() *RedeemRecord {
	cp := r
	return &cp
}

// RefundRequest 是退款返还请求。
type RefundRequest struct {
	// RefundID 退款单号（幂等键），必填。
	RefundID string
	// OrderID 原核销订单，必填。
	OrderID string
	// RedeemKey 原核销号，必填，用于关联原核销。
	RedeemKey string
	// RefundAmount 本次退款金额（最小货币单位），必须 > 0。
	RefundAmount int64
	// Full 是否全额退款。
	Full bool
}

// RefundRecord 是一次退款返还结果。ReturnedBenefit 为本次返还的权益金额；
// 部分退款不恢复券码，全额退款且规则允许时 Restored 为 true。
type RefundRecord struct {
	RefundID        string
	RedeemKey       string
	VoucherID       string
	BatchID         string
	OrderID         string
	RefundAmount    int64
	ReturnedBenefit int64
	FullRefund      bool
	Restored        bool
	At              time.Time
}
