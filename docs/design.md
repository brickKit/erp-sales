# erp/sales 的设计

本页写这个组件为什么这样设计：术语、拥有的数据、状态、规则要点、命令与事件、依赖、实现状态与未决问题。写给要修改它的人与 AI。怎样使用、部署与配置在 `BRICKKIT.md`；代码在哪、怎样构建与测试在 `AGENTS.md`。

细则拆在三份文档里：

| 要改的 | 读 |
| --- | --- |
| 订单确认：过程的状态、同步路径与恢复、取消与完成时怎样释放分支 | [flows/confirmation.md](flows/confirmation.md) |
| 单价、行金额、税额、汇率与币种的取法 | [rules/pricing.md](rules/pricing.md) |
| 发起交货、库存出库、可出账台账与出账的认领 | [flows/delivery-and-billing.md](flows/delivery-and-billing.md) |

跨组件的规则在装配项目：确认过程与预占分支在协议 04（本组件是协调者），认领模式与可出账台账在协议 05，引用与资格在协议 07，单号、金额与汇率在协议 14，数据范围在安全 07；端到端的位置见装配项目的业务流程“订单到收款”。

## 边界

销售的报价、订单与它们的执行进度；订单确认时协调库存与信用的预占；对出账来源额度的裁决。

| 不在这里 | 归谁 |
| --- | --- |
| 库存余额、出库流水 | `erp/inventory`（经它的契约预占与出库） |
| 信用额度与占用 | `erp/credit` |
| 账单、发票编号 | `erp/billing`（向本组件认领可出账额度） |
| 应收、收款 | `erp/finance`、`erp/payment` |
| 税额的计算 | `erp/tax`（远期；缺时税率由人录入） |
| 商机、CRM 的客户关系 | `crm/opportunity`、`crm/customer`（只经事件） |
| 客户、产品本身 | `mdm/partner`、`mdm/product`（引用时快照显示字段） |

## 术语

| 业务叫法 | 定义 | 代码标识符 |
| --- | --- | --- |
| 报价 | 给客户的价格与数量提议，可以转成订单；不预占任何东西 | `quotation` |
| 商机参考、请求报价 | `crm/opportunity` 的商机发布“请求报价”的事实，本组件生成带商机参考的报价草稿；报价转订单时商机参考带到订单（A71） | `opportunity_ref`、`quote_request_id`；表 `quote_request_in` |
| 请求的产品 | 请求报价带来的产品、数量、单位，放在草稿上给销售人员参考，由人补成带价格与税率的明细 | `requested_products` |
| 销售订单与明细 | 一个法人对一个客户的订单；明细（也叫订单行）是实物（库存商品）或服务 | `sales_order`、`order_line`；`line_kind`：`goods`、`service` |
| 订单负责人 | 销售自己拥有的字段，按负责人限定数据范围用它 | `owner_id` |
| 订单部门 | 订单归属的部门（`mdm/org` 的部门），按部门限定数据范围用它 | `department_id` |
| 价格表 | 按法人、币种的产品单价（不含税），带生效日期 | `price_list`、`price_list_item` |
| 定价规则 | 折扣、阶梯价、客户价（替换点，以后） | — |
| 确认过程 | 一次确认的持久记录：参与方、分支身份、每一步的状态（协议 04）；已确认订单的取消与完成时的释放也是过程（种类 `cancel`、`release`），带着确认过程的分支身份 | `confirmation`；种类 `confirm`、`cancel`、`release`；状态 `reserving`、`committing`、`confirmed`、`rejecting`、`rejected`、`cancelling`、`cancelled` |
| 交货 | 一次发货：订单的若干实物明细与数量，经库存出库 | `delivery`；状态 `issuing`、`posted`、`failed` |
| 可出账台账 | 每个订单行的 `billable`、`claimed`、`billed`，数量与金额两维（协议 05）；存在订单行上 | `order_line` 的 `billable_qty`、`billable_amt` 等六列 |
| 出账认领 | 出账组件对可出账额度的认领，身份由出账组件生成 | `billing_claim`；状态 `pending`、`confirmed`、`formalized`、`expired`、`released` |
| 修订 | 已确认订单的修改，重审后对分支发调整（以后，D33） | `revision` |
| 销售退货单、可贷记台账 | 退货、折让、换货、取消（含退货授权）；可贷记台账与原订单行的贷记汇总；待处理退回量（以后，协议 05） | — |
| 款到发货、直运、寄售 | 以后 | — |

## 拥有的数据

| 表 | 内容 | 终态与保留 |
| --- | --- | --- |
| `quotation`、`quotation_line` | 报价：单号、法人、客户、币种、有效期、负责人、部门、状态、商机参考、请求身份、请求的产品；明细同订单 | `accepted`、`cancelled`、`expired`；不删除 |
| `sales_order` | 订单：单号、法人、客户（与快照的名称、税号）、币种与汇率快照、业务日期、负责人、部门（与复制的部门路径）、状态、修订、合计（不含税、税额、含税）、版本；商机参考不存在订单上，按 `quotation_id` 读它的报价（报价不删除也不归档；订单表加列会改变已结束订单这一数据类别的结构，已归档的旧单元要登记转换器才读得出，SDK 的组件接口还登记不了） |
| `quote_request_in` | 收到的请求报价：请求身份（即商机事实的身份，主键）、商机、载荷、结果 `received`、`drafted`（报价身份）、`rejected`（原因） | 终态保留，作去重依据 | `completed`、`cancelled`；不删除 |
| `order_line` | 明细：种类、产品与快照（代码、名称、单位）、数量与销售单位、基本单位数量、仓库、单价、税率、行金额、税额、已交货数量；可出账台账的两维六列（带 CHECK：每维 `claimed + billed ≤ billable`） | 随订单 |
| `confirmation`、`confirmation_branch` | 过程：订单与修订、种类、状态、原因、操作者；每个参与方的分支身份与最后知道的状态（请求的幂等键在 SDK 的幂等表） | 终态保留 |
| `delivery`、`delivery_line` | 交货：单号、订单、法人、部门（`department_path` 是复制来的，随部门事件改写）、状态、库存消耗身份、明细（订单行、数量、批次或序列号） | `posted`、`failed`；不删除 |
| `billing_claim`、`billing_claim_line` | 出账认领：认领身份、订单、状态、期限、每行认领与正式化的数量与金额 | 终态保留，作幂等依据 |
| `price_list`、`price_list_item` | 价格表 | 停用，不删除 |
| 部门投影 `org_units`（SDK 的投影表） | `mdm/org` 部门的投影（身份 → 路径、法人、状态）：取部门的路径、检查部门可用；部门移动时改写单据上复制的路径。按聚合订阅部门的全部事件（`mdm.org_unit.>`，订阅名 `org-unit-events`）：`mdm.org_unit.changed.v1` 按它的投影语义处理；不认识的动作或主版本（来源以后加的）不取它的载荷，把那个部门标为不完整，由修复工作项向 `mdm/org` 取回（协议 02 的 PJ36、线协议 P6.33） | 随来源 |

已结束的订单（`completed`、`cancelled`）连同它的明细、确认过程与分支、交货与交货行、出账认领与认领行是一个数据类别 `erp.sales.orders`（装配项目协议 11，决定 A154、A155）：按法人与最后变化的月分组，结束满一年封存、转层（同库的温层）、释放在线行；归档的订单按身份照样能读，形状不变。保留期由部署给（`TIERING_RETENTION_ERP_SALES_ORDERS`，商法与税法按国家定，客户在上线时定），没给的从不删除。

以后还有：修订历史、销售退货单与可贷记台账、款到发货的放行认领、定价规则。

## 订单的状态

订单的生命周期是一张表：每个**阶段**（存下的状态，外加“已确认且取消过程进行中”的 `cancelling`）× 每个**命令**一格，格里写这个命令在这个阶段做什么与规则编号。代码里是同一张表（`internal/orders/lifecycle.go`），单元测试逐格比较这里与代码，不一致就失败；加一个状态或命令，就是加一列或一行，每一格都要定下来。

格的写法：`允许`（照常做，订单状态不变）、`拒绝`（文字在代码的表里）、`忽略`（什么也不做，照已做完回答）、`→ <状态>`（做完订单变成这个状态）、`经确认过程`、`经取消过程`（订单在过程结束时变）。

| 命令 | `draft` | `confirming` | `confirmed` | `cancelling` | `completed` | `cancelled` |
| --- | --- | --- | --- | --- | --- | --- |
| 修改 `edit` | 允许 SO02 | 拒绝 SO02 | 拒绝 SO02 | 拒绝 SO02 | 拒绝 SO02 | 拒绝 SO02 |
| 提交确认 `confirm` | → `confirming` SO03 | 拒绝 SO03 | 拒绝 SO03 | 拒绝 SO03 | 拒绝 SO03 | 拒绝 SO03 |
| 取消 `cancel` | → `cancelled` SO07 | 经确认过程 SO08 | 经取消过程 SO09 | 忽略 SO11 | 拒绝 SO15 | 忽略 SO11 |
| 交货 `deliver` | 拒绝 SO12 | 拒绝 SO12 | 允许 SO12 | 拒绝 SO12 | 拒绝 SO12 | 拒绝 SO12 |
| 出账认领 `claim` | 拒绝 SO13 | 拒绝 SO13 | 允许 SO13 | 拒绝 SO13 | 拒绝 SO13 | 拒绝 SO13 |
| 完成 `complete` | 忽略 SO14 | 忽略 SO14 | → `completed` SO14 | 忽略 SO14 | 忽略 SO14 | 忽略 SO14 |
| 确认过程 `confirmed` `confirmation_confirmed` | 忽略 SO04 | → `confirmed` SO04 | 忽略 SO04 | 忽略 SO04 | 忽略 SO04 | 忽略 SO04 |
| 确认过程 `rejected` `confirmation_rejected` | 忽略 SO05 | → `draft` SO05 | 忽略 SO05 | 忽略 SO05 | 忽略 SO05 | 忽略 SO05 |
| 确认过程 `cancelled` `confirmation_cancelled` | 忽略 SO06 | → `cancelled` SO06 | 忽略 SO06 | 忽略 SO06 | 忽略 SO06 | 忽略 SO06 |
| 取消过程结束 `cancel_ended` | 忽略 SO10 | 忽略 SO10 | → `cancelled` SO10 | → `cancelled` SO10 | 忽略 SO10 | 忽略 SO10 |

规则目录（订单的生命周期；跨组件的规则用协议的编号：协议 04 的 RS、协议 05 的 BL）：

| 规则 | 内容 | 代码 | 测试 |
| --- | --- | --- | --- |
| SO01 | 新建与报价转订单得到 `draft`（表的默认值） | `orders.CreateOrder`、`quotations.AcceptQuotation` | `quotations`、`confirmations` 两步 |
| SO02 | 只有草稿能修改；确认中的拒绝（RS09） | `orders.UpdateOrder` | `confirmations`、`lifecycle` |
| SO03 | 只有草稿能提交确认；提交时订单与取证时相同（版本），否则重新取证；订单进入 `confirming`。提交本身不审计，确认的结果审计 | `confirmations.startConfirmation` | `confirmations`、`lifecycle` |
| SO04 | 确认过程 `confirmed`：订单 `confirmed`，服务明细整行可出账（`ledger.ServiceConfirmed`）；只认订单当前的确认过程 | `confirmations.finish` | `confirmations` |
| SO05 | 确认过程 `rejected`（参与方拒绝、处理超时）：订单回到 `draft`，附上次的原因，可以改了再确认 | `confirmations.finish` | `confirmations` |
| SO06 | 确认中被取消，确认过程 `cancelled`：订单 `cancelled` | `confirmations.finish` | `confirmations` |
| SO07 | 草稿取消立即 `cancelled` | `confirmations.CancelOrder` | `lifecycle` |
| SO08 | 确认中取消：确认过程转 `cancelling`，对全部参与方发取消（RS04） | `confirmations.CancelOrder` | `confirmations` |
| SO09 | 已确认的取消：没有交货、没有出账（认领或已出账）才行，经取消过程对全部分支发取消；否则拒绝（已交货的走退货，关闭未交货部分以后提供，RS13） | `confirmations.CancelOrder` | `confirmations`、`lifecycle` |
| SO10 | 取消过程结束：订单 `cancelled` | `confirmations.finish` | `confirmations` |
| SO11 | 取消中、已取消的再取消：照原样回答，不起第二个过程 | `confirmations.CancelOrder` | `lifecycle` |
| SO12 | 只有已确认、不在取消中的订单能交货 | `deliveries.CreateDelivery` | `deliveries`、`lifecycle` |
| SO13 | 只有已确认、不在取消中的订单能被认领，否则答问题 `order_not_billable` | `billingsource.Claim` | `billingSource`、`lifecycle` |
| SO14 | 全部明细交完并全部出账：订单 `completed`，释放剩余分支 | `billingsource.completeIfDone` | `billingSource` |
| SO15 | 已完成的订单不能取消 | `confirmations.CancelOrder` | `lifecycle` |

以后（不在本版）：已确认订单的关闭未交货部分（`closed`：对分支发取消，只释放未消耗的部分，协议 04 的 RS13；可出账台账的 `billable` 缩到已交货，不能低于 `claimed + billed`，否则拒绝，BL03；款到发货时向财务冲回放行额度，协议 05；已交货的部分出完账后订单结束）；修订。

交货进度与出账进度不是订单状态，是明细上的量（已交货、可出账、已出账），列表可以按它们筛选。

## 商机的请求报价

`crm/opportunity` 与本组件只经事件协作（边界卡）：本组件订阅它的 `crm.opportunity.quote_requested.v1`，不声明它为依赖（两边都声明会把精确版本互相钉住）；商机没装时订阅的流是空的，什么也不发生，报价照常手工创建。

| 规则 | 内容 | 代码 | 测试 |
| --- | --- | --- | --- |
| QR01 | 收到请求：按请求身份（事实身份）插入 `quote_request_in`（`received`），已有的什么也不做——重复投递只生成一张草稿；同一事务排工作 `draft-quotation`。订阅的处理在 SDK 的事务里，不调用别的组件 | `quoterequests.OnRequested` | `quoteRequests` |
| QR02 | 工作在事务外取证：法人可用、币种可用、客户在该法人有资格（用途 `sell`），同报价的新建；主数据不可达时工作重试。取证之后一个事务：请求已不是 `received` 的什么也不做 | `quoterequests.DraftJob` | `quoteRequests` |
| QR03 | 都可用：生成报价草稿——法人、客户（快照）、币种、负责人取请求的，部门留空，有效期留空，没有明细；记商机参考与请求身份，请求的产品原样放在草稿上（不校验：产品与价格在人补明细时按 `rules/pricing.md` 校验）；请求变 `drafted`；报价的事件与审计（没有用户操作者：是本组件自己做的）同一事务。草稿的负责人看得到它（数据范围按负责人） | `quoterequests.DraftJob` | `quoteRequests` |
| QR04 | 有一项不可用：不生成草稿，请求变 `rejected`，发布事实 `erp.quote_request.rejected.v1`（事实身份 = 请求身份；商机、原因 `LEGAL_ENTITY_NOT_USABLE`、`CURRENCY_NOT_USABLE` 或 `NOT_ELIGIBLE` 与说明） | `quoterequests.DraftJob` | `quoteRequests` |
| QR05 | 没有明细的报价只能是草稿：发出要至少一行（`REQUEST_INVALID`）；修改时给明细就整体替换，照常至少一行。报价接受转订单时商机参考带到订单 | `quotations.QuotationAction`、`quotations.AcceptQuotation` | `quoteRequests` |

## 规则要点

- 订单的确认只有一个过程实例；只有过程进入 `confirmed` 之后订单才算确认（协议 04）。确认前，在事务外取证：法人启用（`mdm/org` 来源读取）、客户的资格（`mdm/partner`，用途 `sell`）、每个产品的资格（`mdm/product`，用途 `sell`）、外币订单的汇率；资格在 W（30 秒）内提交。细节在 [flows/confirmation.md](flows/confirmation.md)。
- 参与方在过程创建时确定：有实物明细就有库存；装了信用就有信用。协议 04 参与方表的“客户没有信用控制则信用不参加”，本项目由信用组件表达（金额为空的额度行：参加、记占用、不拒绝），销售不判断客户是否受控制。只有服务明细的订单不请求库存。
- 可出账额度只由销售裁决；`claimed + billed ≤ billable`，两维各自成立（协议 05）。本版的出账政策是“按交货”：交货过账时 `billable` 增加。细节在 [flows/delivery-and-billing.md](flows/delivery-and-billing.md)。
- 交货数量不超过订单数量减已交货、减交货中；交货经库存出库并消耗分支，库存的出库是事实的权威。
- 单价、金额与税额的取法在 [rules/pricing.md](rules/pricing.md)。
- 以后：已确认订单的修订按协议 04 调整各分支，增加的部分不足则修订不生效，已执行的明细只能修订未执行部分；同一原订单行的贷记汇总分两维，数量维（只算退货）与金额维（退货与折让）各自的认领中加已贷记不超过已出账（协议 05）。
- 已结束的订单在这些时候不封存（装配项目协议 11 的阻塞检查，`internal/orders/archive.go`）：还有进行中的确认、取消或释放过程（预占没结束），还有未结的出账认领（`pending`、`confirmed`），还有出库中的交货（`issuing`）；阻塞的原因记在单元上，过了驻留期 30 天仍阻塞的告警。
- 订单列表的默认范围是“未结束，或最近 N 天创建”（协议 11 的 TR10，N 是 `TIERING_LIST_DAYS`，默认 90）：很早创建、仍未结束的订单照样列出；要全部历史（含已归档的）加 `history=true`，时限内一次答完，超时转成导出。
- 单号：报价 `QT`、订单 `SO`、交货 `DN` 都是按法人、按年的允许缺号的序列，新建时分配（订单号不是法定连续的单据号）；格式 `<前缀>-<法人代码>-<年>-<六位序号>`，本版固定，以后作配置项。

## 命令与查询

| 命令或查询 | 面 | 资源与操作 |
| --- | --- | --- |
| 报价的新建、修改、发出、取消；报价转订单 | 用户面 | `erp.sales.quotation` 的 `erp.sales.quotation.edit` |
| 报价列表与读取 | 用户面 | `erp.sales.quotation.read` |
| 订单的新建与修改（草稿） | 用户面 | `erp.sales.order` 的 `erp.sales.order.edit` |
| 提交确认（带 `Idempotency-Key`；返回已确认、未确认或处理中与过程身份） | 用户面 | `erp.sales.order.confirm` |
| 取消订单 | 用户面 | `erp.sales.order.cancel` |
| 订单列表、读取、确认过程的查询 | 用户面 | `erp.sales.order.read` |
| 发起交货 | 用户面 | `erp.sales.delivery` 的 `erp.sales.delivery.create` |
| 交货列表与读取 | 用户面 | `erp.sales.delivery.read` |
| 价格表的维护与读取 | 用户面 | `erp.sales.price_list` 的 `erp.sales.price_list.manage`、`erp.sales.price_list.read` |
| 可出账台账的读取与出账认领的认领、续期、确认、正式化、释放（出账来源，协议 05；冲回以后） | 系统面 `BillingSourceService` | `erp.sales.system` 的 `erp.sales.system.billing`，只授予 `erp/billing` 的服务身份 |
| 订单的系统读取（`OrderService.Read`：出账取明细与打印数据） | 系统面 | `erp.sales.system.read` |
| 修订、关闭未交货部分、退货与折让、款到发货、服务完成的登记、期初导入 | — | 以后（契约已有修订、关闭、退货的路径；退货的操作 `erp.sales.return.edit`） |

数据范围：报价、订单与交货的资源带三个维度：法人（值集合）、部门（组织树，安全 07）、负责人（主体，“销售只看自己的订单”）。交货继承订单的三个维度。部门维度按单据上复制的部门路径（`department_path`）编译：SDK 的列表只按本表的列下推组织树维度，所以路径随单据保存，部门移动的事件到达时改写（不分单据状态，已完成、已取消的也改：范围总按部门现在的位置），每小时的对齐兜底；部门退役不改路径，单据照旧。部门移动到改写之间有一个窗口：平时是事件投递的时间（L3 里不到 1 秒），事件丢了最长到下一次对齐（1 小时），记在未决问题。

## 事件

流 `erp.sales`：

- 完整状态事件（可覆盖）：报价 `erp.quotation.changed.v1`（带商机参考、请求身份、请求的产品）、订单 `erp.sales_order.changed.v1`（带商机参考；也是 `OrderService.Read` 的数据）、交货 `erp.delivery.changed.v1`、价格表 `erp.price_list.changed.v1` 与价格 `erp.price_list_item.changed.v1`。
- 增量事实：确认结束 `erp.sales_order.confirmation_ended.v1`（事实身份是过程身份，结果与原因）；交货已过账 `erp.delivery.posted.v1`（以后复杂仓储订阅“交货请求”）；可出账台账变化 `erp.billable.changed.v1`（协议 05，供出账显示与对账）；拒绝请求报价 `erp.quote_request.rejected.v1`（QR04，供商机标记请求）。

订阅：`mdm/org` 的部门事件（部门投影）；`crm/opportunity` 的 `crm.opportunity.quote_requested.v1`（QR01，不声明依赖）。

## 依赖

| 依赖 | 必需或可选 | 用来做什么 | 缺席时 |
| --- | --- | --- | --- |
| `mdm/partner` | 必需 | 客户资格（`sell`）；名称与税号的快照 | — |
| `mdm/product` | 必需 | 产品资格（`sell`，回答带是否参与库存）；单位换算；代码、名称、单位的快照 | — |
| `mdm/org` | 必需 | 法人的证据与本位币；部门的投影 | — |
| `mdm/currency` | 必需 | 外币订单在确认时的汇率 | — |
| `erp/inventory` | 可选 | 实物明细的预占与出库 | 实物订单不能确认（答“依赖不可用”，装配时就该禁止，协议 04 的 RS15）；服务订单照常 |
| `erp/credit` | 可选 | 信用预占 | 没有信用控制，过程没有信用分支 |
| 身份、授权 | 必需 | — | — |
| `infra/audit` | 可选 | 确认、取消的审计 | 只写本地日志 |

下游：`erp/billing`（认领可出账额度；缺时不能出账）；`infra/print`（不直接依赖，账单的打印在出账组件）。`crm/opportunity`（只经事件，不声明依赖：订阅它的请求报价，它订阅本组件的报价、订单与拒绝请求的事件）。以后：`erp/tax`、`erp/contract`、`erp/finance`（款到发货）、`crm/customer`（只经事件）、`infra/workflow`（审批）、物流承运与电商平台的成员。

## 实现状态

3.0.50（装配项目阶段计划 C6 的 P2b）：商机的请求报价（QR01–QR05）：订阅、去重、草稿与拒绝的事实；报价与订单带商机参考（契约 `contract/v3.0.0-alpha.4`）。

3.0.49：依赖换到 erp/inventory 3.0.39（按计价序号读流水，装配项目阶段计划 C5 的 P0b）及随之连锁的版本；行为不变。

3.0.48：依赖换到 mdm/product 3.0.38（种子让四个服务可采购；装配项目阶段计划 C2 的 P5），行为不变

3.0.47：依赖换到 mdm/partner 3.0.40（种子开供应商角色；装配项目阶段计划 C2 的 P4a），行为不变

3.0.46：依赖换到 mdm/partner 3.0.39（供应商角色）与 erp/inventory 3.0.37（采购收货与退货；装配项目阶段计划 C2 的 P0b），行为不变

3.0.45：be-sdk-go alpha.121（全系统同一个 SDK 版本；sectest 的授权组件替身答 CheckLinkChange 与 Administrators）

3.0.44：依赖换到 mdm/org 3.0.34（岗位；装配项目阶段计划 C1 的 E0b），行为不变

3.0.43：be-sdk-go alpha.119（全系统同一个 SDK 版本；订阅最后一次投递处理之前先提交死信，进程中途退出时消息不再无声消失，P6.20）

3.0.42：be-sdk-go alpha.117（全系统同一个 SDK 版本；SDK 加了预占的参与方 `reserve`，线协议第 16 章）

3.0.41：合并安全的单元测试扩到 `internal/` 下全部包（分层以来只查入口包，装配项目的结构问题 SF24）；依赖换到分层之后的版本：`mdm/currency@3.0.30`、`mdm/org@3.0.31`、`mdm/partner@3.0.34`、`mdm/product@3.0.33`、`erp/credit@3.0.30`、`erp/inventory@3.0.30`。

3.0.40：订单的规则集中一处，行为不变：生命周期是一张表（`internal/orders/lifecycle.go`，与本页“订单的状态”逐格相同，单元测试比较），订单状态只经 `Transition` 写；可出账台账只经 `internal/ledger` 的五个函数写；入口包的结构检查守住这两条，并登记表以外还读订单状态的地方；新的集成测试步骤 `lifecycle` 锁住业务流程走不到的格子。

3.0.39：代码按业务模块分层，行为不变：入口包 `sales/` 只组装，业务在 `internal/` 下的十二个包，集成测试在 `itest/`、按业务流程分文件；每个代码目录一份 `AGENTS.md`（进入目录之前先读）。

3.0.38：be-sdk-go alpha.116（全系统同一个 SDK 版本；冷热数据：每天的孤儿清理、擦除作废导出、已销毁的记录从决定起答不存在、释放在事务之外读备份记录）

3.0.37（底层先行第 5 项 T11）：按身份读订单先问归档，单元仍在线才读在线表（线协议 P15.22）：释放与销毁分批删除在线行，先读在线表会在中途读到只剩表头的订单；已销毁的订单从决定提交起答不存在。集成测试在释放的半途读订单，旧读法在它上面红。

3.0.36：be-sdk-go alpha.115（全系统同一个 SDK 版本；冷热数据：多层子表、叶子表的多列主键、text[]、部署时给保留期与选路线、标准管理路由、写屏障固定 search_path）

3.0.35（底层先行第 5 项 T10d）：已结束的订单成为数据类别（七张表、三层，A155；交货行的序列号是 `text[]`）：未结束的过程、未结的认领、出库中的交货挡住封存；归档的订单按身份照样读出；订单列表的默认范围改为“未结束，或最近 N 天创建”，`history=true` 读全部（契约 `contract/v3.0.0-alpha.3` 的 OpenAPI）；保全、删除批准与销毁的重放在系统资源上（`erp.sales.system.order_hold`、`order_destruction`、`replay`）。be-sdk-go alpha.114。

3.0.34：be-sdk-go alpha.110（全系统同一个 SDK 版本；底层先行第 5 项的冷热数据：转层、读取、保全与销毁、擦除、恢复核对）

3.0.33（底层先行第 4 项 T13d 的级联）：be-sdk-go alpha.102（与整套系统同一个版本：信任材料历史，运行档案的记录按收到时刻对照它核验，写入者必须是签名证书的服务身份）；依赖换到新版本。

3.0.32（底层先行第 4 项 T13b 的级联）：be-sdk-go alpha.100（与整套系统同一个版本：重放可以答推迟，线协议 P12.20；账号状态记录可以带证据记录身份）；依赖换到新版本。

3.0.31（底层先行第 4 项 T13a 的级联）：be-sdk-go alpha.99（与整套系统同一个版本：双通道失效补写一条证据记录，线协议 P12.23；SDK 的后台作业写明恢复阶段的行为）；依赖换到新版本。

3.0.30（底层先行第 4 项 T11 的级联）：be-sdk-go alpha.97（与整套系统同一个版本：恢复阶段里证据转运照常运行，线协议 P1.23）；依赖换到新版本。

3.0.29（底层先行第 4 项 T11 的级联）：be-sdk-go alpha.96（与整套系统同一个版本：重放可以对效果在恢复组之外的类别做核对，线协议 P12.20）；依赖换到新版本。

3.0.28（底层先行第 4 项 T10 的级联）：be-sdk-go alpha.95（与整套系统同一个版本：恢复阶段里审计转运照常运行、外壳里的阶段归进程、单号的审计）；依赖换到新版本；代码地图补上恢复核对的文件。

3.0.27（底层先行第 4 项 T10 的级联）：be-sdk-go alpha.92。整套恢复的核对（`CheckService`，操作 `erp.sales.system.recover`，只许恢复工具调用）：SDK 自带的三项检查；报价、订单、交货、价格表按版本答“还在不在”；业务变更的审计 `target` 带变更之后的版本（`audit.TargetOf`）。协议 04 的检查 `erp.sales.reservations`（本组件是协调者，替两边报告，`recoverycheck.go`）：恢复点上还在进行的确认、取消、释放过程与出库中的交货列为差异（开放后由恢复工作项推进）；每个订单最近一个已结束的过程：已确认（订单仍是已确认）的分支在参与方应为已提交或已消耗，已拒绝、已取消（最近 30 天内结束的）的分支不应还占着；读本库在回滚的事务里，问参与方（`Query`，与过程推进共用 `queryBranch`）在事务之外；问不到答“无法证明”。清单的 `RECOVERY_PHASE` 加 `verifying`、`read_only`；依赖换到 T10 的新版本。

3.0.26（底层先行第 4 项 T9 的级联）：be-sdk-go alpha.87（重放、恢复阶段 `replaying`、转发器停止时放弃租约）；清单的 `RECOVERY_PHASE` 加 `replaying`。依赖：erp/credit@3.0.17 → erp/credit@3.0.18 erp/inventory@3.0.17 → erp/inventory@3.0.18 mdm/currency@3.0.17 → mdm/currency@3.0.18 mdm/org@3.0.17 → mdm/org@3.0.19 mdm/partner@3.0.21 → mdm/partner@3.0.22 mdm/product@3.0.20 → mdm/product@3.0.21。

3.0.25（底层先行第 4 项 T8e 的演练发现）：组织整套恢复后纪元变大，它的部门事件在投影对齐到新纪元之前不应用（SDK 的 `NewerEpoch`）；原来只有每小时一次的对齐，恢复后部门的变化最长一小时才到单据。现在事件处理时遇到新纪元就排一次对齐（迁移 0002 的 `org_realign` 一行去重，作业 `realign-org-units`）。集成测试开始时删掉它用到的两条流：共用 NATS 上别的运行、格与演练留下的事件（别的实现实例、更新的纪元）会让投影跳过本次的事件。

3.0.24（底层先行第 4 项 T8 的级联）：be-sdk-go alpha.81（恢复模式、补发与消费者进度）；流的补发（`be.source.v1.ResendService`，操作 `erp.sales.system.resend`，登记在本组件的 system 资源下，部署内任何组件可调用，线协议 P6.38）；清单声明 `RECOVERY_PHASE`。

实验第 7 步：报价、订单（实物与服务明细）、价格表、确认过程的全部状态与恢复、取消、发起交货、按交货的可出账台账与出账认领、订单完成时释放剩余分支、列表与查询、事件。答“能力不支持”的：修订、退货与折让、换货、款到发货、直运、寄售、合同的参考、定价规则、提交审批、服务完成的登记、关闭未交货部分、期初导入。

服务明细本版在确认后即可出账（`billable` 等于订单数量），因为“服务完成的登记”还没有。

## 部署与替换

交易中心之一，前台为主；外壳或单跑按负载。定价规则是明确的替换点（D27）；行业销售模型可 Fork。适用协议 01、02、04、05、07、08、09、10、13、14、16、安全 01–09。

## 验证故事

| 故事 | 本版 |
| --- | --- |
| 订单确认同时预占库存与信用，一方失败则全部释放 | L2（替身）、L3 |
| 确认中取消，迟到的预占被拒 | L2、L3 |
| 两张草稿并发认领同一订单行的剩余数量，只有不超额的成功 | L2 |
| 服务订单不请求库存 | L2 |
| 批准后修改数量走修订重审；部分发货后修订只改未执行部分 | 修订本版不做 |

## 未决问题

- 审批与预占的先后（协议 04 待定）：本版没有审批，提交确认即预占。
- 订单完成的判定依赖“全部出账”；没装出账的部署，订单停在 `confirmed`，靠以后的“关闭”结束。
- 交货时是否再查客户的状态（协议 07：确认后客户被暂停，订单仍有效，是否继续发货由交货环节自己决定）：本版不查；法律冻结、制裁这类绝对阻断随执行环节的检查一起做。
- 价格表外的手工单价：本版允许有权限的人改单价（`erp.sales.order.edit` 之外不另设操作），以后由定价规则约束。
- 客户的个人信息擦除（协议 07）：订单里客户的名称与税号受商法与税法的保留约束，现在没有声明可擦除的列；擦除规则随数据生命周期统一后再定。
- 部门维度的新鲜度：部门移动之后、事件到达之前，单据上复制的路径还是旧的，按部门限定的范围在这个窗口里按旧部门判断。SDK 支持按投影下推组织树维度之后，改为按投影编译，不再复制路径。
