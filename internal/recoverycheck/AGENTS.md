# recoverycheck：整套恢复后的预占核对

整套恢复（装配项目协议 15 第 8 步）之后协议 04 的检查 `erp.sales.reservations`。本组件是协调者，替两边报告（线协议 P14.2）：自己的过程对照参与方的分支。

## 负责与不负责

- 负责：恢复点上还在进行的确认、取消、释放过程与出库中的交货列为差异；每个订单最近一个已结束的过程：已确认的分支在参与方应为已提交或已消耗，已拒绝、已取消（最近 30 天内结束的）的分支不应还占着；问不到参与方答“无法证明”。
- 不负责：对象“还在不在”的核对（入口 `sales/checks.go` 的 `Presence`）；恢复工具本身（`be-ops`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `recoverycheck.go` | `CheckReservations`（检查名）、`ReservationsCheck`（本库在回滚的事务里读，问参与方在事务之外）、`expect`（过程状态 → 分支应有的状态）、`collectDiffs` |

## 依赖与被依赖

- 依赖：`confirmations`（`QueryBranch`，与过程推进共用；过程与分支状态的常量）、`core`。
- 被依赖：入口 `sales/checks.go`。

## 规则在哪

装配项目协议 04 的恢复一节、协议 15；线协议第 14 章；`docs/design.md` 实现状态的 3.0.27。

## 测试

`itest/recoverycheck_integration_test.go`（`recoveryChecks`：四种对象的 `Presence`，与本检查在恢复之后的答复）。
