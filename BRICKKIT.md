# erp/sales

## Purpose

Sales orders from draft to shipment: the order lines with their price, the price list that sets that price, and the confirmation that reserves stock in `erp/inventory` and releases it again when anything after the reservation fails. A won opportunity in `crm/opportunity` becomes a confirmed order here automatically, through an event.

**Owns**

- Orders and their lines (`sales_orders`, `sales_order_items`): a quotation is an order in `DRAFT`; `CONFIRMED`, `SHIPPED`, `CANCELLED` (terminal) and `SUSPENDED` follow from the commands and events below. Product SKU, name, unit and price are snapshots taken when the order is created. `COMPLETED` and `CLOSED` exist in the status set, but no command leads to them yet.
- Pricing: an ordered price list (`pricelists`, `pricelist_items`) where the first matching rule wins (product, then category, then global; higher minimum quantity first). Prices are always computed here; `POST /price/dry-run` exists so that front ends never compute money themselves.
- The confirmation orchestration: check customer and products, check the credit limit locally, reserve stock, confirm in one transaction with the event; compensate (release the reservation) when the local confirmation fails; mark the order `SUSPENDED` after three failed compensations.
- Converting a won opportunity (`crm.opportunity.won.v1`) into a confirmed order; the order remembers it in `source_opportunity_id`.
- A local copy of each customer's credit limit (`customer_snapshots`), kept from `mdm.customer.*` events.
- The events `sales.order.created.v1` (published when an order is confirmed, not when it is drafted), `sales.order.cancelled.v1`, `sales.order.shipped.v1`.

**Does not own**

- Who the customer is and the credit limit value: `mdm/customer`. Products, categories and units: `mdm/product`.
- Whether there is enough stock, and the reservation itself: `erp/inventory`. This component asks for a reservation; it never computes availability.
- Receivables, vouchers and the authoritative credit exposure: `erp/finance`, which listens to this component's events and can reject an order's credit afterwards (`finance.credit.rejected.v1`).
- Opportunities and the sales pipeline: `crm/opportunity`. There is no synchronous call between CRM and ERP; the two sides meet only through events.
- The approval and exception inbox: `infra/workflow`. This component creates exception tasks there and reacts to their completion; the inbox holds no sales rules.
- Physical shipping, carriers, invoicing: nobody yet. `ShipOrder` only moves the reservation to an issue in `erp/inventory` and the order to `SHIPPED`.

## Before you deploy

- **PostgreSQL**: a schema `erp_sales`, plus `erp_sales_archive` if your schema convention creates one (this component never writes to it). A login role `erp_sales_rw` with `USAGE` and `CREATE` on `erp_sales`, and its password. The migration runs as this role and creates the tables, so the role owns them; that matters because the running component creates the monthly partitions of `sales_orders` / `sales_order_items` (three months ahead) and the weekly partitions of `event_outbox` / `event_inbox` (four weeks ahead) itself. BrickKit creates none of this; in the BrickEnterprise assembly project `make dev-env` writes the password into `.env` and `make db-init` creates the schemas, role and grants.
- **NATS** reachable at `NATS_URL`: events go out through an outbox table and a background pump, and the consumed events (see Contracts) arrive here. The component starts without NATS reachable; outgoing events wait in the outbox.
- **A warehouse** in `erp/inventory` whose ID goes into `DEFAULT_WAREHOUSE_ID`: every confirmation reserves stock in that one warehouse. In the BrickEnterprise project `erp/inventory`'s migration seeds warehouse `1` (`WH-EAST`).
- **Prices**: the migration seeds one global rule (unit price `100.00`, no discount, tax `0.13`) so orders can be priced at all. There is no API to manage price lists; real prices are rows you insert into `pricelist_items`.
- **Credit limits**: an order is confirmed only if the customer's credit limit, copied from `mdm.customer.created.v1` / `.updated.v1` events, covers it. A customer whose events have not arrived yet has limit `0`, so its orders cannot be confirmed: start this component before customers are created, or replay the customer events.
- **Departments**: a user's `dept_path` (issued by `infra/authz`) decides what the `dept` view shows. A user not yet placed in any department sees only the orders they own; whoever should see the whole organisation belongs in its root department.
- **Authorization** (`infra/authz`) and **identity** (`infra/iam-casdoor`, or any IAM serving a JWKS) reachable at `AUTHZ_BUNDLE_URL` and `IAM_JWKS_URL` for the REST routes to answer anything but errors. They are configuration, not dependencies.
- Demo data (optional): `make seed` in the component directory creates five orders in different states through the real REST API, once this component, `infra/iam-casdoor`, `infra/authz`, `mdm/customer` and `mdm/product` are running (it seeds those first).

## Dependencies

- `mdm/customer` — `BatchGet`: the customer must exist and be active; its name is copied onto the order. Called when an order is created and again when it is confirmed.
- `mdm/product` — `BatchGet`: every product must exist and be active; SKU, name, base unit and category are copied onto the order line (the category selects category price rules).
- `erp/inventory` — `Reserve` when confirming (all lines from `DEFAULT_WAREHOUSE_ID`; not enough stock fails the whole order), `GetReservationStatus` after a `Reserve` timed out, `CancelReservation` to compensate and when a confirmed order is cancelled, `ConfirmIssue` when shipping.
- `infra/workflow` (optional) — `CreateTask` for exception tasks: three failed compensations (assigned to `EXCEPTION_ASSIGNEE_SUB`), or a won opportunity that could not be turned into a confirmed order (assigned to the opportunity's owner). When it is absent no task is created; the order is still suspended and the failure is logged.

`erp/finance` is not a dependency: nothing here calls it. It listens to `sales.order.created.v1` and answers with `finance.credit.rejected.v1`, so the two meet only through events.

On the user's path every call forwards the caller's token (`UserClient`); only the conversion of a won opportunity, which runs in an event handler with no user, calls with this component's own identity. Every call has its own 5-second timeout.

The authorization bundle and the JWKS are fetched from the URLs in the configuration, not through a dependency edge. Without them every protected route fails closed: no or invalid token → `401`; `IAM_JWKS_URL` empty or unreachable → `403`; the bundle never fetched yet → `503`; a valid user without the permission key → `403`. `/healthz` stays `200` throughout: it reports only that this process is alive.

## Configuration

| Variable | Meaning |
|---|---|
| `PG_HOST` | PostgreSQL host. Usually the project's shared value (`$var:PG_HOST`). |
| `PG_PORT` | PostgreSQL port; default `5432`. |
| `PG_DATABASE` | The database holding the `erp_sales` schema (`$var:PG_DATABASE`). |
| `PG_USER` | The login role, `erp_sales_rw` as a literal. Inside a shell the shell logs in with its own role and switches to this one per transaction (`SET LOCAL ROLE`), so the role name must be `<PG_SCHEMA>_rw`. |
| `PG_PASSWORD` | Password of `PG_USER`. Secret: write `${ERP_SALES_DB_PASSWORD}` (or your secret store's reference), never the value. |
| `PG_SCHEMA` | Schema of all tables, the outbox and the migration state table (`schema_migrations_erp_sales`); default `erp_sales`. Write the literal anyway so every component's schema is visible in one place. |
| `NATS_URL` | NATS server for publishing and consuming events (`$var:NATS_URL`). |
| `OTEL_BASE_URL` | OpenTelemetry collector base URL; empty (the default) exports nothing. |
| `AUTHZ_BUNDLE_URL` | URL of the authorization bundle the permission check polls, for example `http://infra-authz-2-0-0:8223/authz/bundle`. Required: without it every protected route answers `503`. Keep it in step with the authz version the project runs. |
| `IAM_JWKS_URL` | URL of the JWKS used to verify user tokens locally, for example `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`. Required: without it every protected route answers `403`. |
| `DEFAULT_WAREHOUSE_ID` | ID of the `erp/inventory` warehouse every confirmation reserves from (a literal, `"1"` in the BrickEnterprise project). Required and must not be empty: the component refuses to start without it, because reserving from the wrong warehouse would go unnoticed. |
| `EXCEPTION_ASSIGNEE_SUB` | The user (`sub`) who receives the exception task when a compensation has failed three times, for example an operations reviewer. Default empty: no task is created for that case, the order is still suspended and an error is logged. |

## Contracts

- `contracts/erp/sales/v1/sales.proto` — gRPC `erp.sales.v1.SalesService`: `CreateOrder`, `ConfirmOrder`, `CancelOrder`, `ShipOrder` (each takes an `idempotency_key`), `CalculatePriceDryRun`, `GetOrder`, `ListOrders` (cursor paging, no offset; filters customer, status, time window, `source_opportunity_id`), `BatchGetOrder` (missing IDs in `missing_ids`, not an error), `GetOrderStatus` (for a caller whose command timed out; `UNSPECIFIED` means no such order). `Order.source_opportunity_id` is the opportunity a won-deal order came from, empty for orders entered by hand. `GetOrder` and `ListOrders` filter by the caller's data scope and need the caller's identity; `BatchGetOrder` and `GetOrderStatus` are component-to-component reads without a scope. The Go package is the separate module `github.com/brickKit/erp-sales/gen/erp/sales`.
- `contracts/sales.openapi.yaml` — REST under `/erp/sales`, every route behind a permission key: `GET /orders` (`erp.sales.view`; `view=dept` (default) or `mine`, any other value is `400`; `customer_id`, `status_filter`, `source_opportunity_id`, `created_after` / `created_before` (RFC 3339, default the last 90 days), `cursor`, `page_size`), `GET /orders/{id}` (`erp.sales.view`), `POST /orders` (`erp.sales.create`; quantities are positive decimal strings, at most 12 integer and 6 fractional digits), `POST /orders/{id}/confirm` (`erp.sales.confirm`), `POST /orders/{id}/cancel` (`erp.sales.cancel`), `POST /orders/{id}/ship` (`erp.sales.ship`), `POST /price/dry-run` (`erp.sales.create`), `GET /stats/summary` (`erp.sales.view`; `period=week|month|quarter` (7 / 30 / 90 days, default `month`) and the same `view` as the list, one view or the other, never both; order count, confirmed count and amount, per day and per status). Amounts are decimal strings. Errors: invalid input, an order in the wrong state, or not enough stock → `400`; unknown order, or reading an order outside the caller's scope → `404` (the same answer, so the scope never reveals that an order exists); confirming, cancelling or shipping an order outside the scope → `403`; reservation state unknown after a timeout → `503` (retry with the same key).
- `contracts/events/sales.events.json` — published through the outbox: `sales.order.created.v1` (on confirmation; carries `source_opportunity_id`, optional, empty for hand-entered orders), `sales.order.cancelled.v1`, `sales.order.shipped.v1`. Consumed: `finance.credit.rejected.v1` (suspend the order), `mdm.customer.created.v1` / `.updated.v1` (credit limit copy; an update without `credit_limit` keeps the limit), `infra.workflow.task.completed.v1` (an approved compensation exception returns the order to `DRAFT`), `crm.opportunity.won.v1` (create and confirm an order).
- `assembly.yaml` — this project's metadata: the five permission keys above, the menu entry, the edge route `/erp/sales/**`, the schema and role, and the data scopes `org` (`dept_path` prefix) and `owner` (`owner_id`) on `sales_orders`. A caller with no department (empty `dept_path` in the token) matches no `org` row: their `dept` view is empty and their statistics zero, and they see and act on only the orders they own. They can still create orders, which then have an empty `dept_path` and are visible only to them. `dept_path = "/"` is the explicit root marker and matches every order that has a department.

## Shell declaration

Not a shell. It can be hosted in a Go shell (in the BrickEnterprise project, `be/go-core`) or run on its own; the code is the same either way.
