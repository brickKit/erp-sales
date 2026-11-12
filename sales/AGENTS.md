# sales/：入口与组装

组件对外的一个包：`main.go` 与外壳只引用它（`sales.Definition()`、`sales.New`）。这里只**组装**：把 `internal/` 下业务模块的函数登记成 SDK 要的路由、gRPC 方法、订阅、后台工作与恢复核对；业务一行也不写。

## 负责与不负责

- 负责：组件的身份与版本、迁移、把哪个路由交给哪个模块、哪个后台工作交给哪个模块。
- 不负责：业务规则（去 [internal/AGENTS.md](../internal/AGENTS.md) 找模块）；资源、操作、维度、事件类型的名字（`internal/core/names.go`）；集成测试（`itest/`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `definition.go` | `Version`、端口、迁移；`Definition()` 把身份、依赖、配置、资源与操作（`core.Definitions()`）、数据类别（`orders.OrdersClass()`）收成组件定义 |
| `module.go` | `Module` 与 `New`；`Routes`（用户面的每条路由 → 模块的处理函数与操作）、`Services`（系统面：出账来源只许 `erp/billing`，订单读取任何组件）、`Subscriptions`（部门事件 → `orgunits`；商机的请求报价 → `quoterequests`）、`Jobs`（每个后台工作 → 模块）；`handler`、`job`、`dead`、`received` 把“先收 `*core.Base`”的模块函数适配成 SDK 的签名 |
| `services.go` | gRPC 方法（`BillingSourceService`、`OrderService.Read`）转给 `billingsource`；`Tiering` 转给 `orders` |
| `checks.go` | 整套恢复的核对（线协议第 14 章）：四种对象按版本答“还在不在”；协议 04 的检查转给 `recoverycheck` |
| `contract_test.go` | 单元测试：清单的 `configSchema` 与 `events`、OpenAPI 与路由一一对应、事件载荷合乎 schema、合并安全 |
| `structure_test.go` | 单元测试，按类型检查 `internal/`：`internal/orders/lifecycle.go` 以外不写 `sales_order.state`，读订单状态的（Go 或 SQL）都登记在 `orderStateReaders`（文件与函数、用途）；`ledger` 以外不写台账列 |
| `migrations/` | 表，整个组件一套 |

## 依赖与被依赖

- 依赖：`internal/` 下除 `documents`、`ledger`、`masterdata` 以外的全部模块（那三个只被模块用）。
- 被依赖：`main.go`、`cmd/seed`（只用 `Definition`）、外壳、`itest/`。
- 模块不能反过来引用这里（会成环）：模块需要的名字放 `internal/core`。

## 加东西

- 新路由：处理函数写在它所属的模块里，在 `module.go` 的 `Routes` 登记，操作的名字加在 `internal/core/names.go`，OpenAPI 改 `contract/openapi.yaml`（`TestTheRoutesAreTheOpenAPIDocument` 核对两边）。
- 新后台工作：函数写在模块里，在 `Jobs` 登记；工作名作为模块的导出常量（例：`confirmations.JobAdvance`）。

## 测试

`go test ./sales/`；`make test`（`go vet ./... && go test ./...`）另外跑 `internal/orders` 的生命周期测试。
