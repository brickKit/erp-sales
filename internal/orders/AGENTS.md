# orders：订单

订单草稿的建、读、改与列表；订单状态的变化在这里写；已结束的订单作为冷热分层的数据类别（装配项目协议 11）。

## 负责与不负责

- 负责：表 `sales_order`、`order_line`（订单行的台账列由各业务在订单行锁下写，见 [internal/AGENTS.md](../AGENTS.md) 的“表的属主”）；路由 `/api/sales/orders` 的列表、新建、读取、修改；`close`、`revisions` 两条契约里有、本版不做的路由（`UnsupportedOnOrder`）。
- 不负责：确认与取消（`confirmations`）；交货（`deliveries`）；出账（`billingsource`）；表头与行的共用部分（`documents`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `orders.go` | `CreateOrder`、`GetOrder`（先问归档，单元仍在线才读在线表）、`UpdateOrder`（确认中拒绝，RS09）；`ListOrders` 与列表的定义（`orderFrom`：默认范围“未结束，或最近 N 天创建”，`to_deliver`、`to_bill` 两个进度筛选）；`SetOrderState`：确认、退回草稿、取消、完成都经过它，订单事件与审计在这里写 |
| `archive.go` | 冷热分层：数据类别 `OrdersClass()`（订单与它的行、过程与分支、交货、认领，三层七张表）、阻塞封存的条件、`Tiering` 登记；按身份读归档的订单（`archived`）；`history=true` 的列表（`history`、`Reader`） |

## 依赖与被依赖

- 依赖：`core`、`documents`。
- 被依赖：`confirmations`、`billingsource`（`SetOrderState`）；入口登记路由、数据类别与 `Tiering`。

## 规则在哪

- 订单的状态：`docs/design.md` 的“订单的状态”；确认中拒绝修改：协议 04 的 RS09。
- 冷热分层：`docs/design.md` 的“规则要点”（什么时候不封存、列表的默认范围）；装配项目协议 11；决定 A154、A155。

## 测试

- `itest/confirmations_integration_test.go`（状态的每次变化）、`itest/lists_integration_test.go`（关键字、`to_bill`）。
- `itest/archive_integration_test.go`（`tiering`：封存、释放、按身份读、默认列表与历史，TR10、TR30）。
