[English](README.md) · [中文](README.zh.md)

# erp/sales

销售报价与订单生命周期、定价，以及确认订单时预留库存的补偿编排。

## 在项目里使用

```bash
brickkit add erp/sales@2.0.0
brickkit up
```

先看 BRICKKIT.zh.md 的"部署前准备"（PostgreSQL schema 与登录角色、NATS、`erp/inventory` 里的一个仓库 ID、授权与 JWKS 的 URL）。它会连带 `mdm/customer`、`mdm/product`、`erp/inventory` 一起起来；`infra/workflow` 可选。

## 文档

| 想知道 | 读 |
|---|---|
| 它做什么、不做什么，怎么配置，要准备什么 | [BRICKKIT.zh.md](BRICKKIT.zh.md) |
| gRPC、REST 与事件契约 | [contracts/](contracts/) |
| 为什么这样设计：边界、订单状态、确认链、数据范围、未决问题 | [docs/design.zh.md](docs/design.zh.md) |
| 依赖什么、有哪些配置键 | [component.yaml](component.yaml) |
| 怎么开发：代码地图、测试、易错点 | [AGENTS.zh.md](AGENTS.zh.md) |

## 开发

Go 1.25、Gin、`database/sql` + pgx、经 be-sdk-go 的 golang-migrate。测试需要真实的 PostgreSQL（`TEST_PG_DSN`）；确认链对真实依赖的测试在项目根用 `make test-cross ID=erp/sales` 跑。命令与成功的样子见 AGENTS.zh.md 的"构建与测试"。在 BrickEnterprise 项目里，项目根的 `make verify ID=erp/sales ROUTE=/erp/sales/orders FOCUS=1` 以容器和本机进程两种方式真机跑一遍，然后收尾。
