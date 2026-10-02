[English](design.md) · [中文](design.zh.md)

# erp/sales 设计

只写结论，给要改本组件设计的人看。怎么用：`BRICKKIT.zh.md`；代码怎么组织：`AGENTS.zh.md`。

## 边界

本组件拥有销售订单从报价到发货的全过程：订单与订单行、价格表，以及确认订单时预留库存、预留之后某一步失败就把预留释放掉的编排。

| 不在这里 | 归谁 | 为什么 |
|---|---|---|
| 客户是谁、信用额度的值 | `mdm/customer` | 建单时 `BatchGet` 校验并复制到订单上；额度靠它的事件在本地保存一份，确认时再用 `BatchGet` 的结果刷新 |
| 产品是什么、单位 | `mdm/product` | 同上：建单时校验并快照 |
| 库存够不够、预留 | `erp/inventory` | 本组件只发起预留；"够不够"由库存那边自己 SQL 里的一个条件判定。发起方从不自己算可用量 |
| 应收、凭证、权威的已用额度 | `erp/finance` | 它消费 `sales.order.created.v1`，事后可以否决信用（`finance.credit.rejected.v1`） |
| 商机与销售漏斗 | `crm/opportunity` | CRM 与 ERP 之间没有同步边；赢单的商机以事件到达本组件 |
| 审批与异常待办箱 | `infra/workflow` | 待办箱里没有业务规则：本组件在那里开异常待办，并对待办完成作出反应 |
| 发运执行、承运商、开票 | 目前没有组件负责 | `ShipOrder` 只把预留转成出库、把订单转成 `SHIPPED` |

**订单状态。** `DRAFT`（报价和订单是同一行；报价转订单若换表，单号、行、快照全要白搬一遍）→ `CONFIRMED` → `SHIPPED`；`DRAFT` 或 `CONFIRMED` → `CANCELLED`；财务否决信用或补偿连续失败三次 → `SUSPENDED`。状态集里有 `COMPLETED` 与 `CLOSED`，但还没有命令能走到它们。`CANCELLED` 是终态，绝不复活：取消时已经在另一个进程里释放了预留，复活就得重新预留，而那时库存可能已被别人占走——跨服务边界的补偿是不对称的。挂起的订单在有人同意补偿异常待办后回到 `DRAFT`（不是"已确认"）：确认从未提交成功，`DRAFT` 是唯一诚实的起点，再次确认要由人或调用方显式发起。没有 `locked` 标志；将来真需要时，是状态之外的一个布尔，不是一个状态。

## 拥有的数据

| 表 | 分区 | 说明 |
|---|---|---|
| `sales_orders` | 按 `created_at` 月分区 | 订单头：客户与名称快照、状态、`total_amount NUMERIC(18,2)`、`dept_id` / `dept_path` / `owner_id`（创建时快照）、`reservation_id`、`compensation_attempts`、`suspended_reason`、`source_opportunity_id`（手工单为空）、`version` |
| `sales_order_items` | 按订单头的 `created_at`（`order_created_at`）月分区 | SKU、名称、单位的快照，`qty NUMERIC(18,6)`（`CHECK qty > 0`）、单价、折扣（`0 ≤ d < 1`）、税率、小计 |
| `pricelists`、`pricelist_items` | 否 | 有序规则：`applied_on` 为 `PRODUCT` / `CATEGORY` / `GLOBAL`、`min_quantity`、生效日期、单价、折扣、`price_limit`、税率 |
| `customer_snapshots` | 否 | `credit_limit`（来自 `mdm.customer.*`，确认时刷新）、`credit_exposure`（还没有任何东西维护它：见未决问题）、`version` |
| `sales_order_reconciliation_queue` | 否 | 预留状态一直未知的每次确认一行 |
| `command_idempotency` | 否 | 四个命令的先声明式幂等：键、用过它的命令、得到的订单 |
| `event_outbox`、`event_inbox` | 按 `created_at` 周分区 | 推送循环读的 Outbox；消费去重用的 Inbox |

`order_no` 是 `SO` + ID：唯一，允许有缺口（插入回滚时序列号照样被消耗）。销售单号有缺口不是审计问题；那条要求属于会计过账号，不在这里。订单行是完整快照：产品后来改名不该改写历史订单，而且产品数据在另一个进程里，本来就无法 join。

**库身份是配置。** schema 就是 `PG_SCHEMA`，角色就是 `PG_USER`，叫什么都行；两者谁也不从谁推，迁移里也不写其中任何一个。部署方给一个 schema、一个对它有 `USAGE` 与 `CREATE` 的登录角色；迁移以这个角色运行，所以表全属于它——分区循环以这个角色建分区，要的正是这一点。独立运行时连接池以 `PG_USER` 登录；进外壳时以外壳自己登录，每个事务切到成员自己配置里的 `PG_USER`（`SET LOCAL ROLE`）。原来的做法（角色 = schema + `_rw`，迁移里写 `ALTER … OWNER TO erp_sales_rw`）把组件绑死在一个项目的命名上：换一个部署，或者同一个项目改了 schema 名，都装不上。组件要可替换、库要可换，就不能带着宿主的名字；`backend/module/identity_test.go` 用两个毫不相干的名字把它装进去，两种形态各走一遍。

## 契约面

gRPC `erp.sales.v1.SalesService`：

| rpc | 类型 | 说明 |
|---|---|---|
| `CreateOrder` | 写 | 幂等；建 `DRAFT`，不预留 |
| `ConfirmOrder` | 写 | 幂等；即下面的 TCC 链 |
| `CancelOrder` | 写 | 幂等；已确认的订单释放其预留；对已取消的订单再取消，原样返回 |
| `ShipOrder` | 写 | 幂等；库存那边 `ConfirmIssue`，然后 `SHIPPED` |
| `CalculatePriceDryRun` | 读 | 纯计算：定价与 `CreateOrder` 相同，什么都不存 |
| `GetOrder`、`ListOrders` | 读 | 带数据范围；游标分页，默认窗口最近 90 天 |
| `BatchGetOrder` | 读 | 给别的组件的批量读；找不到的 ID 放进 `missing_ids` |
| `GetOrderStatus` | 读 | 给命令超时的调用方；`UNSPECIFIED` = 没有这张单 |

REST 前缀 `/erp/sales`，每条路由都带 `assembly.yaml` 里的键：`GET /orders`、`GET /orders/{id}`、`GET /stats/summary`（`erp.sales.view`）；`POST /orders`、`POST /price/dry-run`（`erp.sales.create`：试算是准备建单的一部分，不单开一个权限）；`POST /orders/{id}/confirm`、`/cancel`、`/ship`（`erp.sales.confirm`、`.cancel`、`.ship`）。`BatchGetOrder` 与 `GetOrderStatus` 不上 REST：它们是组件间协议。试算必须上 REST：下单页随输入实时定价，前端永远不自己算钱。

**幂等。** 每个命令最先做的是到 `command_idempotency` 查自己的键，排在状态校验之前，也排在调用任何依赖之前：这个键已经成功过，就原样返回第一次的结果。顺序很要紧：确认成功之后订单已是 `CONFIRMED`，先判状态的话每次重放都答"不是草稿"，响应丢了的调用方会被告知确认失败，同一个赢单商机来两次会开一条假的异常待办。建单的重放也不再校验客户与产品：之后被停用的客户不该让重放失败。一个键只属于一次命令、一张订单，规则同 `infra/workflow`：键被别的命令用过、或用于别的订单，答 `400`。照样短路的话，发货会拿到确认的结果，而库存在那之前已经出了。建单的键对应的订单不在调用者范围内，同样答 `400`，猜中别人的键也拿不到别人的订单。最后那个事务里的声明仍然保留：它决定同一个键的两个并发请求谁先，后到的那个按同样的规则处理。

**确认链**（`ConfirmOrder`，赢单路径也用它）：

```
① BatchGet 客户（mdm/customer）     失败 → 返回，无需补偿
② BatchGet 产品（mdm/product）      失败 → 返回，无需补偿
③ 校验信用额度                       先用①的回答刷新本地副本再判；超额度 → 返回（不多一次调用；在任何预留之前）
④ Reserve（erp/inventory）          被拒 → 返回；超时 → 先查状态，见下
⑤ 确认 + Outbox，同一事务            失败 → 补偿：CancelReservation
```

信用校验刻意排在预留之前：不然一张明显超额度的单会先占库存、再被拒、再补偿，白忙一场。它不只信事件维护的副本：事件走 NATS 核心订阅、从不重放，本组件装进来之前就有的客户没有副本（额度 `0`，每张单都被拒），本组件停机期间的改动也永远到不了。第①步手里已经有这个客户的 `credit_limit` 与 `version`，所以先用它刷新副本（只按 `version` 往前走），再读副本判。`Reserve` 超时绝不用 `CancelReservation` 回应：没收到响应不代表请求没被处理。由 `GetReservationStatus` 决定：`RESERVED` → 继续⑤；查不到 → `503`，带同一个幂等键重试；`CANCELLED` → 失败；查状态也超时 → 往 `sales_order_reconciliation_queue` 写一行、回 `503`，不阻塞用户请求。被拒（库存不足）整单失败：没有部分预留，这比 Odoo 严，是刻意的——部分成功会把补偿变成"补偿一部分"。补偿失败时原地重试，用同一个幂等键，不跟着调用方取消（已经成功的预留，客户端断开了也必须释放），间隔逐次拉长；连续失败三次把订单标成 `SUSPENDED`，并给 `EXCEPTION_ASSIGNEE_SUB` 开一条异常待办（幂等键由这次确认的键派生，同一个命令重放拿到的是同一条待办）。绝不无限重试：补偿的补偿比不补偿更糟。对依赖的每次调用各自限时（5 秒）；不需要补偿的命令（建单、取消、发货）超时就失败，由调用方带同一个键重试。

**定价**：一张有序规则表，第一条命中的赢——按特异度（`PRODUCT`、`CATEGORY`、`GLOBAL`），再按 `min_quantity` 降序，再按 ID 降序；生效日期对照当天。一条规则同时给出单价、折扣、`price_limit` 与税率；规则从不叠加，所以"为什么是这个价"永远只有一个答案。小计 = 数量 × 单价 × (1 − 折扣)，精确计算、最后四舍五入（远离零）到分一次（与 `NUMERIC(18,2)` 的舍入一致）。还没有价格表的接口；迁移预置一条全局规则，让整条链跑得通。

**输入校验**：数量是正的普通十进制数，整数至多 12 位、小数至多 6 位（列是 `NUMERIC(18,6)`）；带符号、指数写法、`NaN`、`Inf`、十六进制与数字分隔符一律 `400`。`NaN` 存进 `NUMERIC` 后排在一切数字之上，`CHECK` 会放它过去，所以校验在代码里、在进 SQL 之前。客户事件带来的信用额度用同一类规则把关（非负，整数至多 16 位、小数至多 2 位）。

**统计**（`GET /stats/summary`）：最近 7 / 30 / 90 个自然日（含今天，UTC），按订单的 `created_at` 归日。`order_count` 数窗口里建的全部订单；`confirmed_count` 与 `total_amount` 只算 `CONFIRMED`、`SHIPPED`、`COMPLETED`；`by_day` 列出每一天（含零单的日子），`by_status` 列出全部七个状态。数据范围与 `GET /orders` 完全相同，含 `view`。

**错误与日志**：输入不合法、状态不对、库存不足、超额度是 `400`；不存在 `404`，读一张不在范围内的订单也是 `404`、文案一字不差，数据范围不泄露订单是否存在；对范围外订单的命令（确认、取消、发货）`403`；预留状态未知 `503`。这类调用方的错误记 INFO，暂时性的记 WARN；ERROR 只留给运维必须处理的事（依赖不可达、数据库出错、订单被挂起）。

## 发布与消费的事件

经 Outbox 发布，与状态变更在同一个事务里：

| Subject | 何时 | Payload |
|---|---|---|
| `sales.order.created.v1` | `ConfirmOrder`（不是 `CreateOrder`：草稿不该产生应收） | 订单 ID 与单号、客户、行（产品、数量、小计）、总额、版本、`source_opportunity_id`（可选；手工单为空） |
| `sales.order.cancelled.v1` | `CancelOrder` | 订单 ID、原因 |
| `sales.order.shipped.v1` | `ShipOrder` | 订单 ID、行 |

`erp/finance` 消费 `created`（应收、已用额度）。`crm/opportunity` 也消费 `created`，`source_opportunity_id` 是它的商机时回填商机的 `order_id`；这样 CRM 与 ERP 只靠事件衔接，两个方向都没有依赖边。这个字段是加在已有 subject 上的，不是新事件，现有消费者不受影响。

消费：

| Subject | 来自 | 作用 |
|---|---|---|
| `finance.credit.rejected.v1` | `erp/finance` | 订单转 `SUSPENDED`，原因里带已用额度与额度；终态订单不动 |
| `mdm.customer.created.v1`、`.updated.v1` | `mdm/customer` | 信用额度副本；`version` 严格递增；没带 `credit_limit` 的更新保留原额度；额度不合法的事件整条拒绝 |
| `infra.workflow.task.completed.v1` | `infra/workflow` | 只认 `source_component = erp/sales`、`source_aggregate = sales_order`、`action = APPROVED`：原因是补偿失败的挂起订单回到 `DRAFT`。`REJECTED` 什么都不做（对异常待办的含义不明确）；本组件从不调 `CloseTask` |
| `crm.opportunity.won.v1` | `crm/opportunity` | 建单并确认（见下） |

不消费 `finance.voucher.posted.v1`：它没有客户 ID，说不出是谁的已用额度变了。

**赢单商机 → 订单。** handler 里没有用户：客户端用本组件自己的身份，负责人与部门取自事件（`owner_id`、`dept_path` 在那边是必填字段）。它复用确认链的辅助函数，不用公开的 `CreateOrder` / `ConfirmOrder`（它们读调用者的范围）。幂等键由商机 ID 派生，同一条事件只能产生一张订单。事件里的报价单价只供参考；订单按本组件的定价。任何失败都给商机负责人开一条异常待办（`source_aggregate = opportunity_conversion`，没有任何东西会自动恢复它），从不回报给 CRM：不论 ERP 这边发生什么，赢单的商机都保持赢单。handler 总是返回成功，因为返回的错误只会被记日志，不会重投。

## 依赖

| 依赖 | 用来做什么 | 在哪里调用 |
|---|---|---|
| `mdm/customer` | `BatchGet`：客户启用、名称快照 | 建单、确认 |
| `mdm/product` | `BatchGet`：产品启用、SKU / 名称 / 单位 / 分类快照 | 建单、确认 |
| `erp/inventory` | `Reserve`、`GetReservationStatus`、`CancelReservation`、`ConfirmIssue` | 确认、取消、发货 |
| `infra/workflow`（可选） | `CreateTask` 开异常待办 | 补偿、赢单转单 |

`infra/workflow` 缺席时平台根本不注入它的地址；组件用 `rt.Config.Endpoint` 判断后跳过开待办（订单照样挂起，失败写进日志）。用户请求上的每次调用都透传调用者的 token；只有事件 handler 用系统身份（`make gates` 扫描请求路径上的 `SystemClient`）。

| 不是依赖 | 为什么 |
|---|---|
| `crm/opportunity` | CRM 与 ERP 之间没有同步边；赢单商机以事件到达，订单 ID 以事件回去 |
| `erp/finance` | 本组件不调用它：财务消费 `sales.order.created.v1`，以 `finance.credit.rejected.v1` 回告。一条没有调用的边只会让本组件每次运行都把财务也带起来 |
| `infra/iam-casdoor`、`infra/authz` | token 与权限包从配置里的 URL 来 |

## 在同步调用图里的位置

根节点：出边指向 `mdm/customer`、`mdm/product`、`erp/inventory` 与可选的 `infra/workflow`，它们都不回调，所以没有路径能回到这里。入边来自用户（REST）与别的组件的读（`BatchGetOrder`、`GetOrderStatus`）。它从不指向任何 CRM 组件。

## 分区与归档

`sales_orders` 与 `sales_order_items` 按月分区，行按订单头的 `created_at` 分区，这样一张订单的行总在订单头所在的分区里，归档时一起走。组件自己提前三个月建分区；`event_outbox` / `event_inbox` 按周分区，提前四周。迁移用同一套分区命名，循环认得出它们建的分区。001 / 002 的初始分区是写迁移时的日期（2026-09 起），装到之后某天的新库上，当天就没有分区，要等循环跑完第一轮；所以迁移 005 按运行当天建好循环的那个窗口，新库迁移完立刻能写。价格表与客户快照量小、永久保留。

归档有设计、没有实现：订单只有在终态（`COMPLETED`、`CANCELLED`、`CLOSED`）**并且**超过 12 个月时才移到 `<schema>_archive`——一张 14 个月前建的、至今没发货的订单仍然是活的。

## 数据范围

`sales_orders` 上两个维度：`org`（`dept_path` 前缀）与 `owner`（`owner_id` 等于调用者），即销售组织最基本的两个视图"本部门及下级"与"我的订单"。范围列是创建时的快照，不是去查这个人现在在哪个部门；否则销售换了部门，他以前的订单会跟着走，每一张报表都悄悄变数。

列表与统计用 `view` 选一个视图（默认 `dept`，或 `mine`；其它取值 `400`，拼错的 `mine` 不会悄悄变成看全部门），绝不取交集或并集（交集会把"本部门"缩窄成"本部门里我自己的"）。单张订单（查看、确认、取消、发货）两者任一命中即可见；范围外的读答 `404`，就像这张单不存在，命令答 `403`。两个操作数始终来自调用者的真实范围（`besdk.ScopeOf`），哪一个都不能缺省成"全部"。范围下推进 SQL 的 `WHERE`，从不在 Go 里事后过滤（否则分页就乱了）。赢单转成的订单，负责人与部门取自事件。

**没有部门的调用者**（token 的 `dept_path` 为空：还没被放进组织的新账号）什么 `org` 行都不命中：SDK 给的部门前缀是任何真实路径都不以它开头的值。他的 `dept` 视图是空列表、统计为零；单张订单只有他是负责人时才可见。他仍然可以建单：快照里的 `dept_path` 与 `dept_id` 为空，这张单只对负责人可见（谁的部门前缀都不命中空路径，根部门也不例外）。SDK 的"无部门"值从不写进行里；仓储层拒绝既不为空、也不以 `/` 开头的 `dept_path`。要看全公司，就坐在根部门里，或持有显式的根标记 `/`，它作为前缀命中所有带部门的订单。仓储层把空前缀或空负责人当成调用方的错误（`InvalidArgument`），绝不当成"不限"。

## 参考实现

| 项目 | 版本 | 看的模块 | 借鉴了什么 | 许可证 | 用法 |
|---|---|---|---|---|---|
| Odoo | 17.0 | `addons/sale/models/sale_order.py` | 报价与订单同表 + 状态；确认动作的原子性边界 | LGPL-3 | 借鉴逻辑 |
| Odoo | 17.0 | `addons/sale/models/sale_order_line.py` | 订单行完整快照（算一次、存下来） | LGPL-3 | 借鉴逻辑 |
| Odoo | 17.0 | `addons/product/models/product_pricelist_item.py` | 价格表是有序规则表、第一条命中的赢；三档适用范围 | LGPL-3 | 借鉴逻辑 |
| metasfresh | master | `de.metas.business` 的定价规则 | `price_limit`（最低售价）作为一等字段而不是公式参数：最常被审计的那一条 | GPL-2/3 | 借鉴逻辑 |

**刻意避开的**：Odoo 的 `action_draft()` 复活已取消的订单；Odoo 在库存不足时部分预留、订单照样确认；定价逻辑散落在许多计算字段里；metasfresh 可组合的规则链，可配置过头，交付现场没人会配。跨服务的补偿链没有参考：两个系统都是单进程单事务，既没有这个问题，也就没有这个解。它的正确性靠对真实依赖的故障注入测试（真的库存不足、真的超时），不靠 mock。

**槽位族**：有四处合理实现因客户而异——预留严格度（确认即预留 / 手工 / 按日期）、缺货策略（整单失败 / 部分发货加欠单 / 等齐再发）、开票基准（按订单量 / 按发货量）、定价引擎（有序表 / 规则链）；其中三处 Odoo 自己就做成了配置项。它们不做成槽位：别的组件依赖本组件，而只有没人对它建依赖边的位置才能做槽位。需要另一种选择的客户，用本组件的 fork（`metadata.id` 不变）。本组件选的是：确认即全额预留、缺货整单失败、暂不开票、有序表定价。

## 未决问题

| 问题 | 目前的答案 |
|---|---|
| 本地信用校验知道额度、不知道已用额度（`credit_exposure` 一直是 `0`）：`finance.voucher.posted.v1` 没有客户 ID，与 `erp/finance` 之间也没有对账 | 权威判定是财务的 `finance.credit.rejected.v1`，事后把订单挂起。改历史订单前的期间检查、对账调用都会带回一条到 `erp/finance` 的同步边；那是一个新的设计决定，不是遗漏 |
| `sales_order_reconciliation_queue` 有行写入，但没人读 | 调用方收到 `503` 后带同一个键重试，订单就能落定；对账任务没做 |
| `ShipOrder`：`ConfirmIssue` 成功了，本地 `SHIPPED` 却失败 | 订单停在 `CONFIRMED`，库存已出；带同一个键重试即可补完（两边都幂等）。没有自动补完 |
| gRPC 的 `GetOrder` / `ListOrders` 需要调用者的 Claims，而 be-sdk-go 的 gRPC 服务端不建立它 | 这类调用以内部错误失败；用户经 REST 读。`BatchGetOrder` 与 `GetOrderStatus` 按设计不带范围（组件间协议） |
| `COMPLETED` / `CLOSED` 没有命令；没有开票 | 还没有页面需要；开票属于槽位族里的选择（见上） |
| 价格表没有接口 | 直接插行；有页面需要时加管理接口，是只增的契约变更 |
| 每次确认都用同一个仓库（`DEFAULT_WAREHOUSE_ID`） | 按行或按客户选仓，需要 `erp/inventory` 提供仓库查询，再加一条分配规则 |
| 赢单 handler 调用依赖期间占着 inbox 事务的数据库连接 | 每个赢单事件至多几次调用超时那么久；按预期的量可以接受 |
| 统计按 UTC 归日 | UTC+8 的用户看到的日界在早上 8 点；仪表盘需要时加时区参数，是只增的变更 |
| 终态且超过 12 个月的订单归档 | 有设计，没实现 |
| 进外壳时 `besdk.StartOutboxPump` 以外壳的登录角色读写 `event_outbox`：它不收角色参数，也不切到 `PG_USER` | 外壳的角色必须是 `PG_USER` 的成员并带 `INHERIT`（写在 BRICKKIT 的「部署前准备」）。按批切角色的推送要改 be-sdk-go |
