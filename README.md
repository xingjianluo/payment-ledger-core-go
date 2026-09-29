# payment-ledger-core-go

本地支付账本核心：用**本地内存存储**模拟支付受理、复式记账、结算与对账的完整闭环，
**不依赖任何外部支付服务或网络接口**。零第三方依赖，仅使用 Go 标准库。

## 能力一览

| 环节 | 保证 |
| --- | --- |
| 支付受理 | 幂等键 + 请求指纹：重复提交不重复扣款；同键不同内容明确拒绝；并发同键只产生一笔 |
| 记账 | 复式记账（借贷平衡、币种一致）；一笔业务在单临界区内原子过账，无半截状态；余额不得为负 |
| 并发 | 全部变更串行化在存储互斥临界区内（相当于数据库事务），并发划转不透支、不丢失更新 |
| 结算 | 待结算支付按批次结算；手续费逐笔计提；结算幂等 |
| 对账 | 识别单边缺失（双侧）、金额不符、重复记录；可自动调整的差异挂暂记科目；同批次重复执行不产生重复调整 |
| 自检 | `TrialBalance()` 试算平衡，任何路径后借方合计 == 贷方合计 |

## 目录结构

```
internal/
  domain/          领域模型：账户/凭证/支付/结算批次/对账差异、错误码、金额类型
  storage/         内存存储 + Update/View 事务临界区（无网络依赖）
  ledger/          复式记账引擎：开户、入金、通用过账、划转、试算平衡
  payment/         支付受理：幂等键状态机（IN_FLIGHT/DONE）+ 请求指纹
  settlement/      结算批次：建批、结算（结算凭证 + 手续费凭证）
  reconciliation/  对账：差异分类、暂记自动调整、批次级幂等、对账单来源(内存/CSV)
cmd/demo/          端到端演示
docs/              记账规则、幂等与结算语义说明
```

## 快速验证

```bash
# 全部自动化测试（带竞态检测、重复 3 次保证稳定）
go test -race -count=3 ./...

# 只跑关键场景并查看含幂等键/交易号与判定依据的日志
go test ./internal/payment/        -v -run 'Idempotent|Concurrent'
go test ./internal/ledger/         -v -run 'Concurrent|Insufficient'
go test ./internal/reconciliation/ -v
go test ./internal/settlement/     -v

# 端到端本地演示
go run ./cmd/demo
```

## 典型用法

```go
st := storage.New()
led := ledger.New(st, nil)
paySvc := payment.New(st, led, nil)
settleSvc := settlement.New(st, led, domain.FeePolicy{BPS: 60}, nil)
reconSvc := reconciliation.New(st, led, nil)

// 1) 开户（钱包为负债账户）与入金
led.OpenAccount("cust-1", "客户1钱包", "CNY", domain.AccountTypeLiability, false)
led.Deposit("cust-1", 1_000_000, "DEP-1")

// 2) 受理支付（幂等键必填）
res, err := paySvc.Accept(&domain.PaymentRequest{
    IdempotencyKey: "IDEM-1001",
    PayerAccountID: "cust-1", PayeeAccountID: "merch-1",
    Amount: 12000, Currency: "CNY", Reference: "订单1001",
})
// 重复提交相同请求 => 返回同一个支付号，Replayed=true，不重复扣款
// 相同幂等键但金额/账户/币种不同 => ErrIdempotencyConflict

// 3) 批次结算（0.60% 手续费）
batch, _ := settleSvc.CreateBatch("CNY", []string{res.Payment.ID})
settleSvc.SettleBatch(batch.ID)

// 4) 对账（对账单来源可由内存或本地 CSV 构造）
src := reconciliation.NewMemorySource()
src.Add(batch.ID, domain.StatementRecord{TxnID: res.Payment.ID, Amount: 12000, Currency: "CNY"})
run, _ := reconSvc.Reconcile(batch.ID, src)
```

## 错误码（失败原因可区分）

`INVALID_ARGUMENT` / `ACCOUNT_NOT_FOUND` / `ACCOUNT_EXISTS` / `CURRENCY_MISMATCH` /
`INSUFFICIENT_FUNDS` / `UNBALANCED_ENTRY` / `IDEMPOTENCY_CONFLICT` /
`PAYMENT_NOT_FOUND` / `PAYMENT_DUPLICATE` / `BATCH_NOT_FOUND` /
`BATCH_ALREADY_SETTLED` / `RECON_ALREADY_RUN` / `INTERNAL`

用 `domain.IsErrorCode(err, domain.ErrInsufficientFunds)` 判定具体原因。

记账规则、幂等语义、结算/对账语义的详细说明见 [docs/ACCOUNTING.md](docs/ACCOUNTING.md)。
