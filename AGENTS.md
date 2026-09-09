# erp-sales · AI 助手导读

## 身份证

| 项 | 值 |
|---|---|
| 组件 ID | `erp/sales` |
| 仓库名 | `erp-sales` |
| 端口 | HTTP `8084` / gRPC `9094`（`registry/ports.tsv`，装配仓库根目录那份） |
| schema / role | `erp_sales` / `erp_sales_rw`（归档 schema `erp_sales_archive`，**本组件真的会用**，见设计计划 §7） |
| 语言 / 框架 | Go：Gin + `database/sql` + `pgx/v5/stdlib` + `sqlc` + `golang-migrate` |
| 合并部署时进 | 外壳一 `go-core` |
| 装配角色 | `default` |
| 设计真相源 | 装配仓库 `docs/design/erp-sales.md`——本文件与它冲突时，以那份为准，回来改这里 |

## 边界

**归我：** 销售报价与订单的生命周期、订单行、定价（价格表/折扣）、防超卖的**发起方**逻辑、TCC 补偿编排——设计书 §8.2 的"标准砖样板 B"。

**不归我：**
- 客户是谁、信用额度**值**：归 `mdm-customer`，建单时 `BatchGet` 校验 + 快照到订单行
- 产品是什么、单位换算：归 `mdm-product`，同上
- 库存够不够、预留判定：归 `erp-inventory`。本组件**发起** `Reserve`，但"够不够"的物理判定在库存那边的一条 SQL 条件里——**发起方不许自己算够不够**
- 应收凭证、已用额度**权威值**：归 `erp-finance`，本组件发事件，财务生成凭证
- 发货的物理执行、物流单：阶段六，本组件的 `ShipOrder` 只是状态流转 + 通知库存出库
- 审批流程本身：`infra-workflow`（阶段三，弱依赖）——workflow 严禁含业务规则，本组件发起待办、消费"审批完成"事件

`data_scopes` 声明 `org`（`dept_path` 前缀）+ `owner`（`owner_id` 相等）两维（设计书 §14.2.2），落在 `sales_orders`。⚠️ **本组件是全系统第一个真的需要 `dept_id`/`dept_path`/`owner_id` 三列的组件，而且要建在分区表上**——分区表回头加列的代价比建表时多两个数量级。`dept_id`/`dept_path` 是**创建时快照**，不是运行时查"这个人现在在哪个部门"。

## 契约面与事件

**gRPC `erp.sales.v1.SalesService`：** `CreateOrder`（建 DRAFT，不预留库存）、`ConfirmOrder`（⚠️ TCC 链在这里）、`CancelOrder`（释放预留+发事件）、`ShipOrder`（预留转实际出库）、`CalculatePriceDryRun`（前端/BFF 严禁自己算钱）、`GetOrder`/`ListOrders`/`BatchGetOrder`（防 N+1）、`GetOrderStatus`（给上游防薛定谔超时用）。

**REST 前缀：** `/erp/sales/**`。`BatchGetOrder`/`GetOrderStatus` 不暴露（组件间协议）；`CalculatePriceDryRun` **必须**暴露（前端下单页要实时试算）。

**`ConfirmOrder` 的 TCC 链**（本阶段最难的一段代码，设计计划 §3.1）：
```
① BatchGet 客户（mdm-customer）    失败 → 直接返回，无需补偿
② BatchGet 产品（mdm-product）     失败 → 直接返回，无需补偿
③ 本地缓存校验信用额度              超限 → 直接返回，无需补偿（零网络开销，刻意提前到④之前）
④ Reserve 库存（erp-inventory）    失败 → 直接返回；超时 → 见下
⑤ 建单 + PublishOutbox（同一事务）  失败 → 补偿：CancelReservation
```
④ 超时：`Reserve` 超时**严禁直接调 `CancelReservation`**，必须先 `GetReservationStatus`——`RESERVED` 当成功继续⑤，`NOT_FOUND` 安全重试或失败返回，`CANCELLED` 失败返回，查询也超时则写「待对账」表兜底。补偿连续失败 3 次 → 订单标 `SUSPENDED` + 告警，阶段二**只标状态+打日志，绝不无限重试**。

**发布事件：** `sales.order.created.v1`（⚠️ 在 `ConfirmOrder` 成功后发，不是 `CreateOrder`——草稿单不该产生应收凭证）、`sales.order.cancelled.v1`、`sales.order.shipped.v1`。

**消费事件：** `finance.credit.rejected.v1`（权威额度超限→订单转 `SUSPENDED`）、`mdm.customer.created.v1`/`.updated.v1`（维护 `customer_snapshots.credit_limit`）、`finance.voucher.posted.v1`（维护 `customer_snapshots.credit_exposure`）、`crm.opportunity.won.v1`（阶段三，本阶段只占位不实现 handler）。

## 依赖与「为什么不依赖某某」

⚠️ **本组件是全阶段二唯一有强依赖的组件**——四条边全在它身上：`mdm/customer`（`BatchGet` 校验+快照）、`mdm/product`（`BatchGet`+`ConvertQuantity`）、`erp/inventory`（TCC 四件套）、`erp/finance`（`CheckPeriodOpen`+`BatchGetCreditExposure`，改历史单/定时对账用）。弱依赖 `infra/workflow`（`optional: true`，阶段三已建成并接通：`compensateReserve` 补偿连续失败 3 次时调 `CreateTask` 建异常待办，消费 `task.completed.v1` 恢复订单，见 §3.1、§4.4.4）——缺失时 `besdk.Endpoint()` 的二值返回要判 `ok==false` 就跳过建待办只打日志。

**明确不依赖：**
- `crm-*`：§1.4 铁律，CRM 与 ERP 零同步边，仅事件握手。阶段三的赢单转订单走事件，**绝不能顺手加同步调用**
- `infra-iam-casdoor`：JWT 本地验签（决策 87）

四条依赖边全部用 `besdk.UserClient`（透传 JWT），**不许用 `SystemClient`**——这是用户请求路径，用 `SystemClient` 会绕过下游数据权限，`make gates` 有扫描守着。

## 这个组件特有的坑

| 不许 | 症状 | 出处 |
|---|---|---|
| `Reserve` 超时后直接调 `CancelReservation` | 误杀（其实已经预留成功却被撤销）或漏杀（其实失败却当成功继续）——两个方向都会把库存数字静默搞错 | 设计计划 §3.1、§4.5 |
| 把 `CANCELLED` 状态的订单拉回 `DRAFT`（照抄 Odoo 的 `action_draft()`） | 取消订单时已经跨进程释放了库存预留，"复活"要重新 `Reserve`，而那时库存可能已经被别人占走——补偿跨了服务边界就不对称 | 设计计划 §2.1 |
| `ConfirmOrder` 里先 `Reserve` 库存再校验信用额度 | 一张明显超额度的单会先占住库存再被拒、再补偿释放，白白产生一次预留+一次补偿。本地免费校验必须排在网络调用之前 | 设计计划 §3.1 |
| 库存预留失败时只让"部分商品"失败、订单照样确认（照抄 Odoo） | 我们的 `Reserve` 是跨进程 TCC 的一步，"部分成功"会让补偿语义变成"补偿一部分"，复杂度上一个数量级。**预留失败必须整单失败**，这比两个成熟系统都严，是刻意的选择 | 设计计划 §8 |
| 用户请求路径上任何一条依赖边用 `besdk.SystemClient` | 绕过下游数据权限，不报错，返回的数据只是"多了一些" | 设计计计划 §3.1；导读第 21 条 |
| 订单行不快照，改成 join `mdm-product`/`mdm-customer` 拿最新值 | 产品数据在另一个进程另一个 schema 里，**物理上 join 不到**；且改名会重写历史订单 | 设计计划 §2.2 |
| `sales_order_items` 按行自己的 `created_at` 分区 | 补加的订单行会落进和订单头不同的分区，主表归档时子表搬不干净——子表必须跟随主表，用**订单头的** `created_at` | 设计计划 §2、§7 |
| 补偿失败时无限重试 | "补偿的补偿"死循环比不补偿更糟。连续失败 3 次必须标 `SUSPENDED` + 告警，阶段二只打日志，阶段三接 `infra-workflow` 建异常待办 | 设计计划 §3.1、§4.4.4 |
| 弱依赖 `infra/workflow` 缺失时把它的 endpoint 当空字符串处理 | 平台对缺失的弱依赖**不注入这个变量**，不是空串——下标/字符串比较判空会得到错误结论。必须用 `besdk.Endpoint()` 的二值返回判 `ok` | 设计计划 §5、总纲 §3.6 |
| 给 `dependencies.components` 加 `crm-*` 或任何非四条强依赖之外的边 | CRM 与 ERP 零同步边是硬铁律，阶段三的赢单转订单走事件 | §1.4 铁律、设计计划 §5 |
| 写弱依赖 `infra/workflow` 时省略 `@版本号`（想着"反正它还不存在，写个版本也没意义"） | `brickkit up`/生成阶段直接报 `MANIFEST_INVALID`：**弱依赖照样要求 `id@精确版本` 的格式**，`optional: true` 只影响"解析不到时警告而不是报错"这个阶段，不影响"引用本身必须写成合法格式"这个更早的 schema 校验。真机跑 `brickkit up --dry-run` 才发现——补版本号（`infra/workflow@1.0.0`，跟其它组件的初版号一致）后警告降级为"弱依赖缺失"，不阻断 | Task 16 实测；brickKit `internal/manifest/parse.go` |
| 调 `infra-workflow` 的 `CloseTask` 来关闭补偿异常待办 | `CloseTask` 是组件间协议（人代表业务组件创建/关闭待办等于绕过业务规则）——异常待办由**人**在 `infra-workflow` 的"我的待办"UI 里点"同意"来关闭，本组件只**消费** `task.completed.v1`，永远不会、也不许主动调 `CloseTask` | 设计计划 §3.1、§4.4.4 |
| `ResumeFromExceptionTx` 只判 `status == SUSPENDED` 就恢复，不核对 `suspended_reason` | 一张后来被权威额度判定（`finance.credit.rejected.v1`）重新标了 `SUSPENDED`（`suspended_reason` 已被覆盖）的订单会被这条事件误恢复——两条 `SUSPENDED` 来源共用同一个 `status` 值，`reason` 是唯一能区分"这次挂起是不是我发起那条待办对应的那次"的依据 | 设计计划 §3.1、`repo/suspend.go` 的 `ExceptionReasonCompensationFailed` |
| `infra.workflow.task.completed.v1` 消费者不过滤 `source_component`/`source_aggregate` | 这个 subject 是全平台共用的，其它业务组件发起的待办完成事件也会广播到这里——不过滤会拿别的组件的 `source_id` 当自己的订单 id 去查，要么查不到报错，要么（更糟）撞上一个恰好存在的无关订单 id | `consumer/consumer.go` 的 `workflowTaskCompletedHandler` |

## 改代码前的自查

1. **我是不是在 `Reserve` 超时后直接调 `CancelReservation`？** 停下——必须先 `GetReservationStatus`。
2. **我是不是把信用额度校验排在 `Reserve` 之后？** 停下——本地免费校验必须在网络调用之前。
3. **我是不是在让库存预留"部分成功"？** 停下——预留失败必须整单失败，不允许部分商品跳过。
4. **我调依赖组件用的是 `UserClient` 还是 `SystemClient`？** 用户请求路径上只许 `UserClient`。
5. **我是不是把订单行改成 join 产品/客户拿最新值，而不是读快照？** 停下——历史订单要读当时的快照，不是当前值。
6. **这个改动会不会让 `contracts/sales.proto` 出现破坏性变更？** 下游（阶段三的 `crm-opportunity`、BFF）都会消费这份契约，只能向后兼容地追加。
7. **我是不是想让本组件主动调 `infra-workflow` 的 `CloseTask`？** 停下——那是组件间协议，异常待办只能由人在 `infra-workflow` 自己的 UI 里关闭，本组件永远只是 `task.completed.v1` 的消费者。
8. **我改 `ResumeFromExceptionTx` 时是不是去掉了 `suspended_reason` 精确匹配这道判据？** 停下——只判 `status == SUSPENDED` 会误恢复一张实际因权威额度超限被挂起的订单。
