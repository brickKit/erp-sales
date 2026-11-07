# quotations：报价

报价的一生：草稿、修改、发出、取消，接受后转成一张同价的订单草稿。

## 负责与不负责

- 负责：表 `quotation`；路由 `/api/sales/quotations` 的列表、新建、读取、修改，与 `send`、`cancel`、`accept` 三个动作；接受时直接建订单草稿（`sales_order`、`order_line`，沿用报价的价格）。
- 不负责：表头的取证与授权、行的取价与金额、单号、列表的共用部分（`documents`）；价格从哪来（`pricing`）；订单之后的一切（`orders`、`confirmations`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `quotations.go` | `CreateQuotation`、`GetQuotation`、`UpdateQuotation`；`QuotationAction`（发出、取消：状态的前提与转换）；`AcceptQuotation`（转订单草稿）；`ListQuotations` 与列表的定义 `quotationList` |

## 依赖与被依赖

- 依赖：`core`、`documents`（`Prepare`、`PatchHead`、`BuildLines`、`WriteLines`、`Number`、列表）、`masterdata`。
- 被依赖：只有入口 `sales/module.go` 登记路由。

## 规则在哪

- 报价的状态与命令：`docs/design.md` 的“命令与查询”。
- 取价与金额：`docs/rules/pricing.md`。

## 测试

`itest/quotations_integration_test.go`（`pricingAndQuotations`：发出、接受成同价的订单草稿；缺价、客户不合格被拒）。
