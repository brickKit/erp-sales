# quoterequests：商机的请求报价

`crm/opportunity` 的请求报价（`crm.opportunity.quote_requested.v1`）变成报价草稿或拒绝的事实（设计 QR01–QR04）。CRM 与 ERP 只经事件：本组件订阅它、不声明它为依赖。

## 负责与不负责

- 负责：表 `quote_request_in`（按请求身份去重，重复投递只生成一张草稿）；订阅的处理 `OnRequested`（在 SDK 的事务里只记下、排工作，不调用别的组件）；工作 `draft-quotation`（`DraftJob`：事务外取证法人、币种、客户资格，再开事务生成草稿或发布 `erp.quote_request.rejected.v1`；主数据不可达时重试）。
- 不负责：报价之后的修改、发出、接受（`quotations`；没有明细的草稿不能发出是那里的 QR05）；订单的商机参考（`core.OrderCols` 从报价读）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `quoterequests.go` | `JobDraft`、`OnRequested`、`DraftJob`、`evidence`（取证，三种原因算拒绝，其余是错误、重试）、`draft`、`refuse`、`lockReceived`（事务里锁请求行，别的运行已经决定了就什么也不做） |

## 依赖与被依赖

- 依赖：`core`、`documents`（单号、报价的事件）、`masterdata`（取证）。
- 被依赖：入口 `sales/module.go`（订阅与工作）。

## 表

`quote_request_in`；生成的草稿写进 `quotation`（`quotations` 的表：带 `opportunity_ref`、`quote_request_id`、`requested_products`，没有明细）。

## 规则在哪

设计 QR01–QR05；装配项目的决定 A71、边界卡 `crm.md` 与 `erp-trade.md`。

## 测试

`itest/quoterequests_integration_test.go`（步 `quoteRequests`）。
