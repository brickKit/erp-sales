# erp/sales

## 组件定位

销售：报价、销售订单与它们的执行进度。
- **报价**：给客户的价格与数量提议，不预占任何东西；发出后可以转成订单草稿（明细与单价照搬）。
- **订单**：一个法人对一个客户；明细是实物（库存商品，要指定仓库）或服务。单价取价格表（销售单位的价，否则基本单位的价乘换算系数），没有就由人录入；行金额、行税额按订单币种的小数位舍入，合计不另舍入；税率本版由人录入。
- **确认**（协议 04 的协调者）：提交确认前在事务外取证（法人、客户与产品的资格、外币订单的即期汇率），然后建一个持久的确认过程，向库存（有实物明细时）与信用（装了信用时）预占、确认；只有全部分支确认占用之后订单才是“已确认”。3 秒内有结果就直接回答，否则回答“处理中”与过程身份，由恢复工作项按同一过程继续；一方拒绝或分支到期，其余分支全部释放，订单回到草稿并附原因。确认中可以取消，取消先到的分支留下墓碑。
- **交货**：已确认订单的实物明细经库存出库，交货身份就是库存的消耗身份；库存的应答丢失时由工作项重发，直到出库或拒绝。
- **可出账台账**（协议 05 的额度属主）：每个订单行的可出账、已认领、已出账，数量与金额两维；交货过账时增加，服务明细在确认时等于订单数量与金额。出账组件按草稿账单认领、确认、正式化、释放；认领 7 天未确认就到期。全部交完并全部出账的订单完成，剩余分支随之释放。
- **商机的请求报价**（3.0.50 起，只经事件，不依赖 `crm/opportunity`）：订阅商机的请求报价，同一请求只生成一张报价草稿（法人、客户、币种、负责人取请求的，没有明细，请求的产品放在草稿上给销售人员参考，补上明细才能发出）；法人、币种或客户不可用时不生成，发布拒绝的事实。报价与它转成的订单带商机参考，商机据此记下关联、按它的设置自动赢单。
- **数据范围**：报价、订单与交货带法人、部门（组织树）与负责人三个维度；部门调整后，单据上的部门路径随 `mdm/org` 的事件改写。

账单与发票编号不在这里（`erp/billing`），库存余额（`erp/inventory`）、信用额度（`erp/credit`）也不在这里。本版答“能力不支持”（`CAPABILITY_UNSUPPORTED`，D75）的：修订、关闭未交货部分、退货与折让、出账的冲回、款到发货、直运、寄售、定价规则、审批、服务完成的登记、期初导入。

## 部署前准备

- 数据库：PostgreSQL 16 及以上；schema `erp_sales`，属主角色 `erp_sales_owner`（只供迁移）与运行角色 `erp_sales_rw`（只有读写数据的权限），名字来自装配项目的注册表。三者由部署方在第一次启动前建好；表由本组件的迁移建，迁移里不建角色、不授权。
- 消息系统（`BUS_URL`）与实现实例（`IMPLEMENTATION_INSTANCE`）：报价、订单、交货、价格表与价格以完整状态事件，确认结束、交货已过账、可出账台账变化以增量事实发布到流 `erp.sales`（4 个分区）；订阅 `mdm/org` 的流 `mdm.org` 里的部门事件（全部动作，`mdm.org_unit.>`）与 `crm/opportunity` 的流 `crm.opportunity` 里的请求报价（没装商机时这个流是空的）。
- 部署的服务证书（`SERVICE_TLS_*`）；身份与授权（`IAM_*`、`AUTHZ_GRPC_URL`、`DEPLOYMENT_ID`）；审计（`AUDIT_GRPC_URL`）。
- 依赖 `mdm/org`、`mdm/partner`、`mdm/product`、`mdm/currency`，经它们的系统面读取；库存与信用是可选的参与方。
- 授权：系统面的出账来源只给 `erp/billing` 的服务身份；订单的系统读取给任一组件。
- 交易中心之一：负载高时单跑（协议 17）。

## 依赖说明

- 必需：数据库、消息系统、身份成员、授权成员、`mdm/org`、`mdm/partner`、`mdm/product`、`mdm/currency`。
- 可选：`erp/inventory`（缺时含实物明细的订单不能确认，答“依赖不可用”；只有服务明细的订单照常）、`erp/credit`（缺时没有信用控制，过程没有信用分支）、审计组件。
- 主数据不可达时：新建、修改、确认答“依赖不可用”，已有数据照常读取。库存或信用在确认中不可达时：订单停在“确认中”，恢复工作项按退避重试；用完重试次数时过程以“依赖不可用”结束并释放其余分支。

## 配置指南

全部键见 `component.yaml` 的 `configSchema`（由 SDK 生成，测试核对）。

| 键 | 说明 |
| --- | --- |
| `DEPLOYMENT_ID`、`IAM_ISSUER`、`IAM_JWKS_URL`、`AUTHZ_GRPC_URL`、`SERVICE_TLS_CERT_FILE`、`SERVICE_TLS_KEY_FILE`、`SERVICE_TLS_CA_FILE`（必填） | 受保护的用户面与 mTLS 的系统面要用；部署身份、身份与授权的地址在外壳里是进程级的键 |
| `PG_HOST`、`PG_DATABASE`、`PG_USER`、`PG_PASSWORD`、`PG_SCHEMA`（必填） | 自己的数据库，密码是密钥；迁移另用 `PG_OWNER_USER`、`PG_OWNER_PASSWORD` |
| `BUS_URL`（必填，密钥）、`IMPLEMENTATION_INSTANCE`（必填） | 消息系统；实现实例是安装时生成的随机身份 |
| `AUDIT_GRPC_URL` | 审计组件的系统面；缺时审计记录留在本地，按协议 03 处理 |
| `TIERING_RETENTION_ERP_SALES_ORDERS` | 已结束订单的保留期 `最短天数/最长天数/依据`（3.0.35 起），按国家的商法与税法与客户要求在上线时给；不给则从不删除，启动时记警告 `tiering_retention_pending` |
| `TIERING_RESIDENCE_ERP_SALES_ORDERS`、`TIERING_LIST_DAYS`、`TIERING_GRACE`、`TIERING_WINDOW` | 结束后在线保留的天数（默认 365）、订单列表的默认范围（默认 90 天）、发布后的宽限期、封存的低峰时段，见 be-sdk-go 的转层 |
| `TIERING_STORE_URL` 与 `TIERING_*_ACCESS_KEY`、`TIERING_*_SECRET_KEY` | 冷层的对象存储（历史的异步导出用；订单本身走同库的温层） |
| `RUN_PROFILE_*` | 运行档案：释放在线行要一次在发布之后完成的备份记录；不配则在线行一直不释放 |

依赖的地址由 BrickKit 按依赖注入（`MDM_ORG_GRPC_ENDPOINT`、`MDM_PARTNER_GRPC_ENDPOINT`、`MDM_PRODUCT_GRPC_ENDPOINT`、`MDM_CURRENCY_GRPC_ENDPOINT`，装了时还有 `ERP_INVENTORY_GRPC_ENDPOINT`、`ERP_CREDIT_GRPC_ENDPOINT`），不是配置键。单号格式（`<前缀>-<法人代码>-<年>-<六位序号>`，前缀 `QT`、`SO`、`DN`）、同步预算（3 秒）、认领期限（7 天）本版固定。

## 契约索引

- **系统面**（本仓库 `contract/proto`，模块 `github.com/brickKit/erp-sales/contract/v3`，标签 `contract/v3.…`）：
  - `be.erp.sales.v3.BillingSourceService/Ledger`、`Claim`、`Renew`、`Confirm`、`Formalize`、`Release`（`Reverse` 本版不支持）：只对 `erp/billing` 开放，操作 `erp.sales.system.billing`；按认领身份幂等。
  - `be.erp.sales.v3.OrderService/Read`：任一组件，操作 `erp.sales.system.read`；数据是订单事件的载荷。
- **用户面**：`contract/openapi.yaml`。操作 `erp.sales.quotation.read`、`erp.sales.quotation.edit`、`erp.sales.order.read`、`erp.sales.order.edit`、`erp.sales.order.confirm`、`erp.sales.order.cancel`、`erp.sales.delivery.read`、`erp.sales.delivery.create`、`erp.sales.price_list.read`、`erp.sales.price_list.manage`（`erp.sales.return.read`、`erp.sales.return.edit` 本版不支持）；报价、订单与交货带法人、部门、负责人三个维度（“销售只看自己的订单”“经理看本部门及下级”），价格表带法人维度。新建、确认、取消、交货要带 `Idempotency-Key`。
- **用户面的转层管理**（SDK 的标准路由，3.0.35 起）：`/api/sales/tiering/erp.sales.orders/holds`、`…/approvals`（操作 `erp.sales.system.order_hold`、`erp.sales.system.order_destruction`），`…/exports/{id}`（请求者本人）；订单列表的 `history=true` 与归档订单的读取见 OpenAPI（`contract/v3.0.0-alpha.3`）。系统面的证据重放 `be.evidence.v1.ReplayService/Replay`（`erp.sales.system.replay`，恢复工具）。
- **事件**（流 `erp.sales`，schema 在 `contract/events/`）：`erp.quotation.changed.v1`、`erp.sales_order.changed.v1`、`erp.delivery.changed.v1`、`erp.price_list.changed.v1`、`erp.price_list_item.changed.v1`（完整状态），`erp.sales_order.confirmation_ended.v1`、`erp.delivery.posted.v1`、`erp.billable.changed.v1`、`erp.quote_request.rejected.v1`（增量事实，按 `fact_id` 去重）；报价与订单的事件带 `opportunity_ref`，报价另带 `quote_request_id` 与 `requested_products`。订阅 `crm.opportunity.quote_requested.v1`（按事实身份只生成一张草稿）与 `mdm/org` 部门的全部事件（`mdm.org_unit.>`；认识的是 `mdm.org_unit.changed.v1`，别的标为不完整再取回）。

## 外壳声明

不是外壳。可以被 Go 的核心外壳（`be/go-core`）承载。
