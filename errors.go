package govoucherpool

import "errors"

// 所有错误信息均不携带券码明文，也不携带调用方传入的摘要前缀，
// 避免通过错误回显枚举券码。
var (
	// ErrBatchNotFound 批次不存在。
	ErrBatchNotFound = errors.New("govoucherpool: batch not found")
	// ErrBatchVoided 批次已整体作废。
	ErrBatchVoided = errors.New("govoucherpool: batch is voided")
	// ErrBatchNotInEffect 批次尚未生效或已过失效时间。
	ErrBatchNotInEffect = errors.New("govoucherpool: batch is not within its effective window")
	// ErrInvalidBatch 批次参数非法。
	ErrInvalidBatch = errors.New("govoucherpool: invalid batch parameters")

	// ErrCodeNotFound 摘要无法匹配到任何券（不回显入参）。
	ErrCodeNotFound = errors.New("govoucherpool: no voucher matches the provided code")
	// ErrVoucherNotFound 券 ID 查询无结果（不回显入参）。
	ErrVoucherNotFound = errors.New("govoucherpool: voucher not found")
	// ErrCodeAlreadyRegistered 券码摘要已登记（不回显入参）。
	ErrCodeAlreadyRegistered = errors.New("govoucherpool: code digest already registered")
	// ErrEmptyCode 券码为空。
	ErrEmptyCode = errors.New("govoucherpool: empty voucher code")

	// ErrVoucherUnavailable 券当前可用但无法满足本次预占之外的状态断言。
	ErrVoucherUnavailable = errors.New("govoucherpool: voucher is not available")
	// ErrVoucherHeld 券已被其他订单预占。
	ErrVoucherHeld = errors.New("govoucherpool: voucher is held by another order")
	// ErrHoldExpired 预占已到期。
	ErrHoldExpired = errors.New("govoucherpool: hold has expired")
	// ErrHoldMismatch 预占归属订单与请求不一致。
	ErrHoldMismatch = errors.New("govoucherpool: hold belongs to a different order")
	// ErrVersionMismatch 确认/释放携带的预占版本不是当前版本。
	ErrVersionMismatch = errors.New("govoucherpool: hold version is stale")
	// ErrVoucherRedeemed 券已核销，终态不可变更。
	ErrVoucherRedeemed = errors.New("govoucherpool: voucher already redeemed")
	// ErrVoucherVoided 券已作废，终态不可变更。
	ErrVoucherVoided = errors.New("govoucherpool: voucher already voided")

	// ErrInvalidArgument 通用参数错误。
	ErrInvalidArgument = errors.New("govoucherpool: invalid argument")
)
