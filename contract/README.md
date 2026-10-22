# erp-sales 的契约

`erp/sales` 发布的契约包：系统面的 protobuf、事件载荷的 JSON Schema、用户面的 OpenAPI。放在组件仓库的 `contract/` 子目录里单独发布（标签 `contract/v3.…`）。

依据：装配项目 [be-assembly-standard](https://github.com/brickKit/be-assembly-standard) 的协议 04（确认过程，本组件是协调者）、协议 05（认领模式与可出账台账，本组件是出账额度的属主）、协议 14（单号、金额、列表）；组件的 `docs/design.md` 与 `docs/flows/`、`docs/rules/`。

## 内容

| 位置 | 是什么 |
| --- | --- |
| `proto/be/erp/sales/v3/sales.proto` | `BillingSourceService`（可出账台账与出账认领：`Ledger`、`Claim`、`Renew`、`Confirm`、`Formalize`、`Release`、`Reverse`）；`OrderService.Read` |
| `gen/go/be/erp/sales/v3/` | Go 代码，包 `salesv3`；模块 `github.com/brickKit/erp-sales/contract/v3` |
| `python/` | Python 包 `contract-erp-sales`，模块 `be.erp.sales.v3` |
| `events/` | 流 `erp.sales`：报价、订单、交货、价格表与价格（完整状态）；确认结束、交货已过账、可出账台账变化（增量事实） |
| `openapi.yaml` | 用户面；每个路由的操作写在 `x-operation` |

## 要点

- **数量与金额**是十进制字符串。明细的数量用销售单位，`base_quantity` 是换算后的基本单位（库存只用它）；可出账台账的数量维也用销售单位。
- **确认**（`POST /orders/{id}/confirm`）在同步预算（3 秒）内答 `confirmed` 或 `rejected`，否则答 `processing` 与过程身份，之后用 `GET /orders/{id}/confirmations/{confirmation_id}` 查；同一 `Idempotency-Key` 返回同一过程。
- **出账认领**按 `claim_id` 幂等，一次认领一张订单的若干行，全部满足才认领（BL01）；确认在编号之前，已确认的不到期；正式化按实际数量，剩余同时释放。
- **能力不支持**（D75）：`Reverse`、修订、关闭未交货部分、退货与折让，答 `CAPABILITY_UNSUPPORTED`。
