# go-voucher-pool

一套支持「预占 — 确认」两阶段结算的兑换券核销服务（Go 库，并发安全，内存实现）。
适用于下单时先锁定兑换券、支付成功后再核销、支付失败或超时自动释放的业务场景。

## 特性

- **批次管理**：券批次记录适用范围（品类 / 门店）、面值、生效时间与失效时间。
- **券码不落明文**：登记时仅保存 `SHA-256(全局胡椒 ‖ 批次盐 ‖ 券码)` 摘要；
  每个批次使用独立随机盐，Store 启动时生成随机胡椒。查询视图、内部对象、
  错误信息中均不出现券码明文；摘要比较使用恒定时间比较。
- **四态状态机**：每张券任一时刻只处于 `可用 available` / `短期预占 reserved` /
  `已核销 redeemed` / `已作废 voided` 之一，后两者为终态。
- **幂等预占**：同一订单重复申请同一张券返回原预占（原版本、原到期时间）；
  不同订单争用同一张券，只有一个成功。
- **乐观版本 + TTL**：每次预占 / 释放 / 接管 / 作废都推进版本号；预占到期后
  可被释放或被别的订单接管，旧订单的迟到确认无法核销重新预占后的券。
- **原子核销入账**：确认必须携带当前预占版本；券状态与核销记录在同一临界区
  写入，面值最多计入订单一次。确认、主动释放、超时推进并发时终态唯一。
- **批次整体作废**：作废未核销的全部券（含预占中），已核销记录保留；
  被作废的预占不能再确认。

## 状态机

```
                 Reserve(orderA, ttl)
   available ───────────────────────────▶ reserved
       ▲          ◀───────────────────────────┼─────┐
       │            Release / AdvanceExpiry     │     │
       │            （到期释放，version+1）       │     │ Reserve
       │                                        │     │ （到期接管，
       │                                        │     │  version+1）
       │            Confirm(正确版本)            │     │
       └───────────────────────────▶ redeemed ◀──┘     │
       │                                               │
       └───────────────────────────▶ voided ◀──────────┘
                    VoidBatch（已核销的保留）
```

`redeemed` 与 `voided` 是终态，任何操作都不能将其改写。

## 快速开始

```go
package main

import (
	"context"
	"fmt"
	"time"

	gvp "github.com/chris64233/go-voucher-pool"
)

func main() {
	ctx := context.Background()
	store, _ := gvp.New()

	// 1. 创建批次：满 100 减 30，适用品类 coupon，门店 shop-1，有效期一天
	now := time.Now()
	batchID, _ := store.CreateBatch(ctx, gvp.CreateBatchInput{
		Scope:       gvp.Scope{CategoryIDs: []string{"coupon"}, ShopIDs: []string{"shop-1"}},
		FaceValue:   3000, // 最小货币单位（分）
		EffectiveAt: now,
		ExpiresAt:   now.Add(24 * time.Hour),
	})

	// 2. 生成并登记券码（明文只在调用方出现一次，服务端只存摘要）
	code, _ := gvp.GenerateCode()
	store.RegisterVouchers(ctx, batchID, []string{code})

	// 3. 下单结算：先预占 5 分钟
	res, err := store.Reserve(ctx, gvp.ReserveInput{
		BatchID: batchID,
		OrderID: "order-1001",
		Codes:   []string{code},
		TTL:     5 * time.Minute,
	})
	if err != nil {
		// ErrVoucherBusy / ErrVoucherRedeemed / ErrVoucherVoided / ErrBatchNotInEffect ...
		panic(err)
	}
	version := res.Items[0].Version // 保存版本号，确认时必须原样带回

	// 4a. 支付成功：携带版本确认核销（返回核销记录）
	rec, err := store.Confirm(ctx, batchID, "order-1001", code, version)
	fmt.Println(rec.FaceValue) // 3000

	// 4b. 支付失败：主动释放；不释放也会在 TTL 后由 AdvanceExpiry 回收
	// _ = store.Release(ctx, batchID, "order-1001", code)
}
```

## API 一览

所有方法接受 `context.Context`，错误均为包级哨兵错误，可用 `errors.Is` 判定。

| 方法 | 说明 |
| --- | --- |
| `New(opts ...Option)` | 创建并发安全的内存存储；可选 `WithClock(func() time.Time)` 注入时钟 |
| `GenerateCode()` | 生成密码学随机、URL 安全的券码明文（192 bit 熵） |
| `CreateBatch` | 创建批次（适用范围、面值、生效 / 失效时间），返回批次 ID |
| `RegisterVouchers` | 按券码明文登记券，仅存摘要，返回券 ID；同批次重复登记报错 |
| `Reserve` | 按订单预占一张或多张券（整批原子），返回每张券的预占版本与到期时间 |
| `Confirm` | 携带预占版本确认核销，原子写入核销记录并累加订单面值 |
| `Release` | 订单主动释放自己的预占 |
| `AdvanceExpiry` | 过期推进：把所有到期预占释放回可用池，幂等 |
| `VoidBatch` | 批次整体作废；已核销的保留，其余转入已作废 |
| `GetBatch` / `GetVoucher` / `ListVouchers` | 批次、单券（凭券码）、批次列表查询 |
| `GetOrder` | 订单维度查询：未到期预占、已核销券、面值合计、核销记录 |
| `GetRedemption` | 按券 ID 查询核销记录 |

## 关键语义

### 预占幂等与争用

- 同一订单对同一张券在预占未到期时重复 `Reserve`：返回原预占，
  `Reused=true`，版本与到期时间不变（不续期），结算侧可安全重试。
- 不同订单同时 `Reserve` 同一张券：恰好一个成功，其余得到 `ErrVoucherBusy`。
- 多张券一次预占是**整批原子**的：其中任意一张不可用，整单失败且不留任何预占。

### 到期、接管与迟到确认

- 预占到期时间会被截断到批次失效时间（不会预占出一张批次已失效的券）。
- 到期是惰性 + 主动推进结合：`AdvanceExpiry` 批量回收；`Confirm` 遇到自己
  到期的预占会顺带释放。
- 到期后其他订单可以 `Reserve` 接管，接管产生**新版本号**。旧订单的迟到
  确认会因「券属于其他订单 (`ErrWrongOrder`)」或「版本不符 (`ErrVersionMismatch`)」
  被拒绝，不可能核销后来重新预占的券。

### 终态唯一与面值不重复计入

- 全部状态流转在同一把 `sync.RWMutex` 写锁内完成；`Confirm` 把
  `券 → redeemed`、核销记录、订单面值合计放在**同一临界区**写入。
- 重复 `Confirm` 已核销券得到 `ErrVoucherRedeemed`，不会二次入账。
- 确认 / 释放 / 过期推进并发作用于同一张券时，券只会落入一个终态，
  订单 `TotalFaceValue` 与核销记录严格一致。

### 批次作废竞争

- `VoidBatch` 后批次不可再登记、预占；批次内非终态券立即作废
  （预占中的券同步摘除订单属主并推进版本）。
- 已经 `Confirm` 的核销与核销记录原样保留，订单面值合计不变。
- 作废与确认竞争时只有两种结果：确认先落盘则保留核销；作废先落盘则确认
  得到 `ErrVoucherVoided` / `ErrBatchVoided`，不存在越过作废的确认。

## 错误码

| 哨兵错误 | 触发场景 |
| --- | --- |
| `ErrInvalidArgument` | 参数为空、面值非正、时间窗口非法、入参券码重复等 |
| `ErrBatchNotFound` / `ErrVoucherNotFound` | 批次或券不存在（错误文本不含券码） |
| `ErrBatchVoided` | 批次已作废后再登记 / 预占 / 重复作废 |
| `ErrVoucherVoided` | 对已作废券预占 / 确认 / 释放 |
| `ErrVoucherRedeemed` | 对已核销券预占 / 确认 / 释放 |
| `ErrVoucherBusy` | 券被其他订单未到期地预占 |
| `ErrVoucherNotReserved` | 对可用券确认 / 释放 |
| `ErrReservationExpired` | 确认时预占已到期（已顺带释放） |
| `ErrVersionMismatch` | 确认携带的版本不是当前版本 |
| `ErrWrongOrder` | 操作非本订单持有的预占 |
| `ErrBatchNotInEffect` | 批次未生效或已过失效时间 |
| `ErrCodeAlreadyRegistered` | 同批次券码重复登记 |
| `ErrOrderNotFound` / `ErrRedemptionNotFound` | 查询对象不存在 |

## 安全说明

- 券码明文只在 `RegisterVouchers` / `Reserve` / `Confirm` / `Release` /
  `GetVoucher` 的入参中短暂出现，用于现场计算摘要，不写入任何结构体字段、
  索引、日志或错误字符串。
- 摘要 = `SHA-256(pepper ‖ salt ‖ code)`：每批次独立 128 bit 盐，
  每个 Store 实例独立 256 bit 胡椒；摘要索引使用其 hex 编码。
- 券码由 `crypto/rand` 生成（192 bit），默认输出 Base64URL（约 32 字符）。
- 本实现为**内存存储**，进程重启数据不保留，适合作为库 / 教学 / 单实例
  服务内核；多实例持久化场景可将同一组临界区操作映射到数据库事务
  （`SELECT ... FOR UPDATE` 或唯一约束 + 条件更新）。

## 测试

```bash
go test -race -count=1 ./...
```

测试覆盖（见 `store_test.go`）：

- 批次参数校验、生效窗口、预占到期被批次失效时间截断；
- 券码重复登记、未知券码 / 批次的错误判定；
- 同订单预占幂等（版本、到期时间不变）、跨订单争用；
- 确认成功写入核销记录、错误版本拒绝、重复确认不重复入账；
- 到期释放、到期接管、旧订单迟到确认（无人接管 / 被他人接管两种情形）；
- 主动释放、释放后旧版本失效；
- 批次作废保留已核销、作废预占不可确认、作废后禁止登记与预占；
- 32 路并发预占恰一个赢家；60 路确认 / 作废竞争终态唯一且面值恰好一次；
  45 路确认 / 释放 / 过期推进竞争终态唯一；
- 错误信息、对外视图与内部对象均不泄漏券码明文。
