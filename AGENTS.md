[English](AGENTS.md) · [中文](AGENTS.zh.md)

# erp/sales

The AI guide to developing this component. How to use it, its boundaries and contracts: BRICKKIT.md. Why it is shaped this way: `docs/design.md`. Dependencies, configuration and deployment: component.yaml.

## Code map

| Path | Owns |
|---|---|
| `backend/module/module.go` | The only entry, `New(ctx, rt)`: reads the configuration (`readConfig`; the schema is `PG_SCHEMA`, the role `PG_USER`), builds repo → orchestrator → service → HTTP + gRPC, starts the outbox pump, the two partition loops and the consumers. Same function standalone and in a shell |
| `backend/cmd/server/main.go` | One line, `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | One line, `migrate.Main(migrations.FS)`: the migration container's entry (`./migrate up`) |
| `backend/internal/tcc/` | The cross-component orchestration: `confirm.go` (reserve, status after a timeout, compensation, suspension and exception task), `create.go`, `cancel.go`, `ship.go`, `opportunity_won.go` (won opportunity → confirmed order, system identity), `credit_rejected.go` (the exception task after finance rejects an order's credit), `loglevel.go` (`FailureLevel`) |
| `backend/internal/client/client.go` | Dialing the four dependencies: `Customer`, `Product`, `Inventory`, `Workflow` forward the caller's token; the `*System` variants are for event handlers only |
| `backend/internal/repo/write.go` | Local writes: `CreateOrder` and the `Finalize*` steps; each runs idempotency claim → state check under row lock → write → outbox in one transaction |
| `backend/internal/repo/order.go` | Order types and statuses, `GetOrder`, `ListOrders` (scope, filters, keyset paging), `BatchGetOrder`, `GetOrderStatus` |
| `backend/internal/repo/pricing.go` | `validateQty`, the price rule lookup `findPriceTx`, `CalculatePriceDryRun` |
| `backend/internal/repo/stats.go` | `StatsSummary` for `GET /stats/summary` |
| `backend/internal/repo/snapshot.go` | The customer credit limit copy (`UpsertCustomerSnapshotCreditLimitTx`, `GetCustomerSnapshotTx`); `RefreshCustomerSnapshot` in `write.go` refreshes it at confirmation |
| `backend/internal/repo/suspend.go` | `SuspendOrderTx`, the compensation failure counter, `ResumeFromExceptionTx` |
| `backend/internal/repo/repo.go` | Sentinel errors, `Order.InScope`, `parseID` |
| `backend/internal/decimal/decimal.go` | Exact decimal arithmetic on strings (big.Rat from the standard library), rounding half away from zero to cents |
| `backend/internal/consumer/consumer.go` | The five consumed subjects and their handlers |
| `backend/internal/service/` | Input checks, the data scope on single-order commands, logging by `tcc.FailureLevel`, the error → gRPC status mapping (`status.go`) |
| `backend/internal/http/http.go` | REST routes, each registered with its permission key; `parseListInput`, `statsSummaryHandler` |
| `backend/internal/grpc/grpc.go` | `erp.sales.v1.SalesService` |
| `backend/internal/partition/` | Background loops: weekly partitions of `event_outbox` / `event_inbox` four weeks ahead, monthly partitions of `sales_orders` / `sales_order_items` three months ahead |
| `migrations/` | SQL migrations, embedded by `migrations/embed.go`; no schema or role names (the search path and the migrating role supply them); `005` creates the partition window for the day it runs |
| `contracts/` | proto, OpenAPI, event schema |
| `gen/erp/sales/` | Generated Go code: a nested Go module, tagged on its own as gen/erp/sales/v1.x.y; never edited by hand |
| `scripts/` | `seed.sh` / `seed-clean.sh` for local demo data |

| Feature | Start here | Then |
|---|---|---|
| A step of order confirmation, a timeout, the compensation | `backend/internal/tcc/confirm.go` | `backend/internal/tcc/compensate_test.go`, `backend/internal/tcc/confirm_test.go` (real dependencies) |
| Won opportunity → order | `backend/internal/tcc/opportunity_won.go` | `backend/internal/consumer/consumer.go` (`opportunityWonHandler`), `backend/internal/tcc/opportunity_source_test.go` |
| Price rules, quantity checks | `backend/internal/repo/pricing.go` | `backend/internal/repo/qty_test.go`, `backend/internal/repo/money_test.go` |
| A new list filter or query parameter | `backend/internal/http/http.go` (`parseListInput`) | `backend/internal/repo/order.go` (`ListOrders`), `contracts/` (append only) |
| The dashboard numbers | `backend/internal/repo/stats.go` | `backend/internal/repo/stats_test.go`, `backend/internal/http/http_test.go` |
| A new field on the order | `migrations/` (a new migration) | `contracts/` (append only), `buf generate`, `backend/internal/repo/order.go` |
| A consumed event | `backend/internal/consumer/consumer.go` | the producer's event contract (copy fields, never import) |
| An error answering with the wrong status, or logged at the wrong level | `backend/internal/service/status.go` | `backend/internal/tcc/loglevel.go` |

## Build and test

```bash
# tests run against the test database brickkit_test_db, never brickkit_db (make test-db-init at the project root)
set -a; . ../../../.env; set +a
export TEST_PG_DSN="postgres://postgres:${POSTGRES_PASSWORD}@localhost:5432/brickkit_test_db?sslmode=disable"
make test                    # every package ends in "ok"; refuses to run without TEST_PG_DSN
go test ./... -count=1 -v | grep -- '--- SKIP'   # only the tests that need real dependencies (14, see below)
make check-version dag-check contract-check import-scan module-check   # each prints one ✓ line
make docs-check              # "0 with errors, 0 warnings"
# migrations, as the login role (this project's: erp_sales_rw):
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=erp_sales_rw \
  PG_PASSWORD="$ERP_SALES_DB_PASSWORD" PG_SCHEMA=erp_sales make migrate-idempotent   # ✓ 迁移幂等
```

The skipped tests (in `backend/internal/tcc/confirm_test.go`, `backend/internal/tcc/opportunity_won_test.go`, `backend/internal/tcc/snapshot_refresh_test.go`, `backend/internal/consumer/opportunity_won_dlq_test.go`) drive real `mdm/customer`, `mdm/product`, `erp/inventory` and `infra/workflow` containers: real insufficient stock, real timeouts. They run from the project root with `make test-cross ID=erp/sales`, which injects the dependency addresses. After `buf generate` (contracts changed), the contract package needs a new tag: see Pitfalls. On a real machine, from the project root: `make verify ID=erp/sales ROUTE=/erp/sales/orders FOCUS=1` builds the image, starts this component with its dependencies, checks migration, health and the permission check, runs a focus run, and tears down.

## Design decisions

- **Confirmation is a TCC chain**, local checks first: customer, products, credit limit (the local copy, refreshed from the customer `BatchGet` just made, before any reservation), then `Reserve`, then the local confirmation with the event in one transaction; only a failure after `Reserve` is compensated.
- **A `Reserve` timeout is never answered with `CancelReservation`**: the reservation state is asked first (`GetReservationStatus`); unknown even then → a row in `sales_order_reconciliation_queue`, `503` to the caller.
- **Compensation retries in place, three times**, under the same idempotency key and without the caller's cancellation; the third failure suspends the order and opens an exception task.
- **Not enough stock fails the whole order**; there is no partial reservation.
- **`sales.order.created.v1` is published on confirmation**, not on creation: a draft must not produce a receivable.
- **Order lines are snapshots** (SKU, name, unit, price); nothing joins the master data later.
- **Two data-scope views, chosen by `view`**: `dept` (department prefix) or `mine` (owner), never combined; single-order reads and commands accept either. A caller with no department matches no `dept` row and sees only their own orders.
- **The database identity is configuration**: the schema is `PG_SCHEMA`, the role every transaction switches to is `PG_USER`, under any names; the migration runs as that role, so it owns the tables and no migration names it. `backend/module/identity_test.go` installs the component under unrelated names, standalone and as a shell member.
- **Money and quantities are decimal strings** end to end, computed with `math/big.Rat` and rounded once, half away from zero, to cents.
- **A won opportunity becomes an order through the event only** (system identity, ownership taken from the event); failures become an exception task for the opportunity's owner, never an event back to CRM.

## Pitfalls

| Never | Symptom | Why |
|---|---|---|
| Call `CancelReservation` right after a `Reserve` timeout | Stock that was reserved is released while the order goes on, or a failed reservation is treated as success; the numbers drift with no error | A missing response does not mean the request was not processed; `resolveAfterTimeout` asks first |
| Check the credit limit after `Reserve` | An order clearly over its limit first takes stock, then is rejected and compensated | The local check is free; it runs before any reservation |
| Judge the credit limit from the event-fed copy alone | Installed into a project that already has customers, every confirmation answers `400 … exceeds limit 0`; a limit lowered while this component was down is never seen | Core NATS never replays; `checkCredit` refreshes the copy from step ①'s `BatchGet` (by `version`) before reading it |
| Use the `*System` clients on a request path, or the plain clients in an event handler | Request path: the response silently contains rows the user may not see. Event handler: the call goes out with no token | `UserClient` forwards the caller; event handlers have no caller (`make gates` runs `system-client-scan`) |
| Reuse `CreateOrder` / `ConfirmOrder` for the won-opportunity path | `besdk.ScopeOf` panics in the event handler: there are no claims | `createOrderForOpportunity` / `confirmOrderForOpportunity` take ownership from the event |
| Return an error from `opportunityWonHandler` expecting a redelivery | The won event is never seen again | `besdk.Consume` uses core NATS: a handler error is logged, not redelivered |
| Check the order's state, or call a dependency, before looking up the command's idempotency key | Every replay of a successful confirm or ship answers `400` ("not a draft"); a second `make seed` dies; a won opportunity seen twice opens a false exception task | `Repo.ReplayCommand` runs first in every orchestrator and returns the first result; a key used by another command or order is `400` |
| Refuse to cancel every `SUSPENDED` order, or suspend a `SHIPPED` one | An order suspended by finance's credit rejection keeps its reservation in `erp/inventory` forever, since no command can leave that state; a shipped order disappears into a state nothing leads out of | `CancelOrder` refuses only `SuspendedByCompensation()`; `SuspendOrderTx` leaves `SHIPPED` alone and the handler opens a task |
| Resume a suspended order on `status == SUSPENDED` alone | An order suspended by finance's credit rejection is put back to `DRAFT` by an unrelated task completion | `ResumeFromExceptionTx` matches `suspended_reason` exactly |
| Handle every `infra.workflow.task.completed.v1` | Another component's task ID is taken for an order ID | The subject is shared by all components; filter on `source_component` / `source_aggregate` |
| Parse an amount or quantity with `strconv.ParseFloat`, or store a value without checking it | `"NaN"` lands in a `NUMERIC` column and outranks every number, so `CHECK (qty > 0)` and every comparison let it through; `0.1 + 0.2` rounds to the wrong cent | `validateQty` and `creditLimitPattern` accept plain decimals only; arithmetic goes through `backend/internal/decimal` |
| Log a caller's error at ERROR | Every rejected draft or out-of-stock order pages someone | `tcc.FailureLevel`: 4xx → INFO, transient → WARN, the rest → ERROR |
| Write `besdk.ScopeOf(ctx).Prefix` into a row without checking `HasDept` | The SDK's no-department value lands in `dept_path`; every other caller without a department now sees that order in their `dept` view | Snapshot `dept_path` / `dept_id` only when `HasDept`, empty otherwise; `CreateOrder` in the repository rejects a `dept_path` that does not start with `/` |
| Treat an empty `ScopePrefix` / `ScopeOwner` as "no restriction" | A `LIKE` on an empty prefix matches the whole table, and every caller sees every order | The repository returns `InvalidArgument`; `Order.InScope` treats that side as not matching |
| Answer `403` when someone reads a single order outside their scope | The answer tells a stranger that the order exists | A read outside the scope is `404`, worded exactly like an unknown ID; only commands answer `403` (`GetOrder` vs `checkOrderInScope`) |
| Derive the role from the schema name, or write a role or schema name into a migration (`OWNER TO`, `erp_sales.`) | Green in this project, where the names happen to be `erp_sales` / `erp_sales_rw`; any other deployment gets `must be able to SET ROLE` from the migration or `role … does not exist` on every request | The role is `PG_USER` and the schema `PG_SCHEMA` (`readConfig`); the migration runs as the role and resolves bare names through the search path |
| Rely on the dated initial partitions of a migration alone | On a database installed after those dates, today has no partition and every write fails until the maintenance loop's first run | Migration `005` creates the loops' window for the day it runs; a new partitioned table needs the same |
| Partition `sales_order_items` by its own time | Items of one order land in different partitions from their header | They are partitioned by the header's `created_at` (`order_created_at`) |
| Change a comment or option in `contracts/erp/sales/v1/sales.proto` without tagging the contract package | Builds pass locally (the `replace` hides it); a shell fetching `erp-sales/v2` compiles against the old `gen/` | Any change under `gen/erp/sales/` needs a new tag gen/erp/sales/v1.x.y and the root `go.mod` requiring it |

## Before changing code

1. Is this about the order (here), the stock (`erp/inventory`), the money owed (`erp/finance`) or the opportunity (`crm/opportunity`)? Only the first goes here.
2. Does the change add a call between CRM and ERP? Stop: only events cross that line.
3. In the confirmation chain, what happens if this new step times out, and is a reservation already held at that point?
4. Request path or event handler? That decides `UserClient` or `SystemClient`, and whether `besdk.ScopeOf` is available.
5. Any new decimal input: strict pattern, sign and size checked before it reaches SQL; a red test with `NaN` first.
6. Contract change? Append only. Did `gen/` change? Then the contract package needs a new tag and `go.mod` must require it.
7. A new REST route is registered with `besdk.GET` / `POST` and a permission key from `assembly.yaml`; a new list or stats endpoint filters by the same scope as `GET /orders` and has a two-users test on a real database.
8. Bump `metadata.version` before the first change after a release; update BRICKKIT.md and `docs/design.md` in the same commit as the code.

<!-- brickkit:managed:begin lang=en -->
<!-- maintained by brickkit (init, add, remove, upgrade, skills update): edits between these markers are overwritten -->

## BrickKit

This is a BrickKit component: `component.yaml` is all the platform reads. The rules it relies on:

- `configSchema` keys are the environment variable names the code reads. Never use a reserved name: `COMPONENT_ID`, `COMPONENT_VERSION`, `PORT`, `BRICKKIT_SERVED_MEMBERS`, `BRICKKIT_SERVED_MEMBERS_CONFIG`, or any `*_ENDPOINT`.
- Dependencies are exact versions. A dependency's address arrives as `<ID>_ENDPOINT`; an optional dependency that is absent has no variable at all, so read it with a fallback.
- `/healthz` checks only this process, never a dependency. The migration command runs from the same image and must fail on an argument it does not know.
- `BRICKKIT.md` travels to every project that uses this component and is read there without the repository: keep it in step with the code, with no relative links.
- Release: raise `metadata.version`, commit, push, `brickkit release`. `brickkit lint` checks the manifest and these docs — inside a project, run in this directory, it checks only this component (`--all` for the whole project).
- The full rules are in the `brickkit-component` skill (`.claude/skills/brickkit-component/SKILL.md` at the root of the project or repository where skills are installed; `brickkit skills update` installs it); for flags ask `brickkit <command> --help`; BrickKit's own documentation is `brickkit docs`.
<!-- brickkit:managed:end -->
