# ledger：可出账台账（共用）

订单行的可出账台账（装配项目协议 05，本组件是额度的属主）：每行、每维（数量、金额）的可出账、认领中、已出账，以及它增长的来源已交货量；不变量 `认领中 + 已出账 ≤ 可出账`（表上的 CHECK 兜底）。**这几列只经这里的五个函数改**，每个函数改完在同一事务里发 `erp.billable.changed.v1`。

## 负责与不负责

- 负责：`Claim`（新认领加进认领中）、`GiveBack`（认领释放或到期，从认领中退回）、`Bill`（正式出账：认领中转已出账）、`Deliver`（交货过账：已交货与可出账增长，金额按舍入、最后一次补齐）、`ServiceConfirmed`（服务订单确认：服务明细整行可出账）；`LedgerFacts`。
- 不负责：什么时候可以认领、交货（订单的生命周期，`orders`）；认领本身的表（`billingsource`）；交货本身（`deliveries`）。调用方先锁订单行。

## 文件

| 文件 | 内容 |
| --- | --- |
| `ledger.go` | 五个写入函数与 `Move`；`LedgerFacts` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`billingsource`（`Claim`、`GiveBack`、`Bill`）、`deliveries`（`Deliver`）、`confirmations`（`ServiceConfirmed`）。

## 规则在哪

装配项目协议 05（BL 编号）；`docs/flows/delivery-and-billing.md` 的台账；`docs/rules/pricing.md` 的“可出账金额”；事件的 schema 在 `contract/events/`。

## 测试

- 单元：`sales/structure_test.go`（别处的 SQL 不写这几列）。
- 集成：`itest/deliveries_integration_test.go`、`itest/billingsource_integration_test.go`（台账的数）、`itest/outbox_integration_test.go`（事件合乎 schema）。
