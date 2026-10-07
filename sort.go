package govoucherpool

import "sort"

// sortRedemptions 按券 ID 稳定排序核销记录。
func sortRedemptions(rs []Redemption) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].VoucherID < rs[j].VoucherID })
}

// sortRedeemRecords 按券 ID 稳定排序兑换核销记录。
func sortRedeemRecords(rs []RedeemRecord) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].VoucherID < rs[j].VoucherID })
}
