# ledger：可出账台账的事件（共用）

订单行的可出账台账（可出账、认领中、已出账，数量与金额两维）每次变化后发 `erp.billable.changed.v1`，带变化之后的台账。

## 负责与不负责

- 负责：`LedgerFacts`，在改台账的同一个事务里写发件表。
- 不负责：台账怎样变（`deliveries` 的过账、`billingsource` 的认领、`confirmations` 的 `finish`），它们改完调这里。

## 文件

| 文件 | 内容 |
| --- | --- |
| `ledger.go` | `LedgerFacts` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`confirmations`、`deliveries`、`billingsource`。

## 规则在哪

装配项目协议 05（本组件是可出账额度的属主）；事件的 schema 在 `contract/events/`。

## 测试

`itest/outbox_integration_test.go`（每个事件合乎 schema、每种类型都发过）；台账的数在 `itest/deliveries_integration_test.go`、`itest/billingsource_integration_test.go`。
