# erp/sales

销售组件的代码。用法与边界在 `BRICKKIT.md`，为什么这样设计（术语、数据、状态、规则、未决问题）在 [docs/design.md](docs/design.md)；细则在 [docs/flows/confirmation.md](docs/flows/confirmation.md)（确认过程）、[docs/rules/pricing.md](docs/rules/pricing.md)（取价与金额）、[docs/flows/delivery-and-billing.md](docs/flows/delivery-and-billing.md)（交货与出账认领）。跨组件的规则在装配项目 be-assembly-standard 的协议 04（本组件是协调者）、协议 05（本组件是可出账额度的属主）、协议 02（部门投影）、协议 07（资格取证）、协议 14（单号、金额、汇率、列表）、安全 07（三个维度）。

## 代码地图

代码按业务模块分层。**先读这一节，再进目录**：每个代码目录都有自己的 `AGENTS.md`（同目录的 `CLAUDE.md` 让 Claude Code 读这个目录的文件时自动带上它），写这一层负责什么、不负责什么、每个文件、依赖谁、被谁用、测在哪；读完就知道要不要往下走。

| 目录 | 是什么 |
| --- | --- |
| [`sales/`](sales/AGENTS.md) | 入口：组件定义与组装（路由、gRPC、订阅、后台工作、恢复核对都在这里登记，转给业务模块）；不写业务。`main.go` 与外壳只引用它 |
| [`internal/`](internal/AGENTS.md) | 业务：下面的表里每个模块一个包；模块之间的依赖表在它的 `AGENTS.md`。Go 的 `internal` 让别的组件编译期就引用不了（决定 0101） |
| [`itest/`](itest/AGENTS.md) | 集成测试，按业务流程分文件；它的 `AGENTS.md` 是测试索引（文件 → 流程 → 规则编号） |
| `sales/migrations/` | 表，整个组件一套（迁移顺序是全组件的，不按模块拆） |
| `cmd/seed/` | 种子：角色与三个销售账号的范围、价格表、各种状态的报价与订单（含确认被拒的反例）、交货（`make seed`） |
| `contract/` | 本组件发布的契约，单独的模块：`contract/proto/`、`contract/events/`、`contract/openapi.yaml` |
| `docs/` | 设计与细则（规则的正文；各层的 `AGENTS.md` 只指过去，不抄） |

业务模块（`internal/` 下）：

| 模块 | 负责 | 拥有的表 |
| --- | --- | --- |
| [`quotations`](internal/quotations/AGENTS.md) | 报价：草稿、发出、取消、接受成订单草稿 | `quotation`、`quotation_line` |
| [`orders`](internal/orders/AGENTS.md) | 订单草稿；订单状态的写入处（`SetOrderState`：确认、退回草稿、取消、完成都经过它）；已结束订单的冷热分层（协议 11） | `sales_order`、`order_line` |
| [`confirmations`](internal/confirmations/AGENTS.md) | 订单的过程（协议 04）：确认、取消、完成时释放；过程的推进与恢复 | `confirmation`、`confirmation_branch` |
| [`deliveries`](internal/deliveries/AGENTS.md) | 交货：建交货、库存出库、过账并增长可出账额度 | `delivery`、`delivery_line` |
| [`billingsource`](internal/billingsource/AGENTS.md) | 出账来源（协议 05，给出账组件 erp/billing 的 gRPC）与订单读取 | `billing_claim`、`billing_claim_line` |
| [`pricing`](internal/pricing/AGENTS.md) | 价格表；一行取哪个价（`PriceOf`） | `price_list`、`price_list_item` |
| [`orgunits`](internal/orgunits/AGENTS.md) | 部门投影（协议 02）：部门路径复制到单据上 | `org_realign`（投影本身在 SDK 的表里） |
| [`recoverycheck`](internal/recoverycheck/AGENTS.md) | 整套恢复后协议 04 的核对 | — |

模块共用的（不属于某一个业务）：[`documents`](internal/documents/AGENTS.md)（报价与订单共用的表头、行的取价与金额、单号、列表）、[`ledger`](internal/ledger/AGENTS.md)（可出账台账的事件）、[`masterdata`](internal/masterdata/AGENTS.md)（事务外向主数据取证）、[`core`](internal/core/AGENTS.md)（名字、资源与操作的定义、错误与校验、记录类型）。

| 想改 | 从这里开始 | 然后 |
| --- | --- | --- |
| 确认过程的状态与转换 | `docs/flows/confirmation.md` 与协议 04 的表 | `internal/confirmations` 的 `decide`、`step`；`itest/confirmations_integration_test.go` |
| 取价、行金额、税额 | `docs/rules/pricing.md` | `internal/documents/lines.go` 的 `BuildLines`、`internal/pricing` 的 `PriceOf`；`itest/quotations_integration_test.go` |
| 可出账额度怎样增长 | `docs/rules/pricing.md` 的“可出账金额” | `internal/deliveries` 的 `post`；服务明细在 `internal/confirmations/advance.go` 的 `finish` |
| 认领的守卫 | `docs/flows/delivery-and-billing.md` 与协议 05 | `internal/billingsource`；`itest/billingsource_integration_test.go` |
| 订单状态变化时要做的事（发事件、审计） | `internal/orders` 的 `SetOrderState`（发 `erp.sales_order.changed.v1`、记审计；本组件不通知人，要通知的组件订阅这个事件） | 取消有三条路径（草稿取消在 `internal/confirmations/cancel.go`，确认中与已确认的取消在过程结束时），都汇到这里；只有进入“确认中”不经过它（在 `internal/confirmations` 提交确认的 T1 里写） |
| 一个维度、一个操作、列表能筛选的字段 | `internal/core/names.go` | `docs/design.md` 的命令与查询、`BRICKKIT.md` 的契约索引；路由的登记在 `sales/module.go` |
| 加字段、加接口、改事件 | 装配项目 be-assembly-standard 规范里的“改动配方” | `docs/design.md` 的数据与命令 |

## 构建与测试

- `go test ./...`（`make test`）：单元测试，在 `sales/contract_test.go`：清单的 `configSchema` 与 `events`、OpenAPI 与路由、事件载荷、合并安全。
- `make test-integration`（`go test -tags integration ./itest/`）：真实的 PostgreSQL 与消息系统；每个文件测哪条流程、覆盖哪些规则、要什么前提，在 [itest/AGENTS.md](itest/AGENTS.md)。
- `brickkit lint`

## 设计取舍

- **一个 `advance`**：同步路径与恢复工作项调同一个函数；每一轮按状态向需要的分支并行发命令，应答与下一状态在一个事务里、在过程行的锁下决定；分支状态只前进不后退。恢复工作项与过程同事务入队，同步预算内用 `Wait` 让路，进程中途崩溃也有人接手。
- **订单行锁是唯一的裁决点**：交货、认领、到期都先锁订单行，同一订单的台账变化串行；表上的 CHECK 兜底 `claimed + billed ≤ billable`。
- **取消与释放复用过程表**：已确认订单的取消、完成时的释放是 `kind` 为 `cancel`、`release` 的过程，带着确认过程的分支身份。
- **部门路径复制到单据上**：SDK 的列表只按本表的列下推组织树维度，所以单据带 `department_path`，部门事件到达时改写，对齐工作项兜底；改写之前有一个窗口（投影的新鲜度）。
- **单号允许缺号**：`QT`、`SO`、`DN` 按法人、按年，在事务之前取号；幂等重放会留下缺号，这是允许的。

## 易错点

- 不要在事务里调主数据、库存或信用：先取证（`internal/masterdata`），再开事务；过程推进的调用都在 `advance` 的事务之外。
- 不要换分支身份重发：结果未知时按同一身份查询或重发（参与方按分支身份幂等）。
- 列表能排序、筛选的字段必须在资源定义的字段里声明（`internal/core/names.go` 的 `Definitions()`，报价与订单共用 `DocumentFields`），否则 SDK 答 `FIELD_NOT_QUERYABLE`。
- 不要用浮点算金额与数量：乘法经 `core.Times`（`apd`），比较与加减在 SQL 里做。

## 改代码前自查

- 改了契约（`contract/`）：proto 改后 `make -C contract gen`，发布 `contract/v…`，再改这里的依赖；OpenAPI 与路由由测试核对。
- 改了版本号吗？每次改动都升版本。

<!-- brickkit:managed:begin lang=zh -->
## BrickKit

这是一个 BrickKit 组件：平台只读 `component.yaml`。它依赖的规则：

- `configSchema` 的键就是代码读的环境变量名。不能用保留名：`COMPONENT_ID`、`COMPONENT_VERSION`、`PORT`、`BRICKKIT_SERVED_MEMBERS`、`BRICKKIT_SERVED_MEMBERS_CONFIG`，以及任何 `*_ENDPOINT`。
- 依赖写精确版本。依赖的地址以 `<ID>_ENDPOINT` 注入；缺席的可选依赖根本没有这个变量，读的时候要带兜底。
- `/healthz` 只查本进程，不查依赖。迁移命令用同一个镜像跑，遇到不认识的参数必须直接失败；`local.migrationCommand` 从源码跑同一条迁移，给 `mode: local` 用。
- `BRICKKIT.md` 会随版本进入每个使用它的项目，在那里是脱离仓库单独读的：跟代码一起改，不放相对链接。
- 发版：改 `metadata.version`，提交、推送，`brickkit release`。`brickkit lint` 会检查清单和这些文档——在项目里、在这个目录下跑，只查这个组件（`--all` 查整个项目）。
- 完整规则在 `brickkit-component` 技能里（装了技能的项目或仓库根目录下的 `.claude/skills/brickkit-component/SKILL.md`；`brickkit skills update` 会装上）；参数问 `brickkit <命令> --help`；BrickKit 自己的文档用 `brickkit docs` 看。
<!-- brickkit:managed:end -->
