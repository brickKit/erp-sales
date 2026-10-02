[English](README.md) · [中文](README.zh.md)

# erp/sales

Sales quotations and the order lifecycle, pricing, and the compensating orchestration that reserves stock when an order is confirmed.

## Use it in a project

```bash
brickkit add erp/sales@2.0.0
brickkit up
```

Prepare first: see "Before you deploy" in BRICKKIT.md (a PostgreSQL schema and login role, NATS, a warehouse ID in `erp/inventory`, and the authorization and JWKS URLs). It brings `mdm/customer`, `mdm/product` and `erp/inventory` along; `infra/workflow` is optional.

## Documentation

| To find out | Read |
|---|---|
| What it does and does not do, how to configure it, what to prepare | [BRICKKIT.md](BRICKKIT.md) |
| Its gRPC, REST and event contracts | [contracts/](contracts/) |
| Why it is designed this way: boundary, order states, the confirmation chain, data scopes, open questions | [docs/design.md](docs/design.md) |
| What it depends on and its configuration keys | [component.yaml](component.yaml) |
| How to develop it: code map, tests, pitfalls | [AGENTS.md](AGENTS.md) |

## Development

Go 1.25, Gin, `database/sql` with pgx, golang-migrate through be-sdk-go. Tests need a real PostgreSQL (`TEST_PG_DSN`); the tests of the confirmation chain against real dependencies run with `make test-cross ID=erp/sales` at the project root. The commands and what success looks like are in AGENTS.md, section "Build and test". Inside the BrickEnterprise project, `make verify ID=erp/sales ROUTE=/erp/sales/orders FOCUS=1` at the project root runs it for real, in a container and as a local process, and tears down afterwards.
