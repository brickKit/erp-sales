# erp/sales

销售组件的代码。用法与边界在 `BRICKKIT.md`，为什么这样设计（术语、数据、状态、规则、未决问题）在 [docs/design.md](docs/design.md)；细则在 [docs/flows/confirmation.md](docs/flows/confirmation.md)（确认过程）、[docs/rules/pricing.md](docs/rules/pricing.md)（取价与金额）、[docs/flows/delivery-and-billing.md](docs/flows/delivery-and-billing.md)（交货与出账认领）。跨组件的规则在装配项目 be-assembly-standard 的协议 04（本组件是协调者）、协议 05（本组件是可出账额度的属主）、协议 02（部门投影）、协议 07（资格取证）、协议 14（单号、金额、汇率、列表）、安全 07（三个维度）。

## 代码地图

| 位置 | 内容 |
| --- | --- |
| `sales/definition.go` | 组件定义：身份、依赖（库存与信用可选）、流与事件类型、资源与操作、三个维度、参数（同步预算、W、认领期限） |
| `sales/module.go` | 路由与系统面的注册、本组件的错误原因、订阅与后台工作的声明、`check`、`decimal`、审计与事件的写法 |
| `sales/upstream.go` | 事务外的取证：法人、部门、客户、产品的资格与快照、换算（`convert`）、即期汇率、币种小数位 |
| `sales/orgunits.go` | 部门投影（`org_units`）：订阅、按需补取、每小时对齐；部门路径复制到单据上并随事件改写（`repath`） |
| `sales/pricing.go` | 价格表与价格；`priceOf`（销售单位的价，否则基本单位的价乘系数） |
| `sales/lines.go` | 报价与订单的行：输入的检查、取证、取价与金额（`buildLines`）、整份替换（`writeLines`） |
| `sales/documents.go` | 报价与订单共用的部分：表头的取证与授权（`prepare`、`patchHead`）、部门（`departmentOf`）、单号（`number`）、读取 |
| `sales/quotations.go` | 报价：新建、修改、读取、发出、取消、转订单草稿 |
| `sales/orders.go` | 订单草稿：新建、读取、修改（确认中拒绝，RS09） |
| `sales/confirmation.go` | 订单确认：过程的类型与读取、提交确认（取证、T1）、过程的查询 |
| `sales/advance.go` | 过程的推进：`advance`（同步路径与恢复工作项共用）、`send`、`step`、`decide`（协议 04 的过程表）、`finish`、恢复工作项与死信 |
| `sales/cancel.go` | 取消订单；种类为 `cancel`、`release` 的过程（`startBranchProcess`） |
| `sales/delivery.go` | 交货：T1 建交货、库存出库（交货身份即消耗身份）、T2 过账与可出账台账；恢复工作项 |
| `sales/billing.go` | 出账来源（`BillingSourceService`）：台账、认领、续期、确认、正式化、释放、到期；订单完成；`OrderService.Read` |
| `sales/lists.go` | 报价、订单、交货的列表 |
| `sales/records.go` | 记录的类型、读取的 SQL、`emitOrder` |
| `sales/migrations/` | 表 |
| `cmd/seed/` | 种子：角色与三个销售账号的范围、价格表、各种状态的报价与订单（含确认被拒的反例）、交货（`make seed`） |
| `contract/` | 本组件发布的契约，单独的模块：`contract/proto/`、`contract/events/`、`contract/openapi.yaml` |
| `docs/` | 设计与细则 |

| 想改 | 从这里开始 | 然后 |
| --- | --- | --- |
| 确认过程的状态与转换 | `docs/flows/confirmation.md` 与协议 04 的表 | `advance.go` 的 `decide`、`step`；集成测试的 `confirmations` 一步 |
| 取价、行金额、税额 | `docs/rules/pricing.md` | `lines.go` 的 `buildLines`、`pricing.go` 的 `priceOf` |
| 可出账额度怎样增长 | `docs/rules/pricing.md` 的“可出账金额” | `delivery.go` 的 `post`；服务明细在 `advance.go` 的 `finish` |
| 认领的守卫 | `docs/flows/delivery-and-billing.md` 与协议 05 | `billing.go`；集成测试的 `billingSource` 一步 |
| 一个维度、一个操作 | `definition.go` | `docs/design.md` 的命令与查询、`BRICKKIT.md` 的契约索引 |
| 加字段、加接口、改事件 | 装配项目 be-assembly-standard 规范里的“改动配方” | `docs/design.md` 的数据与命令 |

## 构建与测试

- `go test ./...`（`make test`）：清单的 `configSchema` 与 `events`、OpenAPI 与路由、事件载荷、合并安全。
- `go test -tags integration ./sales/`（`make test-integration`）：真实的 PostgreSQL 与消息系统，例如 `BE_SDK_TEST_PG_ADMIN=postgres://postgres:<口令>@localhost:5432/brickkit_test_db`（口令是装配项目 `.env` 的 `POSTGRES_PASSWORD`）与 `BE_SDK_TEST_NATS_URL=nats://localhost:4222`；be-sdk-go 的 `sectest` 是测试部署；测试按步骤写在 `flows_integration_test.go`（取价与报价、确认、交货、出账来源、列表、部门、发件表），启动与辅助在 `sales_integration_test.go`；四个主数据组件与库存、信用是按契约写的替身（`standins_integration_test.go`），库存与信用的替身可以拒绝、丢一次应答、不可达、让暂存到期；测试以 `erp/billing` 的身份调出账来源，自己扮演 `mdm/org` 发部门事件。
- 集成测试的前提：装配项目里 `make up` 起了 `be-postgres` 与 `be-nats`；`be-postgres` 里有库 `brickkit_test_db`（没有就 `docker exec be-postgres createdb -U postgres brickkit_test_db`）；测试每次开始时删掉再建自己的 schema `erp_sales_it`，跑完留着；端口 28084、9094 与替身的 19102–19107 要空着，被占时测试直接失败。
- 授权视图是定期取的：测试中途加的授予要等下一次取到（集成测试用 `Eventually`）。
- `brickkit lint`

## 设计取舍

- **一个 `advance`**：同步路径与恢复工作项调同一个函数；每一轮按状态向需要的分支并行发命令，应答与下一状态在一个事务里、在过程行的锁下决定；分支状态只前进不后退。恢复工作项与过程同事务入队，同步预算内用 `Wait` 让路，进程中途崩溃也有人接手。
- **订单行锁是唯一的裁决点**：交货、认领、到期都先锁订单行，同一订单的台账变化串行；表上的 CHECK 兜底 `claimed + billed ≤ billable`。
- **取消与释放复用过程表**：已确认订单的取消、完成时的释放是 `kind` 为 `cancel`、`release` 的过程，带着确认过程的分支身份。
- **部门路径复制到单据上**：SDK 的列表只按本表的列下推组织树维度，所以单据带 `department_path`，部门事件到达时改写，对齐工作项兜底；改写之前有一个窗口（投影的新鲜度）。
- **单号允许缺号**：`QT`、`SO`、`DN` 按法人、按年，在事务之前取号；幂等重放会留下缺号，这是允许的。

## 易错点

- 不要在事务里调主数据、库存或信用：先取证（`upstream.go`），再开事务；过程推进的调用都在 `advance` 的事务之外。
- 不要换分支身份重发：结果未知时按同一身份查询或重发（参与方按分支身份幂等）。
- 列表能排序、筛选的字段必须在资源定义的字段里声明（`definition.go` 的 `documentFields`），否则 SDK 答 `FIELD_NOT_QUERYABLE`。
- 不要用浮点算金额与数量：乘法经 `times`（`apd`），比较与加减在 SQL 里做。

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
