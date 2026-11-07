# billingsource：出账来源

`erp/billing` 认领订单行的可出账额度（装配项目协议 05 的认领模式，本组件是额度的属主）；另有给任何组件的订单读取。都在系统面（gRPC）。

## 负责与不负责

- 负责：表 `billing_claim`、`billing_claim_line`；`BillingSourceService` 的 `Ledger`、`Claim`、`Renew`、`Confirm`、`Formalize`、`Release`（只许 `erp/billing`）与 `Reverse`（本版不做）；`OrderService.Read`；每分钟的 `expire-claims`；全部交货并出账时完成订单、释放剩下的分支。
- 不负责：订单在哪个阶段能被认领、什么时候完成（`orders` 的表，`Claim`、`Complete` 两格，SO13、SO14）；台账列怎样改（`ledger`）；出账单本身（`erp/billing`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `billing.go` | 认领的读取与行的核对（`claimLines`）；`Claim` 在订单行锁下查表 `Claim`，不允许时答格里的问题 `order_not_billable`；`claimTx`（先锁订单行，再锁认领）、`giveBack`；每个 gRPC 方法一个函数；`completeIfDone`（查表 `Complete`：全部交完并出账时订单完成、启动释放）；`Ledger` 把订单状态原样交给 `erp/billing`（登记在 `sales/structure_test.go` 的 `orderStateReaders`）；`ExpireClaims`；`Read` |

gRPC 的方法在入口 `sales/services.go` 转到这里。

## 依赖与被依赖

- 依赖：`orders`（`PhaseOf`、`Decide`、`Transition`）、`ledger`（`Claim`、`GiveBack`、`Bill`）、`confirmations`（`StartBranchProcess`：完成时释放）、`core`、`documents`。
- 被依赖：只有入口。

## 规则在哪

- `docs/flows/delivery-and-billing.md`；装配项目协议 05（BL 编号）；`claimed + billed ≤ billable` 两维各自成立，表上的 CHECK 兜底。

## 测试

`itest/billingsource_integration_test.go`（`billingSource`：BL01、BL02、BL24、BL05/BL26，两张认领抢同一行，正式化完成订单并释放分支，只有 `erp/billing` 能调）。`order_not_billable`（未确认、已完成、取消中的订单被认领）在 `itest/lifecycle_integration_test.go`。
