# erp/sales

## 组件定位

销售订单从草稿到发货：带价格的订单行、决定这个价格的价格表，以及确认订单时在 `erp/inventory` 预留库存、预留之后任何一步失败就把预留释放掉的编排。`crm/opportunity` 里赢单的商机会经事件自动在这里变成一张已确认的订单。

**拥有**

- 订单与订单行（`sales_orders`、`sales_order_items`）：报价就是 `DRAFT` 状态的订单；`CONFIRMED`、`SHIPPED`、`CANCELLED`（终态）和 `SUSPENDED` 由下文的命令与事件推出。产品 SKU、名称、单位、价格都是建单时的快照。状态集里有 `COMPLETED` 与 `CLOSED`，但还没有命令能走到它们。
- 定价：有序的价格表（`pricelists`、`pricelist_items`），第一条命中的规则赢（产品级、分类级、全局依次；最小数量大的优先）。价格一律在这里算；`POST /price/dry-run` 就是为了让前端永远不自己算钱。
- 确认订单的编排：校验客户与产品、校验信用额度、预留库存、在同一个事务里确认并写事件；本地确认失败时补偿（释放预留）；补偿连续失败三次把订单标成 `SUSPENDED`。
- 把赢单的商机（`crm.opportunity.won.v1`）转成已确认的订单；订单在 `source_opportunity_id` 里记住来源商机。
- 每个客户信用额度的本地副本（`customer_snapshots`），靠 `mdm.customer.*` 事件维护，每次确认订单时再用 `mdm/customer` 的回答刷新。
- 事件 `sales.order.created.v1`（在订单确认时发，不是在建草稿时）、`sales.order.cancelled.v1`、`sales.order.shipped.v1`。

**不拥有**

- 客户是谁、信用额度的值：`mdm/customer`。产品、分类与单位：`mdm/product`。
- 库存够不够、预留本身：`erp/inventory`。本组件只发起预留，从不自己算可用量。
- 应收、凭证与权威的已用额度：`erp/finance`，它消费本组件的事件，事后可以否决一张订单的信用（`finance.credit.rejected.v1`）。
- 商机与销售漏斗：`crm/opportunity`。CRM 与 ERP 之间没有同步调用，两边只靠事件衔接。
- 审批与异常待办箱：`infra/workflow`。本组件在那里开异常待办、对待办完成作出反应；待办箱里没有销售规则。
- 实际发运、承运商、开票：目前没有组件负责。`ShipOrder` 只是让 `erp/inventory` 把预留转成出库，并把订单转成 `SHIPPED`。

## 部署前准备

- **PostgreSQL**：一个 schema、一个登录角色，名字随你定，分别写进 `PG_SCHEMA` 与 `PG_USER`。这个角色要能连上数据库、对 schema 有 `USAGE` 与 `CREATE`，并由它运行迁移（`./migrate up`），所以迁移建的表都属于它。这一点要紧：运行中的组件以这个角色自己建 `sales_orders` / `sales_order_items` 的月分区（提前三个月）和 `event_outbox` / `event_inbox` 的周分区（提前四周）。迁移里不写 schema 名和角色名，并按运行当天建好分区窗口，所以新库迁移完立刻能写。两个名字都必须是小写标识符（`^[a-z][a-z0-9_]*$`），否则组件拒绝启动并点名是哪个键。进外壳时外壳以自己的角色登录、每个事务切到 `PG_USER`（`SET LOCAL ROLE`），所以外壳的登录角色必须是 `PG_USER` 的成员，并保持默认的 `INHERIT`（Outbox 推送读 Outbox 时不切角色）。按 schema 约定另建的 `<schema>_archive` 在 search_path 里，但本组件从不写它。BrickKit 不建这些；在 BrickEnterprise 组装项目里由 `make dev-env` 把密码写进 `.env`、`make db-init` 建 schema `erp_sales`、角色 `erp_sales_rw` 与授权。
- **NATS** 在 `NATS_URL` 可达：事件经 Outbox 表和后台推送循环发出，消费的事件（见契约索引）也从这里进来。NATS 不可达时组件照样启动，要发的事件留在 Outbox 里。
- **仓库**：`erp/inventory` 里的一个仓库，把它的 ID 填进 `DEFAULT_WAREHOUSE_ID`：每次确认都从这一个仓库预留。BrickEnterprise 项目里 `erp/inventory` 的迁移预置了仓库 `1`（`WH-EAST`）。
- **价格**：迁移预置了一条全局规则（单价 `100.00`、无折扣、税率 `0.13`），让订单至少能定价。没有管理价格表的接口；真实价格是往 `pricelist_items` 里插的行。
- **信用额度**：只有客户的信用额度够用，订单才能确认。确认时本来就要向 `mdm/customer` 要这个客户，它的回答先刷新本地副本再判，所以在本组件装进来之前就有的客户、本组件停机期间改过额度的客户，都按当前额度判。副本只按 `version` 往前走；平时由 `mdm.customer.*` 事件保持最新。
- **部门**：用户的 `dept_path`（由 `infra/authz` 签发）决定 `dept` 视图里看到什么。还没分到任何部门的用户只看得到自己名下的订单；要看全公司的人应当分在根部门。
- **授权**（`infra/authz`）与**身份**（`infra/iam-casdoor`，或任何提供 JWKS 的 IAM）要能经 `AUTHZ_BUNDLE_URL`、`IAM_JWKS_URL` 访问到，REST 路由才不只回错误。它们是配置，不是依赖。
- 演示数据（可选）：本组件、`infra/iam-casdoor`、`infra/authz`、`mdm/customer`、`mdm/product`、`erp/inventory` 都在跑之后，在组件目录 `make seed` 经真实 REST 接口建五张不同状态的订单（会先灌好前面几个组件的种子，包括库存）。

## 依赖说明

- `mdm/customer` —— `BatchGet`：客户必须存在且启用；客户名称复制到订单上。建单时调一次，确认时再调一次。
- `mdm/product` —— `BatchGet`：每个产品都必须存在且启用；SKU、名称、基本单位与分类复制到订单行上（分类用来匹配分类级价格规则）。
- `erp/inventory` —— 确认时 `Reserve`（所有行都从 `DEFAULT_WAREHOUSE_ID` 预留；库存不足整单失败），`Reserve` 超时之后 `GetReservationStatus`，补偿时与取消已确认订单时 `CancelReservation`，发货时 `ConfirmIssue`。
- `infra/workflow`（可选）—— `CreateTask` 开异常待办：补偿连续失败三次（派给 `EXCEPTION_ASSIGNEE_SUB`），赢单商机没能转成已确认的订单（派给商机负责人），或财务拒绝了信用的订单（派给订单负责人）。缺席时不开待办；订单照样挂起，失败写进日志。

`erp/finance` 不是依赖：本组件从不调用它。它消费 `sales.order.created.v1`，以 `finance.credit.rejected.v1` 回告，两边只靠事件衔接。

用户请求路径上的每次调用都透传调用者的 token（`UserClient`）；只有赢单转单这条跑在事件 handler 里、没有用户的路径用本组件自己的身份调用。每次调用各自限时 5 秒。

授权包与 JWKS 从配置里的 URL 拉取，不经依赖边。拿不到时每条受保护路由都失败关闭：没有 token 或 token 无效 → `401`；`IAM_JWKS_URL` 为空或不可达 → `403`；授权包还没拉到过 → `503`；合法用户但没有该权限键 → `403`。`/healthz` 始终是 `200`：它只报告本进程活着。

## 配置指南

| 变量 | 含义 |
|---|---|
| `PG_HOST` | PostgreSQL 主机。一般用项目共享值（`$var:PG_HOST`）。 |
| `PG_PORT` | PostgreSQL 端口；默认 `5432`。 |
| `PG_DATABASE` | 放 `PG_SCHEMA` 那个 schema 的数据库（`$var:PG_DATABASE`）。 |
| `PG_USER` | 本组件干活用的角色，名字随意：独立运行时就是登录角色；进外壳时外壳以自己的角色登录，每个事务切到这个角色（`SET LOCAL ROLE`）。必填；必须是跑过迁移的那个角色。写字面量，BrickEnterprise 项目里是 `erp_sales_rw`。 |
| `PG_PASSWORD` | `PG_USER` 的密码。密钥：写 `${ERP_SALES_DB_PASSWORD}`（或你的密钥库引用），绝不写值。 |
| `PG_SCHEMA` | 全部表、Outbox 与迁移状态表（`schema_migrations_<PG_SCHEMA>`）所在的 schema，名字随意；默认 `erp_sales`。照样写上字面量，让每个组件的 schema 在一处看得见。 |
| `NATS_URL` | 发布与消费事件的 NATS 服务器（`$var:NATS_URL`）。 |
| `OTEL_BASE_URL` | OpenTelemetry 采集器的基础 URL；为空（默认）时什么都不导出。 |
| `AUTHZ_BUNDLE_URL` | 权限判定轮询的授权包 URL，例如 `http://infra-authz-2-0-0:8223/authz/bundle`。必填：没有它每条受保护路由都回 `503`。要跟项目里 authz 的版本保持一致。 |
| `IAM_JWKS_URL` | 本地验用户 token 用的 JWKS URL，例如 `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`。必填：没有它每条受保护路由都回 `403`。 |
| `DEFAULT_WAREHOUSE_ID` | 每次确认都从这个 `erp/inventory` 仓库预留（字面量，BrickEnterprise 项目里是 `"1"`）。必填且不能为空：缺了组件拒绝启动，因为从错误的仓库预留不会有任何人发现。 |
| `EXCEPTION_ASSIGNEE_SUB` | 补偿连续失败三次时异常待办派给谁（用户的 `sub`），例如一位运营审核人。默认为空：这种情况不开待办，订单照样挂起并记一条错误日志。 |

## 契约索引

- `contracts/erp/sales/v1/sales.proto` —— gRPC `erp.sales.v1.SalesService`：`CreateOrder`、`ConfirmOrder`、`CancelOrder`、`ShipOrder`（都带 `idempotency_key`）、`CalculatePriceDryRun`、`GetOrder`、`ListOrders`（游标分页，没有 offset；可按客户、状态、时间窗口、`source_opportunity_id` 过滤）、`BatchGetOrder`（找不到的 ID 放进 `missing_ids`，不报错）、`GetOrderStatus`（给命令超时的调用方用；`UNSPECIFIED` 表示没有这张单）。`Order.source_opportunity_id` 是赢单订单的来源商机，手工建的单为空。`GetOrder` 与 `ListOrders` 按调用者的数据范围过滤，需要调用者的身份；`BatchGetOrder` 与 `GetOrderStatus` 是组件之间的读，不带范围。Go 包是独立模块 `github.com/brickKit/erp-sales/gen/erp/sales`。
- `contracts/sales.openapi.yaml` —— REST 前缀 `/erp/sales`，每条路由都带权限键：`GET /orders`（`erp.sales.view`；`view=dept`（默认）或 `mine`，其它取值 `400`；`customer_id`、`status_filter`、`source_opportunity_id`、`created_after` / `created_before`（RFC 3339，默认最近 90 天）、`cursor`、`page_size`），`GET /orders/{id}`（`erp.sales.view`），`POST /orders`（`erp.sales.create`；数量是正的十进制字符串，整数至多 12 位、小数至多 6 位），`POST /orders/{id}/confirm`（`erp.sales.confirm`），`POST /orders/{id}/cancel`（`erp.sales.cancel`），`POST /orders/{id}/ship`（`erp.sales.ship`），`POST /price/dry-run`（`erp.sales.create`），`GET /stats/summary`（`erp.sales.view`；`period=week|month|quarter`（7 / 30 / 90 天，默认 `month`）与列表相同的 `view`，二选一，不会两个一起；订单数、已确认数与金额，按日、按状态分布）。金额是十进制字符串。错误：输入不合法、订单状态不对或库存不足 → `400`；订单不存在、或读一张不在调用者范围内的订单 → `404`（两者答得一样，数据范围不泄露订单是否存在）；确认、取消、发货一张不在范围内的订单 → `403`；超时后预留状态未知 → `503`（带同一个幂等键重试）。同一个键重放返回第一次的结果；键已被别的命令或别的订单用过 → `400`。
- `contracts/events/sales.events.json` —— 经 Outbox 发布：`sales.order.created.v1`（确认时发；带 `source_opportunity_id`，可选，手工单为空）、`sales.order.cancelled.v1`、`sales.order.shipped.v1`。消费：`finance.credit.rejected.v1`（挂起订单并给负责人开待办；挂起的订单用 `CancelOrder` 取消，同时释放库存；已发货的订单不挂起）、`mdm.customer.created.v1` / `.updated.v1`（额度副本；没带 `credit_limit` 的更新保留原额度）、`infra.workflow.task.completed.v1`（补偿异常待办被同意后订单回到 `DRAFT`）、`crm.opportunity.won.v1`（建单并确认）。
- `assembly.yaml` —— 本项目的元数据：上面五个权限键、菜单项、网关路由 `/erp/sales/**`、本项目开出的 schema 与角色（`erp_sales`、`erp_sales_rw`），以及 `sales_orders` 上的数据范围 `org`（`dept_path` 前缀）与 `owner`（`owner_id`）。没有部门的调用者（token 里 `dept_path` 为空）什么 `org` 行都不命中：`dept` 视图为空、统计为零，只看得到、动得了自己名下的订单。他仍然可以建单，这些单的 `dept_path` 为空，只对他本人可见。`dept_path = "/"` 是显式的根标记，命中所有带部门的订单。

## 外壳声明

不是外壳。可以由 Go 外壳托管（BrickEnterprise 项目里是 `be/go-core`），也可以单独运行；两种方式代码相同。
