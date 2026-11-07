# orders：订单与它的生命周期

订单草稿的建、读、改与列表；**订单的生命周期**（哪个阶段能做什么、状态怎样变）只在这里；已结束的订单作为冷热分层的数据类别（装配项目协议 11）。

## 负责与不负责

- 负责：表 `sales_order`（订单行的台账列归 `ledger`）；生命周期表与订单状态唯一的写入（`Transition`）；路由 `/api/sales/orders` 的列表、新建、读取、修改；`close`、`revisions` 两条契约里有、本版不做的路由（`UnsupportedOnOrder`）。
- 不负责：确认与取消的过程怎样跑（`confirmations`，它按表的格行事）；交货（`deliveries`）；出账与台账（`billingsource`、`ledger`）；表头与行的共用部分（`documents`）。

## 生命周期（改订单规则先读这里）

`lifecycle.go` 是一张表：6 个阶段（`draft`、`confirming`、`confirmed`、`cancelling`＝已确认且取消过程进行中、`completed`、`cancelled`）× 10 个命令（修改、提交确认、取消、交货、认领、完成、三种确认结果、取消过程结束），每格写：拒绝（原文）、忽略、允许、变成某状态（审计与否、操作）、经哪种过程，附加条件，规则编号 SO02–SO15。

- 用法：命令先 `PhaseOf` 取阶段，再 `Decide(阶段, 命令)`，按格行事；要变状态时调 `Transition`（带前置状态的条件更新、发订单事件、按格审计）。表以外不比较、不写订单状态：`sales/structure_test.go` 会拦；表以外必须读状态的（本包的列表与归档也算）登记在它的 `orderStateReaders`。
- **加一个状态或命令**：在表里加一列或一行并定下每一格（`TestTheLifecycleDecidesEveryCell` 不定完不过）；同步改 `docs/design.md` 的同一张表（`TestTheDesignDocumentIsTheLifecycle` 逐格比较）；规则目录加编号、集成测试的注释写上编号（`TestEveryRuleIsTested`）；再看 `sales/structure_test.go` 的 `orderStateReaders`：登记在那里的每一处（订单列表的 `to_deliver` 与默认范围、归档的“已结束”、历史列表、把状态交给 `erp/billing` 的台账、恢复核对）都要决定新状态怎样处理；契约里的状态枚举（OpenAPI、事件 schema）一并改。

## 文件

| 文件 | 内容 |
| --- | --- |
| `lifecycle.go` | 阶段、命令、效果、`Decision`；表 `lifecycle`；`Decide`、`PhaseOf`、`StoredPhase`（事务外、只用于两格相同的命令）、`Refused`、`Words`；`Transition` |
| `lifecycle_test.go` | 单元测试：每一格都定了且完整；`StoredPhase` 只用于两格相同的命令；设计文档的表与代码逐格相同；每条规则都有集成测试写明 |
| `orders.go` | `CreateOrder`、`GetOrder`（先问归档，单元仍在线才读在线表）、`UpdateOrder`（查表 `Edit`）；`ListOrders` 与列表的定义（`orderFrom`：默认范围“未结束，或最近 N 天创建”，`to_deliver`、`to_bill` 两个进度筛选）；`UnsupportedOnOrder` |
| `archive.go` | 冷热分层：数据类别 `OrdersClass()`（订单与它的行、过程与分支、交货、认领，三层七张表）、阻塞封存的条件、`Tiering` 登记；按身份读归档的订单（`archived`）；`history=true` 的列表（`history`、`Reader`） |

## 依赖与被依赖

- 依赖：`core`、`documents`。
- 被依赖：`confirmations`、`deliveries`、`billingsource`（`PhaseOf`、`Decide`、`Transition`）；入口登记路由、数据类别与 `Tiering`。

## 规则在哪

- 生命周期：`docs/design.md` 的“订单的状态”（同一张表与规则目录 SO01–SO15）；确认中拒绝修改：协议 04 的 RS09。
- 冷热分层：`docs/design.md` 的“规则要点”；装配项目协议 11；决定 A154、A155。

## 测试

- 单元：`lifecycle_test.go`（上表）；`sales/structure_test.go`（别处不读、不写订单状态）。
- 集成：`itest/lifecycle_integration_test.go`（业务流程走不到的格）、`itest/confirmations_integration_test.go`（状态的每次变化）、`itest/lists_integration_test.go`、`itest/archive_integration_test.go`（`tiering`：封存、释放、按身份读、默认列表与历史，TR10、TR30）。
