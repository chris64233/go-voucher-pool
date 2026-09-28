package govoucherpool

import "errors"

// 哨兵错误统一不携带券码明文、摘要、券 ID 以外的敏感信息，
// 调用方可以安全地把错误文本返回给上游或记入日志。
var (
	// ErrInvalidArgument 请求参数非法（空值、时间窗口非法等）。
	ErrInvalidArgument = errors.New("voucher: invalid argument")
	// ErrBatchNotFound 批次不存在。
	ErrBatchNotFound = errors.New("voucher: batch not found")
	// ErrBatchVoided 批次已经整体作废。
	ErrBatchVoided = errors.New("voucher: batch already voided")
	// ErrVoucherNotFound 券不存在（券码错误或不属于该批次）。
	ErrVoucherNotFound = errors.New("voucher: voucher not found")
	// ErrVoucherVoided 券已经作废（随批次整体作废或单独作废）。
	ErrVoucherVoided = errors.New("voucher: voucher voided")
	// ErrVoucherRedeemed 券已经核销。
	ErrVoucherRedeemed = errors.New("voucher: voucher already redeemed")
	// ErrVoucherBusy 券正被其他订单短期预占。
	ErrVoucherBusy = errors.New("voucher: voucher reserved by another order")
	// ErrVoucherNotReserved 释放/确认一张未被预占的券。
	ErrVoucherNotReserved = errors.New("voucher: voucher is not reserved")
	// ErrReservationExpired 预占已经到期（需重新预占后再确认）。
	ErrReservationExpired = errors.New("voucher: reservation expired")
	// ErrVersionMismatch 携带的预占版本不是当前版本（典型场景：旧订单迟到确认）。
	ErrVersionMismatch = errors.New("voucher: reservation version mismatch")
	// ErrWrongOrder 预占属于其他订单，当前订单无权操作。
	ErrWrongOrder = errors.New("voucher: reservation belongs to another order")
	// ErrBatchNotInEffect 批次尚未生效或已经超过失效时间。
	ErrBatchNotInEffect = errors.New("voucher: batch is not within its effective window")
	// ErrCodeAlreadyRegistered 券码在同一批次或其他批次中已经登记过（去重）。
	ErrCodeAlreadyRegistered = errors.New("voucher: code already registered")
	// ErrOrderNotFound 订单没有任何预占或核销记录。
	ErrOrderNotFound = errors.New("voucher: order not found")
	// ErrRedemptionNotFound 券没有核销记录。
	ErrRedemptionNotFound = errors.New("voucher: redemption not found")
)
