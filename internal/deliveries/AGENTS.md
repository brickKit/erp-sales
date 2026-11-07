# deliveries：交货

已确认订单的交货：T1 建交货（出库中），库存出库并消耗订单的分支（交货身份就是消耗身份），T2 过账并增长可出账额度。

## 负责与不负责

- 负责：表 `delivery`、`delivery_line`；路由 `/api/sales/deliveries` 的列表、新建、读取；后台工作 `JobIssue`（出库的恢复，重试到库存应答为止）；事件 `erp.delivery.changed.v1`、`erp.delivery.posted.v1`。
- 不负责：订单在哪个阶段能交货（`orders` 的表，`Deliver` 一格，SO12）；已交货与可出账怎样增长（`ledger.Deliver`）；库存怎样出库（`erp/inventory`）；认领（`billingsource`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `delivery.go` | `CreateDelivery`（查表 `Deliver`；数量不超过订单数量减已交货、减交货中）、`GetDelivery`；`issue`（调库存）、`post`（过账：先写交货的行与事件，再 `ledger.Deliver`）；`IssueJob`；`ListDeliveries` 与列表的定义 `deliveryList`（自己的列：`order_id`、`legal_entity_id`、`state` 可筛选，按 `created_at` 排序；筛选与编译用 `documents.Filters`、`Build`） |

## 依赖与被依赖

- 依赖：`orders`（`PhaseOf`、`Decide`）、`confirmations`（`Deref`、`ErrUnknown`）、`core`、`documents`、`ledger`、`masterdata`。
- 被依赖：只有入口登记路由与工作。

加列表字段：`deliveryList` 加列，`internal/core/names.go` 的 `Definitions()` 里交货资源的字段加上它，`contract/openapi.yaml` 加参数。

## 规则在哪

- `docs/flows/delivery-and-billing.md`；可出账金额：`docs/rules/pricing.md` 的“可出账金额”；交货身份即消耗身份：协议 04 的 RS19。

## 测试

`itest/deliveries_integration_test.go`（`deliveries`：RS19；可出账按舍入增长、最后一次补齐；库存拒绝则交货失败，没应答的稍后过账）。
