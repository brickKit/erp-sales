# pricing：价格表与取价

价格表的维护，以及一行在某天、某个销售单位上取哪个价。

## 负责与不负责

- 负责：表 `price_list`、`price_list_item`；路由 `/api/sales/price-lists` 的列表、新建、读取、修改，与价格项的列表、设置；事件 `erp.price_list.changed.v1`、`erp.price_list_item.changed.v1`；`PriceOf`。
- 不负责：取价的顺序与拒绝（`documents/lines.go` 的 `BuildLines`：`docs/rules/pricing.md` 单价表的第 1、2 步与第 5 步的 `NO_PRICE` 在那里，第 3、4 步调 `PriceOf`）；行的金额、税额与舍入（也在 `BuildLines`）；单位换算（`masterdata.Convert`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `pricing.go` | 价格表的处理函数；`SetPrice`（同一产品与单位之前的价在前一天结束）；`ListPriceLists`、`ListPrices`；`PriceOf`（销售单位的价，否则基本单位的价乘系数） |

## 依赖与被依赖

- 依赖：`core`、`masterdata`。
- 被依赖：`documents`（`PriceOf`）；入口登记路由。

## 规则在哪

`docs/rules/pricing.md` 的“单价”。

## 测试

`itest/quotations_integration_test.go`（`pricingAndQuotations`：每 EA 的价，一盒取它乘系数）。
