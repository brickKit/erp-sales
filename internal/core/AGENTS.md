# core：全组件共用的名字与写法（共用）

每个模块都引用、自己什么业务也不做的东西：名字、资源定义、错误原因与校验、审计与事件的写法、记录的类型与读取。改这里会影响所有模块。

## 负责与不负责

- 负责：组件 ID、依赖的 ID、流与事件类型、资源与操作、三个维度、参数（`names.go`）；资源定义 `Definitions()`（全组件一张表：列表能筛选的字段、维度的映射按资源定）；本组件的错误原因与校验、审计（`Change`）、发件表（`Emit`）、金额乘法（`Times`）（`rules.go`）；记录类型、读取的 SQL、`EmitOrder`（`records.go`）；`Base`（`base.go`）。
- 不负责：任何业务规则；数据类别（`orders/archive.go`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `base.go` | `Base`：组件的运行时，每个模块函数的第一个参数 |
| `names.go` | 常量；`fields`、`scoped`、`DocumentFields`（报价与订单的字段）、`Definitions()`（每种资源的维度与字段：交货、价格表各有自己的字段清单）。列表能排序、筛选的字段要在这里它的资源下声明，不声明 SDK 就答 `FIELD_NOT_QUERYABLE` |
| `rules.go` | `Err…`（本组件的原因）、表单的格式、`DocFacts`、`Check`（授权）、`Invalid`、`NotFound`、`Decimal`、`Change`、`Emit`、`Times` |
| `records.go` | `Quotation`、`Order`、`Delivery`、`PriceList` 等记录与 `Scan…`、`Get…`（`lock` 取订单行锁：订单与它的台账的每次变化都取这把锁）、`EmitOrder` |

## 依赖与被依赖

- 依赖：只有 SDK 与契约。
- 被依赖：全部模块与入口。

## 规则在哪

资源、操作、维度：安全 06、安全 07、安全 09a；事件与发件表：装配项目协议 02；审计：协议 03；金额：协议 14（不用浮点，乘法经 `Times`，比较与加减在 SQL 里）。

## 测试

入口的单元测试 `sales/contract_test.go`（清单、事件载荷）；其余由全部流程测试间接覆盖。
