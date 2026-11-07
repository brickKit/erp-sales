# documents：报价与订单共用的部分（共用）

报价与订单长得一样的部分：表头的取证与授权、行（取价、金额、整份替换）、单号、读取、列表。不是一个业务，没有自己的路由。

## 负责与不负责

- 负责：表头（法人、客户、币种、负责人、部门）的取证与授权（`Prepare`、`PatchHead`）；部门的选取（`departmentOf`）；单号（`Number`：`<前缀>-<法人代码>-<年>-<六位序号>`，允许缺号）；行：输入的检查、取证、取价与金额（`BuildLines`）、整份替换与合计（`WriteLines`）；报价的事件（`EmitQuotation`）；列表的共用部分。
- 不负责：价格表与 `PriceOf`（`pricing`）；报价、订单各自的命令（`quotations`、`orders`）；记录的类型与读取的 SQL（`core/records.go`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `documents.go` | 表头、单号、`Quotation`、`Order` 的读取、`PatchIn`、幂等结果的写法（`IDResult`、`ResultID`）；列表的共用部分：列（`DocColumns`）、按参数的相等筛选（`Filters`）、编译（`Build`）、关键字（单号与客户名）、默认排序（最新在前） |
| `lines.go` | `LineIn`（契约的 `LineIn`）、`checkLines`、`evidenceOf`（事务外取产品与换算）、`BuildLines`、`WriteLines` |

列表（报价、订单、交货都用 `Filters`、`Build`，各自的列定义在各自的模块）：用户面的列表是协议 14 的游标分页、最新在前，筛选与关键字在这里声明；每个字段都是普通字段，一行要么整行列出、要么不列；法人、部门、负责人三个维度编译进 `WHERE`（部门比较复制来的 `department_path`，见 `orgunits`）。

## 依赖与被依赖

- 依赖：`core`、`masterdata`、`orgunits`（`Department`）、`pricing`（`PriceOf`）。
- 被依赖：`quotations`、`orders`、`confirmations`、`deliveries`、`billingsource`。

## 规则在哪

`docs/rules/pricing.md`（每个数怎样来）；装配项目协议 14（单号、金额、列表）；安全 07（三个维度）。

## 测试

`itest/quotations_integration_test.go`（取价与金额）、`itest/lists_integration_test.go`（关键字找单号）、`itest/orgunits_integration_test.go`（部门维度）。
