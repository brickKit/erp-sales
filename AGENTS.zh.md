[English](AGENTS.md) · [中文](AGENTS.zh.md)

# erp/sales

开发本组件的 AI 指南。用法、边界与契约：BRICKKIT.md；为什么长这样：`docs/design.zh.md`；依赖、配置与部署：component.yaml。

## 代码地图

| 路径 | 负责 |
|---|---|
| `backend/module/module.go` | 唯一入口 `New(ctx, rt)`：读配置（`readConfig`），装配 repo → 编排器 → service → HTTP + gRPC，启动 Outbox 推送、两个分区循环与消费者。独立运行与进外壳同一个函数 |
| `backend/cmd/server/main.go` | 一行 `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | 一行 `migrate.Main(migrations.FS)`：迁移容器的入口（`./migrate up`） |
| `backend/internal/tcc/` | 跨组件编排：`confirm.go`（预留、超时后查状态、补偿、挂起与异常待办）、`create.go`、`cancel.go`、`ship.go`、`opportunity_won.go`（赢单商机 → 已确认订单，系统身份）、`loglevel.go`（`FailureLevel`） |
| `backend/internal/client/client.go` | 拨号到五个依赖：`Customer`、`Product`、`Inventory`、`Finance`、`Workflow` 透传调用者 token；`*System` 一组只给事件 handler 用 |
| `backend/internal/repo/write.go` | 本地写：`CreateOrder` 与各个 `Finalize*` 步骤；每个都在一个事务里做 幂等声明 → 锁行核对状态 → 写 → Outbox |
| `backend/internal/repo/order.go` | 订单类型与状态、`GetOrder`、`ListOrders`（范围、过滤、keyset 分页）、`BatchGetOrder`、`GetOrderStatus` |
| `backend/internal/repo/pricing.go` | `validateQty`、价格规则查找 `findPriceTx`、`CalculatePriceDryRun` |
| `backend/internal/repo/stats.go` | `GET /stats/summary` 的 `StatsSummary` |
| `backend/internal/repo/snapshot.go` | 客户信用额度副本（`UpsertCustomerSnapshotCreditLimitTx`、`GetCustomerSnapshotTx`） |
| `backend/internal/repo/suspend.go` | `SuspendOrderTx`、补偿失败计数、`ResumeFromExceptionTx` |
| `backend/internal/repo/repo.go` | 哨兵错误、`Order.InScope`、`parseID` |
| `backend/internal/decimal/decimal.go` | 字符串上的精确十进制运算（标准库的 big.Rat），四舍五入（远离零）到分 |
| `backend/internal/consumer/consumer.go` | 消费的五个 subject 及其 handler |
| `backend/internal/service/` | 入参校验、单张订单命令的数据范围、按 `tcc.FailureLevel` 记日志、错误 → gRPC 状态码映射（`status.go`） |
| `backend/internal/http/http.go` | REST 路由，每条都带权限键注册；`parseListInput`、`statsSummaryHandler` |
| `backend/internal/grpc/grpc.go` | `erp.sales.v1.SalesService` |
| `backend/internal/partition/` | 后台循环：`event_outbox` / `event_inbox` 的周分区提前四周、`sales_orders` / `sales_order_items` 的月分区提前三个月建好 |
| `migrations/` | SQL 迁移，由 `migrations/embed.go` 嵌入 |
| `contracts/` | proto、OpenAPI、事件 schema |
| `gen/erp/sales/` | 生成的 Go 代码：嵌套的独立 Go 模块，单独打 tag gen/erp/sales/v1.x.y；绝不手改 |
| `scripts/` | 本地演示数据的 `seed.sh` / `seed-clean.sh` |

| 功能 | 从这里开始 | 然后 |
|---|---|---|
| 确认订单的某一步、超时、补偿 | `backend/internal/tcc/confirm.go` | `backend/internal/tcc/compensate_test.go`、`backend/internal/tcc/confirm_test.go`（真实依赖） |
| 赢单商机 → 订单 | `backend/internal/tcc/opportunity_won.go` | `backend/internal/consumer/consumer.go`（`opportunityWonHandler`）、`backend/internal/tcc/opportunity_source_test.go` |
| 价格规则、数量校验 | `backend/internal/repo/pricing.go` | `backend/internal/repo/qty_test.go`、`backend/internal/repo/money_test.go` |
| 新的列表过滤或查询参数 | `backend/internal/http/http.go`（`parseListInput`） | `backend/internal/repo/order.go`（`ListOrders`）、`contracts/`（只增） |
| 仪表盘统计 | `backend/internal/repo/stats.go` | `backend/internal/repo/stats_test.go`、`backend/internal/http/http_test.go` |
| 订单上的新字段 | `migrations/`（新的迁移） | `contracts/`（只增）、`buf generate`、`backend/internal/repo/order.go` |
| 消费的事件 | `backend/internal/consumer/consumer.go` | 发布方的事件契约（照抄字段，绝不 import） |
| 错误回了不对的状态码，或日志级别不对 | `backend/internal/service/status.go` | `backend/internal/tcc/loglevel.go` |

## 构建与测试

```bash
# 测试连测试库 brickkit_test_db，绝不连 brickkit_db（项目根 make test-db-init 准备）
set -a; . ../../../.env; set +a
export TEST_PG_DSN="postgres://postgres:${POSTGRES_PASSWORD}@localhost:5432/brickkit_test_db?sslmode=disable"
make test                    # 每个包都是 "ok"；没设 TEST_PG_DSN 直接失败
go test ./... -count=1 -v | grep -- '--- SKIP'   # 只有需要真实依赖的测试（11 条，见下文）
make check-version dag-check contract-check import-scan module-check   # 每个打印一行 ✓
make docs-check              # "0 with errors, 0 warnings"
# 迁移，以登录角色运行：
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=erp_sales_rw \
  PG_PASSWORD="$ERP_SALES_DB_PASSWORD" PG_SCHEMA=erp_sales make migrate-idempotent   # ✓ 迁移幂等
```

被跳过的测试（在 `backend/internal/tcc/confirm_test.go`、`backend/internal/tcc/opportunity_won_test.go`、`backend/internal/consumer/opportunity_won_dlq_test.go` 里）驱动真实的 `mdm/customer`、`mdm/product`、`erp/inventory` 与 `infra/workflow` 容器：真的库存不足、真的超时。在项目根用 `make test-cross ID=erp/sales` 跑，它会注入依赖地址。`buf generate` 之后（契约变了），契约包要打新 tag：见易错点。真机在项目根跑 `make verify ID=erp/sales ROUTE=/erp/sales/orders FOCUS=1`：构建镜像、连同依赖起本组件、核对迁移 / 健康 / 权限判定、跑一次 focus、收尾。

## 设计取舍

- **确认是一条 TCC 链**，本地校验在前：客户、产品、信用额度（本地副本，在任何预留之前），然后 `Reserve`，然后在同一个事务里本地确认并写事件；只有 `Reserve` 之后的失败才需要补偿。
- **`Reserve` 超时绝不用 `CancelReservation` 回应**：先问预留状态（`GetReservationStatus`）；还是不知道 → 写一行 `sales_order_reconciliation_queue`，给调用方回 `503`。
- **补偿原地重试三次**，同一个幂等键、不跟着调用方取消；第三次失败挂起订单并开异常待办。
- **库存不足整单失败**；没有部分预留。
- **`sales.order.created.v1` 在确认时发**，不是建单时：草稿不该产生应收。
- **订单行是快照**（SKU、名称、单位、价格）；之后不再 join 主数据。
- **两个数据范围视图，由 `view` 选**：`dept`（部门前缀）或 `mine`（负责人），绝不取交集；单张订单的读与命令两者任一命中即可。
- **金额与数量全程是十进制字符串**，用 `math/big.Rat` 计算，最后四舍五入（远离零）到分一次。
- **赢单商机只经事件变成订单**（系统身份，归属取自事件）；失败变成派给商机负责人的异常待办，绝不发事件回 CRM。

## 易错点

| 不许 | 症状 | 原因 |
|---|---|---|
| `Reserve` 超时后直接调 `CancelReservation` | 已经预留成功的库存被释放而订单照走，或失败的预留被当成功；数字悄悄漂移，没有报错 | 没收到响应不代表请求没被处理；`resolveAfterTimeout` 先查 |
| 在 `Reserve` 之后才校验信用额度 | 明显超额度的单先占住库存，再被拒、再补偿 | 本地校验不花钱，排在任何预留之前 |
| 用户请求路径上用 `*System` 客户端，或事件 handler 里用普通客户端 | 请求路径：响应里悄悄多出用户不该看到的行。事件 handler：调用不带 token | `UserClient` 透传调用者；事件 handler 没有调用者（`make gates` 跑 `system-client-scan`） |
| 赢单路径复用 `CreateOrder` / `ConfirmOrder` | 事件 handler 里 `besdk.ScopeOf` panic：没有 Claims | `createOrderForOpportunity` / `confirmOrderForOpportunity` 的归属取自事件 |
| 让 `opportunityWonHandler` 返回 error，指望重投 | 这条赢单事件再也不会出现 | `besdk.Consume` 走 NATS 核心订阅：handler 报错只记日志，不重投 |
| 只按 `status == SUSPENDED` 恢复挂起的订单 | 被财务信用否决挂起的订单，被一条无关的待办完成事件拉回 `DRAFT` | `ResumeFromExceptionTx` 精确匹配 `suspended_reason` |
| 处理每一条 `infra.workflow.task.completed.v1` | 别的组件的待办 ID 被当成订单 ID | 这个 subject 全平台共用；按 `source_component` / `source_aggregate` 过滤 |
| 用 `strconv.ParseFloat` 解析金额或数量，或不校验就落库 | `"NaN"` 进了 `NUMERIC` 列，排在一切数字之上，`CHECK (qty > 0)` 与每一个比较都放它过去；`0.1 + 0.2` 舍入到错的一分 | `validateQty` 与 `creditLimitPattern` 只认普通十进制写法；运算走 `backend/internal/decimal` |
| 把调用方的错误记成 ERROR | 每一张被拒的草稿、每一次库存不足都在告警 | `tcc.FailureLevel`：4xx → INFO，暂时性 → WARN，其余 → ERROR |
| `sales_order_items` 按行自己的时间分区 | 同一张订单的行和订单头落在不同分区 | 按订单头的 `created_at`（`order_created_at`）分区 |
| 改了 `contracts/erp/sales/v1/sales.proto` 的注释或选项，却不给契约包打 tag | 本地全绿（`replace` 掩盖了它）；外壳拉 `erp-sales/v2` 时对着旧的 `gen/` 编译 | `gen/erp/sales/` 下有任何变化都要打新 tag gen/erp/sales/v1.x.y，根 `go.mod` require 它 |

## 改代码前自查

1. 这件事关于订单（这里）、库存（`erp/inventory`）、欠款（`erp/finance`）还是商机（`crm/opportunity`）？只有第一种放这里。
2. 这个改动会在 CRM 与 ERP 之间加一条调用吗？停下：两边之间只有事件。
3. 在确认链里，新加的这一步超时了会怎样？那一刻是否已经持有预留？
4. 用户请求路径还是事件 handler？这决定用 `UserClient` 还是 `SystemClient`，以及 `besdk.ScopeOf` 能不能用。
5. 新的十进制输入：先用严格的写法、符号与位数校验再进 SQL；先写一条用 `NaN` 的红测试。
6. 改契约？只增。`gen/` 变了吗？那契约包要打新 tag，`go.mod` 要 require 它。
7. 新 REST 路由用 `besdk.GET` / `POST` 加 `assembly.yaml` 里的权限键注册；新的列表或统计端点按与 `GET /orders` 相同的范围过滤，并有一条两个用户、真实建库的测试。
8. 发布之后第一次改动前先升 `metadata.version`；BRICKKIT.md 与 `docs/design.md` 与代码同一个提交更新。
