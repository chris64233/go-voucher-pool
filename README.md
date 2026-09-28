# go-voucher-pool

支持「预占 → 确认/释放 → 超时推进 → 批次作废」完整生命周期的兑换券核销服务（Go，零第三方依赖）。

## 核心能力

| API | 说明 |
|---|---|
| `NewService(clock)` | 创建服务；可注入时钟便于测试 |
| `CreateBatch` | 创建券批次：适用范围、面值（最小货币单位 `int64`）、生效/失效时间 |
| `RegisterVoucher` | 在批次下登记券码；明文只用于当场计算摘要，不落库 |
| `Reserve(orderID, code, ttl)` | 按订单预占；TTL 自动截断到批次失效时间 |
| `Confirm(orderID, code, version)` | 携带预占版本确认核销，核销记录同事务写入 |
| `Release(orderID, code, version)` | 持版本主动释放 |
| `ExpireHolds()` | 批量推进所有到期预占 |
| `VoidBatch(batchID, reason)` | 批次整体作废 |
| `GetBatch` / `GetVoucher` / `OrderRedemptions` / `OrderRedeemedValue` | 批次、券状态、订单核销记录与面值合计查询 |

## 状态机

每张券在任一时刻只处于四个互斥状态之一：

```
                 Reserve            Confirm(版本匹配、未到期)
 available ───────────────► held ───────────────────────────► redeemed
    ▲                         │  │                                ▲
    │      Release/Expire     │  └─ VoidBatch（抢占未完成预占）    │
    └─────────────────────────┘                                   │
                              └────► voided ◄─────────────────────┘
                                    VoidBatch（available 直接作废；
                                    redeemed 终态保留，不被作废）
```

- `redeemed`、`voided` 为终态，任何后续操作返回对应错误。
- 每次**发放新预占**都会分配单调递增的 `version`（同单重复申请返回原预占，不递增）。

## 关键语义

1. **预占幂等与争用**：同一订单对同一券在 TTL 内重复 `Reserve` 返回原预占（版本、到期时间均不变）；不同订单争用时，未到期的持有者获胜，其余得到 `ErrVoucherHeld`。
2. **版本栅栏（防迟到确认）**：预占到期被释放/回收后，其他订单可重新预占并取得新版本；旧订单持旧版本迟到确认时，按「订单归属 / 版本号 / 到期时间」顺序校验，必然被拒（`ErrHoldMismatch` / `ErrVersionMismatch` / `ErrHoldExpired`），不可能核销已被重新预占的券。
3. **核销原子性与面值不重复**：券状态翻转为 `redeemed` 与核销记录、订单面值入账在同一临界区内完成；重复确认同单同版本幂等返回同一条 `Redemption`，订单面值只计一次。
4. **确认/释放/超时并发唯一终态**：全部操作经同一把写锁串行化状态转移；同一版本下确认与释放先到先得，只产生一个终态。
5. **批次作废竞争**：作废前先推进到期预占；`redeemed` 保留（含面值），其余状态（含预占中的券）一律置为 `voided` 并清空持有信息，未完成预占无法越过作废继续确认；作废幂等可重复调用。
6. **双重时间校验**：预占到期时间截断到批次失效点；确认时同时校验预占 TTL 与批次有效期。

## 券码安全

- 服务启动生成 32 字节随机 salt，仅保存 `sha256(salt ‖ code)` 摘要；明文不进入任何字段、错误或日志。
- 对外状态视图 `Snapshot` 既无明文也无摘要；错误信息为固定文案，不回显调用方传入的券码或摘要（防止枚举）。
- 业务方应只暴露服务端生成的 `VoucherID`，券码由用户在核销链路一次性提交。
- 注意：摘要机制保护的是「落库泄露」场景；salt 为实例级随机值，服务重启后旧摘要失效。持久化部署时应把 salt 放入密钥管理并改为按批次/券盐存储（可在此结构上直接扩展）。

## 快速上手

```go
svc, _ := govoucherpool.NewService(nil)

batch, _ := svc.CreateBatch(govoucherpool.CreateBatchInput{
    Name:        "满100减20",
    FaceValue:   2000, // 以最小货币单位计
    Scope:       govoucherpool.Scope{SKUs: []string{"sku-1"}},
    EffectiveAt: time.Now(),
    ExpiresAt:   time.Now().Add(24 * time.Hour),
})

voucherID, _ := svc.RegisterVoucher(batch.ID, "USER-ENTERED-CODE")

hold, err := svc.Reserve("order-123", "USER-ENTERED-CODE", 5*time.Minute)
if err != nil { /* ErrVoucherHeld / ErrBatchNotInEffect / ... */ }

// 结算完成：必须带回预占版本
rec, err := svc.Confirm("order-123", "USER-ENTERED-CODE", hold.Version)

// 或放弃：Release("order-123", "USER-ENTERED-CODE", hold.Version)

_ = svc.GetVoucher(voucherID)           // 脱敏状态快照
svc.OrderRedeemedValue("order-123")     // => 2000
svc.ExpireHolds()                       // 定时任务推进到期预占
svc.VoidBatch(batch.ID, "运营下架")      // 批次整体作废
```

## 测试

```bash
go test -race -count=1 ./...
```

覆盖内容：

- 批次参数校验、生效窗口、TTL 截断到批次失效时间；
- 券码查重、明文不落库、错误/快照不泄漏；
- 同单幂等预占、异单争用唯一赢家、到期重占版本递增；
- 旧订单迟到确认（释放周期、并发场景）一律拒绝；
- 确认 vs 释放、重复确认、作废 vs 预占/确认等并发不变量（`-race` 下多轮重复）：终态唯一、核销数守恒、面值与核销集合一致。
