# internal/：业务模块

销售的业务，每个模块一个包。只给本仓库引用（Go 的 `internal`）：别的组件编译期就引用不了，跨组件只走契约（决定 0101）。

先按下表找到模块，再读那个目录的 `AGENTS.md`。

## 模块

| 包 | 一句话 | 依赖（本组件内） |
| --- | --- | --- |
| `quotations` | 报价：草稿、发出、取消、接受成订单草稿 | `core`、`documents`、`masterdata` |
| `orders` | 订单草稿；订单状态的写入处（`SetOrderState`）；已结束订单的冷热分层 | `core`、`documents` |
| `confirmations` | 订单的过程（确认、取消、释放）与推进 | `core`、`documents`、`ledger`、`masterdata`、`orders` |
| `deliveries` | 交货、出库、过账 | `confirmations`、`core`、`documents`、`ledger`、`masterdata` |
| `billingsource` | 出账来源（协议 05）与订单读取 | `confirmations`、`core`、`documents`、`ledger`、`orders` |
| `pricing` | 价格表；一行取哪个价 | `core`、`masterdata` |
| `orgunits` | 部门投影（协议 02） | `core`、`masterdata` |
| `recoverycheck` | 整套恢复后协议 04 的核对 | `confirmations`、`core` |
| `documents`（共用） | 报价与订单共用：表头的取证与授权、行的取价与金额、单号、列表 | `core`、`masterdata`、`orgunits`、`pricing` |
| `ledger`（共用） | 可出账台账变化的事件 | `core` |
| `masterdata`（共用） | 事务外向主数据组件取证 | `core` |
| `core`（共用） | 名字、资源与操作的定义、错误与校验、记录类型、`Base` | — |

分层（每个包只引用比它低的层，没有环）：

| 层 | 包 |
| --- | --- |
| 7 | `deliveries`、`billingsource`、`recoverycheck` |
| 6 | `confirmations` |
| 5 | `orders`、`quotations` |
| 4 | `documents` |
| 3 | `orgunits`、`pricing` |
| 2 | `masterdata`、`ledger` |
| 1 | `core` |

核对：`go list -f '{{.Imports}}' ./internal/<包>`。

## 共同的写法

- **每个模块的函数先收 `m *core.Base`**（组件的运行时：数据库、调用、事件、审计、日志），入口包的 `Module` 持有它、登记时适配；模块之间直接调对方导出的函数。
- **名字在 `core`**：资源、操作、维度、事件类型、依赖的 ID、错误原因都在 `core/names.go`、`core/rules.go`；资源定义（`core.Definitions()`）是全组件一张表，因为列表能筛选的字段、维度的映射是按资源而不是按模块定的。
- **表的属主**写在各模块的说明里。订单的两张表是 `orders` 的，但订单行同时是台账：各业务在**订单行锁**下直接写自己的那几列（`deliveries` 写已交货，`billingsource` 写认领与已出账，`confirmations` 在服务订单确认时写可出账，`quotations` 接受报价时建订单草稿）；订单状态的变化经 `orders.SetOrderState`，只有进入“确认中”在 `confirmations` 提交确认的 T1 里写。
- **事务外取证、事务内写**：上游调用都在 `masterdata`，在事务之前；写入、审计（`core.Change`）与发件表（`core.Emit`）同一个事务。

## 加一个模块

一个新的业务能力有自己的表、路由与规则时才建模块（拿不准时放进最近的模块，宁可少切）：建目录、写 `AGENTS.md` 与 `CLAUDE.md`（`@AGENTS.md`），把它加进上面的表与仓库根的模块表，在 `sales/module.go` 登记它的路由与工作。

## 测试

模块没有单独的单元测试：业务规则由 `itest/` 的流程测试覆盖，每个模块的说明写了它在哪个文件里被测。
