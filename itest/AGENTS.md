# itest/：集成测试与测试索引

销售组件对着真实的 PostgreSQL 与消息系统、经它真实的接口（用户面 HTTP、系统面 gRPC、事件）的测试（L2）。构建标签 `integration`；一条业务流程一个文件。

## 怎样跑

- `make test-integration`（`go test -count=1 -tags integration ./itest/`），约 22 秒。
- 环境变量：`BE_SDK_TEST_PG_ADMIN=postgres://postgres:<口令>@localhost:5432/brickkit_test_db`（口令是装配项目 `.env` 的 `POSTGRES_PASSWORD`）、`BE_SDK_TEST_NATS_URL=nats://localhost:4222`；be-sdk-go 的 `sectest` 是测试部署。
- 前提：装配项目里 `make up` 起了 `be-postgres` 与 `be-nats`；`be-postgres` 里有库 `brickkit_test_db`（没有就 `docker exec be-postgres createdb -U postgres brickkit_test_db`）；测试每次开始时删掉再建自己的 schema `erp_sales_it`，跑完留着；端口 28084（用户面）、9094（系统面）与替身的 19102–19107 要空着，被占时测试直接失败。
- **只有一个测试** `TestSalesEndToEnd`：它按下表的顺序跑各步，每步把订单留在 `fx.orders` 给后面的步，所以一步不能单独跑（各步是普通的方法调用，不是 `t.Run` 子测试，没有 `-run TestSalesEndToEnd/<步>`）；改了哪个流程就跑整个（`-run TestSalesEndToEnd`）。失败时报出的文件与行指向下表的那一步。
- 授权视图是定期取的：测试中途加的授予要等下一次取到（用 `Eventually`）。

## 文件

按运行顺序：

| 文件 | 步 | 测的流程 | 覆盖的规则 | 被测的模块 |
| --- | --- | --- | --- | --- |
| `quotations_integration_test.go` | `pricingAndQuotations` | 每 EA 的价、一盒取它乘系数；报价发出、接受成同价的订单草稿；缺价、客户不合格被拒 | `docs/rules/pricing.md`；SO01 | `pricing`、`documents`、`quotations` |
| `confirmations_integration_test.go` | `confirmations` | 协议 04 的过程表：确认、处理中后恢复、参与方拒绝、丢一次应答、分支到期、确认中取消、服务订单、已确认订单的取消 | RS02–RS05、RS07–RS09、RS14；SO02–SO06、SO08–SO10 | `confirmations`、`orders` |
| `deliveries_integration_test.go` | `deliveries` | 交货身份即消耗身份；可出账按舍入增长、最后一次补齐；库存拒绝则交货失败，没应答的稍后过账 | RS19；SO12 | `deliveries`、`ledger` |
| `billingsource_integration_test.go` | `billingSource` | 以 `erp/billing` 调出账来源；两张认领抢同一行；正式化完成订单并释放分支；别的调用者被拒 | BL01、BL02、BL24、BL05/BL26；SO13、SO14 | `billingsource`、`ledger` |
| `lists_integration_test.go` | `lists` | 关键字按单号找到订单；`to_bill` 列出还有可出账的订单 | 协议 14 的列表 | `orders`、`documents` |
| `orgunits_integration_test.go` | `departments`、`laterOrgEvents` | 按部门维度读订单；部门移到另一部门之下，它的订单跟着走；不认识的动作标为不完整并补取，同一流上的法人不是部门 | 安全 07；P6.33、PJ36 | `orgunits`、`documents` |
| `lifecycle_integration_test.go` | `lifecycle` | 订单生命周期里业务流程走不到的格子：草稿取消（两次）、已取消的不能改不能确认、草稿不能交货不能认领、已完成的不能取消交货认领、有交货的不能取消、取消过程进行中不能交货不能认领且再取消不起第二个过程 | SO02、SO03、SO07、SO09、SO11–SO13、SO15 | `orders`、`confirmations`、`deliveries`、`billingsource` |
| `outbox_integration_test.go` | `outbox` | 每个事件合乎 schema，每种类型都发过 | 协议 02 | 全部发事件的模块 |
| `recoverycheck_integration_test.go` | `recoveryChecks` | 整套恢复的核对：四种对象按版本答“还在不在”（只许恢复工具）；恢复之后协议 04 的检查 | 线协议第 14 章、P14.3 | 入口 `sales/checks.go`、`recoverycheck` |
| `archive_integration_test.go` | `tiering` | 两张早已结束的订单在没有进行中的过程挡着之后封存、复制、释放；按身份照样读出；默认列表留下很早创建、仍未结束的订单，不列归档的；历史读得到 | 协议 11 的 TR10、TR30；A154、A155 | `orders`（`archive.go`） |

共用的：

| 文件 | 内容 |
| --- | --- |
| `endtoend_integration_test.go` | `TestSalesEndToEnd`（各步的顺序）；`fixture`（运行中的成员、替身、世界的 ID、步与步之间传的订单）；`start`（建世界、替身与 `mdm/org` 的流，迁移并运行成员）；辅助：`client.do`（新的幂等键）、`at`、`goods`、`service`、`newOrder`、`order`、`confirm`、`process`、`orderState`、`get`、`publishOrgUnit`、`publishOrg`、`portFree` |
| `standins_integration_test.go` | 替身，各按已发布的契约经 mTLS 答：四个主数据组件（CNY 与 USD，一美元 7.1 元）；库存与信用两个参与方可以被设成拒绝、丢一次应答、不可达、让暂存到期，驱动确认表的每一行；测试自己扮演 `mdm/org` 发部门事件 |

没有覆盖的：RS06（最后一次提交与本地确认之间崩溃，要 L3）。

## 加测试

- 订单生命周期的规则（SO 编号，`docs/design.md` 的“订单的状态”）：测它的那一步在注释里写明编号；`internal/orders` 的 `TestEveryRuleIsTested` 查每条规则都有一步写明。

- 新流程：新文件 `<流程>_integration_test.go`，写一个 `func (fx *fixture) <步>()`，在 `TestSalesEndToEnd` 里按依赖排进顺序，在上表加一行。
- 已有流程的新情形：加在那一步的文件里，更新上表的“覆盖的规则”。
- 测试不为了通过而改（装配项目规范的铁律）。
