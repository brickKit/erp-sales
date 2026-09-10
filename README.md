# erp-sales · 销售管理

销售报价与订单的生命周期、订单行、定价（价格表/折扣）、防超卖的**发起方**逻辑、TCC 补偿编排——设计书 §8.2 的"标准砖样板 B"就是本组件，也是**唯一的链上一环**（四条强依赖边全在它身上）。

## 它能做什么
- 报价/订单的生命周期：`DRAFT → CONFIRMED → SHIPPED → COMPLETED`，`CANCELLED`/`CLOSED` 是另外两个终态
- `ConfirmOrder` 的 TCC 链：校验客户/产品 → 本地信用额度预判 → `Reserve` 库存 → 建单 + 发事件，失败按情形分别处理或补偿
- 定价（`CalculatePriceDryRun`）：前端/BFF 严禁自己算钱，只能调这个接口
- `ShipOrder`：预留转实际出库（调 `erp-inventory` 的 `ConfirmIssue`）
- 消费 `crm.opportunity.won.v1`：赢单自动转订单（阶段三 Task 14），走同一套建单+确认逻辑但用系统身份客户端，失败建 `infra-workflow` 异常待办通知销售，不回传给 `crm-opportunity`

## 需要哪些基础资源
| 资源 | 形态 | 为什么需要 | 怎么起 |
|---|---|---|---|
| PostgreSQL 16 | **A**（brickKit 基础资源，`kind: database`） | 数据持久化，独占 schema `erp_sales` | 装配仓库根目录 `make up` |
| NATS 2.10 | **A**（`kind: mq`） | 发布 `sales.order.*` 事件；消费 `finance.credit.rejected.v1`/`mdm.customer.*`/`infra.workflow.task.completed.v1`/`crm.opportunity.won.v1` | 同上 |

⚠️ 形态 A / B / C 的区别见设计书 §2.7.0。本组件**不需要** Traefik 与 Casdoor
就能单独跑起来——它不对 IAM 建依赖边，JWT 走本地验签（决策 87）。

⚠️ **本组件是全阶段二唯一有强依赖的组件**：`mdm/customer`、`mdm/product`、
`erp/inventory`、`erp/finance` 四个都要先起来（`brickkit up` 会按拓扑顺序处理，
不需要手动排序，但单独跑本组件做开发时要记得这四个也要在）。

## 怎么起来
（Task 17 实现完成后补：装配路径 + 单独跑的完整命令）

## 怎么用
（Task 17 后补：一条 curl + 一条 grpcurl）

## 配置项
（Task 16 写完 component.yaml 后补，平台注入的保留变量单列一段）

## 参考实现
| 项目 | 看的模块 | 借鉴了什么 | 许可证（已复核） | 用法 |
|---|---|---|---|---|
| Odoo 17.0 | `addons/sale/models/sale_order.py` | 状态机划分（报价与订单同表 + 状态位） | LGPL-3 | 借鉴逻辑 |
| Odoo 17.0 | `addons/sale/models/sale_order_line.py` | 订单行全量快照（compute + store + readonly=False） | LGPL-3 | 借鉴逻辑 |
| Odoo 17.0 | `addons/product/models/product_pricelist_item.py` | 价格表有序规则表 + 第一条命中赢 | LGPL-3 | 借鉴逻辑 |
| metasfresh | `pricing/rules/` | `priceLimit`（最低售价）作为一等字段，不是公式参数 | GPL-2/3 | 借鉴逻辑 |

**要避免它的什么**：Odoo 确认订单时库存不足不报错、预留能预留的部分照样确认订单——
本组件**预留失败就整单失败**，因为 `Reserve` 是跨进程 TCC 的一步，"部分成功"会让补偿
语义变成"补偿一部分"，复杂度上一个数量级。**这比两个成熟系统都严，是刻意的选择**。

**⚠️ 跨服务的 TCC 补偿编排，两个参考系统都没有对应物**——它们是单体单事务，
没有这个问题。这条链的正确性只能靠我们自己的故障注入测试保证，不是 mock。

## 边界与禁令
- 本组件是**唯一的链上一环**：四条强依赖边全指向零出边的枢纽（两个 mdm + inventory + finance），
  不违反"CRM 与 ERP 零同步边"——阶段三 `crm-opportunity → erp-sales` 是事件边不是同步边
- 四条依赖边全部用 `besdk.UserClient`（透传 JWT），**不许用 `SystemClient`**——
  这是用户请求路径，用 `SystemClient` 会绕过下游数据权限
- `Reserve` 超时**严禁直接调 `CancelReservation`**，必须先 `GetReservationStatus`
  查真实状态（防"薛定谔的超时"）
- `CANCELLED` 是终态，不能复活——取消订单时跨进程释放了库存预留，复活要重新 `Reserve`，
  而那时库存可能已经被别人占走
