# 记账规则、幂等与结算/对账语义

## 1. 复式记账规则

### 1.1 金额与币种

- 金额一律使用**最小币种单位的整数**（`domain.Money.Amount int64`），不使用浮点。
- 每个交易有且只有一个币种；账户记录自己的币种。分录命中账户的币种必须与交易币种一致，
  否则返回 `CURRENCY_MISMATCH`。
- 分录金额必须 `> 0`，否则返回 `INVALID_AMOUNT`。

### 1.2 借贷方向（资金账户视角）

| 方向 | 含义 | 对账户余额影响 |
|------|------|----------------|
| `Debit`（借）  | 资金流出 | 余额减少 |
| `Credit`（贷） | 资金流入 | 余额增加 |

- 一笔交易的**借方合计必须等于贷方合计**，否则整笔拒绝（`INVALID_ARGUMENT`）。
- 支付受理：借 `收单过渡账户`，贷 `商户账户`。
- 资金划转：借 `付款方`，贷 `收款方`。
- 手续费：借 `商户账户`，贷 `手续费账户`。
- 对账调整（以外部为准）：
  - 本地少记：借 `挂账账户(suspense)`，贷 `商户账户`；
  - 本地多记：借 `商户账户`，贷 `挂账账户`。

### 1.3 原子性（不允许半截状态）

`storage.DB` 用**单把互斥锁**保护全部可变状态。`ledger.Service.Post` 与
`ledger.Service.Atomic` 在同一临界区内执行，并严格遵循"**两阶段：先完整校验/预演，
后统一提交**"：

1. 交易号去重；
2. 逐条校验账户存在、币种一致、金额为正，汇总每个账户净变动；
3. 校验借贷平衡；
4. **预演**每个账户变更后余额，任一将为负则返回 `INSUFFICIENT_FUNDS`，状态完全不变；
5. 全部通过后才更新余额（账户版本号 `Version++`）并落库交易。

因此一笔划转要么整体成功要么整体失败，绝不会"只扣款不入账"。全局锁串行化所有提交，
配合"提交前预演"，保证并发划转时**余额恒不为负、无丢失更新**（见
`internal/ledger/service_test.go` 的两个并发测试）。

交易状态：`PENDING`（已受理待结算）→ `SETTLED`（已结算）。手续费与对账调整交易
生成时即为 `SETTLED`。

## 2. 支付受理与幂等语义

入口：`payment.Service.Accept(ctx, Request)`，`Request.IdemKey` 必填。

对幂等键绑定的请求内容（交易号、收款账户、金额、币种）计算 SHA-256 摘要：

| 情形 | 行为 | 错误/结果 |
|------|------|-----------|
| 新键 | 执行复式记账，登记 `DONE`/交易号 | 成功，`Replayed=false` |
| 同键 + 相同内容，已成功 | 重放首次交易号，**不再次扣款** | `Replayed=true` |
| 同键 + **不同内容** | 明确拒绝 | `IDEM_CONFLICT` |
| 首次业务失败 | 登记 `FAILED` + 原始错误码，不产生交易/余额变更 | 具体错误码 |
| 同键重试失败请求 | 重放首次失败码，**不重新执行** | 同一错误码 |

可区分的失败码（`internal/apperr`）：

- `INSUFFICIENT_FUNDS` 余额不足；
- `CURRENCY_MISMATCH` 币种不一致；
- `ACCOUNT_NOT_FOUND` 账户不存在；
- `INVALID_AMOUNT` 金额非法；
- `INVALID_ARGUMENT` 其他参数非法；
- `DUPLICATE_TRANSACTION` 交易号重复；
- `IDEM_CONFLICT` 同幂等键内容冲突；
- `IDEM_MISSING` 幂等记录处于异常的 IN_FLIGHT 状态（单机同步模型下仅理论可能）。

> 实现细节：先以 `IN_FLIGHT` 占位幂等键，业务执行后再回写终态。由于记账调用本身
> 是同步且加锁的，正常路径不会观测到 `IN_FLIGHT`；冲突判定只比较请求内容摘要。

## 3. 结算语义

入口：`settlement.Service.SettleBatch(batchID, txnIDs, rules, at)`。

- 手续费按币种配置 `FeeRule{FeeFixed, FeeBps}`：
  `fee = FeeFixed + floor(amount * FeeBps / 10000)`（1bp = 0.01%）。
- 整批在一个 `Atomic` 临界区内完成：
  1. 批次号未结算；
  2. 每笔交易存在且为 `PENDING`，且未在任何批次结算过；
  3. 手续费账户币种已配置；
  4. **按商户汇总整批手续费并预演余额**，余额不足返回 `INSUFFICIENT_FUNDS`；
  5. 全部通过后：逐笔生成即时 `SETTLED` 的 `FEE` 交易、原交易转 `SETTLED`、
     登记批次（含按币种的手续费汇总）。
- 任一笔失败 → 整批不变（无手续费交易、无状态流转、批次不落库），修正后可原批次号重试。
- 幂等：同一批次重复结算返回 `BATCH_SETTLED`；同一交易换批次结算同样被拒绝，
  **不会重复扣手续费**。

## 4. 对账语义

入口：`reconciliation.Service.Reconcile(key, localTxnIDs, remote, suspenseAcct)`。

### 4.1 差异类型

| 类型 | 判定 |
|------|------|
| `MISSING_LOCAL`  | 外部账单有、本地无此交易号 |
| `MISSING_REMOTE` | 本地有、外部账单无（单边缺失） |
| `AMOUNT_MISMATCH`| 同交易号金额或币种不一致 |
| `DUPLICATE`      | 外部侧同一交易号多条，或本地请求中重复传入同一交易号 |

只允许对**已结算**交易对账；存在 `PENDING` 交易返回 `SETTLEMENT_PENDING`。

### 4.2 自动调整

- 对"本地存在 + 金额不符 + 币种一致"的差异，以**外部金额为准**生成一笔
  `SETTLED` 的 `ADJUSTMENT` 复式交易（商户 ↔ 挂账账户），调整交易号为
  `ADJ-<reconKey>-<txnID>`。
- 因余额不足无法入账时：**保留差异、不强行调整**，差异备注中标注 skipped 原因。
- 币种不一致只报差异，不自动调整（需人工处理）。
- 单边缺失、重复记录只报告，不自动改账。
- 所有调整与差异登记在同一原子临界区内完成。

### 4.3 对账幂等（不重复调整）

- 同一对账 `key` 重复执行：直接重放首次差异报告，`Adjusted=false`，**不再生成调整**。
- 即使换一个对账 `key`，只要其中任一本地交易已在历史对账中被覆盖，整次拒绝
  （`BATCH_SETTLED`），从两个维度杜绝重复调整。

## 5. 验证方法

```bash
# 全流程演示（受理/幂等重放/划转/结算收费/对账差异/对账重放）
go run ./cmd/demo

# 全量单测（-v 日志中打印幂等键或交易号 + 判定依据）
go test ./... -v

# 竞态 + 重复多次，验证并发划转稳定性
go test -race -count=5 ./internal/ledger/
go test -race ./...
```

关键测试与场景对应：

| 场景 | 测试 |
|------|------|
| 幂等重复提交不重复扣款 | `payment.TestAccept_IdempotentReplay` |
| 同键不同内容被拒绝 | `payment.TestAccept_SameKeyDifferentContent` |
| 失败原因可区分且失败也幂等 | `payment.TestAccept_FailureCausesDistinguishable` |
| 并发划转余额不为负 | `ledger.TestPost_ConcurrentTransfers` |
| 并发划转无丢失更新 | `ledger.TestPost_ConcurrentNoLostUpdate` |
| 划转失败整体回滚 + 原因区分 | `ledger.TestPost_FailureAtomicityAndCauses` |
| 批次原子 / 手续费 / 批次幂等 | `settlement.TestSettleBatch_*` |
| 四类对账差异 + 自动调整 | `reconciliation.TestReconcile_AllDiffTypes` |
| 对账重放不重复调整 | `reconciliation.TestReconcile_IdempotentReplay` |
| 跨对账键重复覆盖被拒绝 | `reconciliation.TestReconcile_OverlapDifferentKey` |
| 正常路径端到端不回归 | `e2etest.TestEndToEnd_HappyPath` |
