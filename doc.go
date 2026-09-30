// Package govoucherpool implements a voucher redemption service with a
// reserve -> confirm/release -> expire -> void lifecycle. Voucher codes are
// stored only as salted digests, and each voucher is always in exactly one
// of the available, held, redeemed, or voided states.
package govoucherpool
