# confirmations：订单的过程

订单在库存与信用两个参与方上的预占（装配项目协议 04，本组件是协调者）：确认、已确认订单的取消、完成时的释放，三种过程共用一张过程表与一个推进函数。

## 负责与不负责

- 负责：表 `confirmation`、`confirmation_branch`；路由 `POST /api/sales/orders/:id/confirm`、`POST /api/sales/orders/:id/cancel`、`GET /api/sales/orders/:id/confirmations/:confirmation_id`；后台工作 `advance-process`（`JobAdvance`）与它的死信；事件 `erp.sales_order.confirmation_ended.v1`。
- 不负责：订单在哪个阶段能确认、能取消，过程结束时订单怎样变（`orders` 的生命周期表，本模块按格行事、经 `orders.Transition` 写）；台账（`ledger`）；参与方自己的规则（`erp/inventory`、`erp/credit`）；交货消耗分支（`deliveries`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `confirmation.go` | 过程与分支的类型、读取（`readProcess`）；`ConfirmOrder`：事务外取证，T1 核对订单没变、写汇率快照、建过程与分支身份，订单进入确认中（RS09），同步预算内跟着推进（RS07）；`GetConfirmation` |
| `advance.go` | `advance`：一轮按状态向分支并行发命令（`send`），应答与下一状态在过程行的锁下决定（`step`、`decide` 是协议 04 的表）；`finish`：过程结束时按表的格（`ConfirmationConfirmed` 等、`CancelEnded`）改订单，在结束过程的同一个事务里（RS06；服务明细的可出账经 `ledger.ServiceConfirmed`）；`QueryBranch`；恢复工作项 `AdvanceJob` 与死信 `AdvanceDead` |
| `cancel.go` | `CancelOrder`：按表的 `Cancel` 一格行事（草稿立即取消、确认中经确认过程 RS04、已确认的查“没有交货与出账”后建 `cancel` 过程）；`StartBranchProcess`（`cancel`、`release` 过程，带着确认过程的分支身份） |

## 依赖与被依赖

- 依赖：`core`、`documents`、`ledger`、`masterdata`、`orders`。
- 被依赖：`billingsource`（`StartBranchProcess`）、`deliveries`（`Deref`、`ErrUnknown`：问参与方分支的结果不明）、`recoverycheck`（`QueryBranch`、状态常量）；入口登记路由与工作。

## 规则在哪

- 订单怎样变：`docs/design.md` 的“订单的状态”（SO03–SO11）；过程本身的状态与每一行转换：`docs/flows/confirmation.md`；装配项目协议 04（RS 编号）。
- 易错：推进的调用都在事务之外；结果未知时按同一分支身份查询或重发，不换身份。

## 测试

`itest/confirmations_integration_test.go`（`confirmations`：协议 04 的过程表逐行走，RS02–RS05、RS07–RS09、RS14）；参与方的替身（拒绝、丢一次应答、不可达、到期）在 `itest/standins_integration_test.go`。RS06（最后一次提交与本地确认之间崩溃）要 L3。
