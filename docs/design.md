[English](design.md) · [中文](design.zh.md)

# erp/sales design

Conclusions only, for whoever changes this component's design. How to use it: `BRICKKIT.md`; how the code is laid out: `AGENTS.md`.

## Boundaries

This component owns the sales order from quotation to shipment: the order and its lines, the price list, and the orchestration that reserves stock when an order is confirmed and releases it when something after the reservation fails.

| Not here | Owner | Why |
|---|---|---|
| Who the customer is, the credit limit value | `mdm/customer` | Checked with `BatchGet` and copied onto the order at creation; the limit is kept locally from its events |
| What the product is, units | `mdm/product` | Same: checked and snapshotted at creation |
| Whether there is enough stock, the reservation | `erp/inventory` | This component asks for a reservation; "enough" is decided by a condition in inventory's own SQL. The caller never computes availability |
| Receivables, vouchers, the authoritative credit exposure | `erp/finance` | It listens to `sales.order.created.v1` and may reject the credit afterwards (`finance.credit.rejected.v1`) |
| Opportunities and the pipeline | `crm/opportunity` | CRM and ERP have no synchronous edge; a won opportunity reaches this component as an event |
| The approval and exception inbox | `infra/workflow` | The inbox holds no business rules: this component opens exception tasks and reacts to their completion |
| Shipping execution, carriers, invoicing | nobody yet | `ShipOrder` only turns the reservation into an issue and the order into `SHIPPED` |

**Order states.** `DRAFT` (a quotation and an order are the same row; converting one into the other would move numbers, lines and snapshots for nothing) → `CONFIRMED` → `SHIPPED`; `DRAFT` or `CONFIRMED` → `CANCELLED`; `SUSPENDED` from finance's credit rejection or from three failed compensations. `COMPLETED` and `CLOSED` exist in the status set with no command leading to them yet. `CANCELLED` is terminal and never revived: cancelling released a reservation in another process, and reviving would have to reserve again when the stock may be gone — compensation across a service boundary is not symmetric. A suspended order returns to `DRAFT` (not to "confirmed") when a person approves the compensation exception task: the confirmation never committed, so `DRAFT` is the only honest starting point, and confirming again is an explicit act by a person or caller. There is no `locked` flag; if one is ever needed, it is a boolean beside the state, not a state.

## Owned data

| Table | Partitioned | Notes |
|---|---|---|
| `sales_orders` | monthly, by `created_at` | header: customer and name snapshot, status, `total_amount NUMERIC(18,2)`, `dept_id` / `dept_path` / `owner_id` (snapshot at creation), `reservation_id`, `compensation_attempts`, `suspended_reason`, `source_opportunity_id` (empty for hand-entered orders), `version` |
| `sales_order_items` | monthly, by the header's `created_at` (`order_created_at`) | snapshot of SKU, name, unit, `qty NUMERIC(18,6)` (`CHECK qty > 0`), unit price, discount (`0 ≤ d < 1`), tax rate, subtotal |
| `pricelists`, `pricelist_items` | no | ordered rules: `applied_on` `PRODUCT` / `CATEGORY` / `GLOBAL`, `min_quantity`, validity dates, unit price, discount, `price_limit`, tax rate |
| `customer_snapshots` | no | `credit_limit` (from `mdm.customer.*`), `credit_exposure` (nothing maintains it yet: see Open questions), `version` |
| `sales_order_reconciliation_queue` | no | a row per confirmation whose reservation state stayed unknown |
| `command_idempotency` | no | claim-first idempotency of the four commands |
| `event_outbox`, `event_inbox` | weekly, by `created_at` | outbox for the pump; inbox for consumer deduplication |

`order_no` is `SO` + the ID: unique, gaps allowed (an insert that rolls back still consumes the sequence). A sales number with gaps is not an audit problem; that applies to an accounting posting number, not here. Order lines are full snapshots: a product renamed later must not rewrite past orders, and the product data lives in another process where no join is possible anyway.

## Contract surface

gRPC `erp.sales.v1.SalesService`:

| rpc | Kind | Notes |
|---|---|---|
| `CreateOrder` | write | idempotent; creates `DRAFT`, reserves nothing |
| `ConfirmOrder` | write | idempotent; the TCC chain below |
| `CancelOrder` | write | idempotent; releases the reservation of a confirmed order; cancelling a cancelled order returns it unchanged |
| `ShipOrder` | write | idempotent; `ConfirmIssue` in inventory, then `SHIPPED` |
| `CalculatePriceDryRun` | read | pure: the same pricing as `CreateOrder`, nothing stored |
| `GetOrder`, `ListOrders` | read | data-scoped; cursor paging, default window the last 90 days |
| `BatchGetOrder` | read | the batched read for other components; missing IDs in `missing_ids` |
| `GetOrderStatus` | read | for a caller whose command timed out; `UNSPECIFIED` = no such order |

REST under `/erp/sales`, every route behind a key from `assembly.yaml`: `GET /orders`, `GET /orders/{id}`, `GET /stats/summary` (`erp.sales.view`); `POST /orders`, `POST /price/dry-run` (`erp.sales.create`: the dry run is part of preparing an order, not a separate right); `POST /orders/{id}/confirm`, `/cancel`, `/ship` (`erp.sales.confirm`, `.cancel`, `.ship`). `BatchGetOrder` and `GetOrderStatus` are not on REST: they are component protocols. The dry run must be on REST: the order entry page prices as the user types, and front ends never compute money.

**The confirmation chain** (`ConfirmOrder`, also used by the won-opportunity path):

```
① BatchGet customer (mdm/customer)   failure → return, nothing to compensate
② BatchGet products (mdm/product)    failure → return, nothing to compensate
③ credit check on the local copy      over the limit → return (no network; before any reservation)
④ Reserve (erp/inventory)             refusal → return; timeout → status first, see below
⑤ confirm + outbox, one transaction    failure → compensate: CancelReservation
```

The credit check sits before the reservation on purpose: an order clearly over its limit would otherwise take stock, be rejected, and be compensated for nothing. A `Reserve` timeout is never answered with `CancelReservation`: a missing response does not mean the request was not processed. `GetReservationStatus` decides: `RESERVED` → continue with ⑤; not found → `503`, retry with the same key; `CANCELLED` → fail; the status call times out too → a row in `sales_order_reconciliation_queue`, `503`, and the user request is not blocked. A refusal (not enough stock) fails the whole order: there is no partial reservation, which is stricter than Odoo and deliberate — a partial success would turn compensation into "compensate a part". A failed compensation retries in place with the same idempotency key, detached from the caller's cancellation (a reservation that succeeded must be released even if the client went away), with a growing pause; three consecutive failures mark the order `SUSPENDED` and open an exception task for `EXCEPTION_ASSIGNEE_SUB` (idempotency key derived from the confirmation's key, so a replay of the same command finds the same task). Never an endless retry: compensating a compensation is worse than not compensating. Every call to a dependency has its own timeout (5 s); commands that need no compensation (create, cancel, ship) just fail on a timeout and are retried by the caller with the same key.

**Pricing**: one ordered rule table, the first match wins — by specificity (`PRODUCT`, `CATEGORY`, `GLOBAL`), then `min_quantity` descending, then ID descending; validity dates against the current date. One rule gives unit price, discount, `price_limit` and tax together; rules never stack, so "why this price" always has one answer. Subtotal = qty × unit price × (1 − discount), computed exactly and rounded once, half away from zero, to cents (as `NUMERIC(18,2)` rounds). There is no API for price lists yet; a migration seeds one global rule so the chain can run.

**Input checks**: a quantity is a positive plain decimal with at most 12 integer and 6 fractional digits (the column is `NUMERIC(18,6)`); signs, exponents, `NaN`, `Inf`, hex and digit separators are rejected with `400`. `NaN` stored in `NUMERIC` sorts above every number, so a `CHECK` would let it through; that is why the check is in code, before SQL. The same rule (non-negative, at most 16 integer and 2 fractional digits) guards the credit limit arriving in customer events.

**Statistics** (`GET /stats/summary`): the last 7 / 30 / 90 calendar days including today (UTC), by the order's `created_at`. `order_count` counts every order created in the window; `confirmed_count` and `total_amount` count only `CONFIRMED`, `SHIPPED` and `COMPLETED`; `by_day` lists every day (zero days included), `by_status` all seven states. Filtered by exactly the same scope as `GET /orders`, including `view`.

**Errors and logs**: invalid input, a wrong state, insufficient stock and an exceeded credit limit are `400`; unknown `404`, and reading a single order outside the scope is `404` too, worded exactly the same, so the scope never reveals that the order exists; a command (confirm, cancel, ship) on an order outside the scope `403`; reservation state unknown `503`. Such caller errors are logged at INFO, transient ones at WARN; ERROR is kept for what an operator must act on (an unreachable dependency, a database error, a suspended order).

## Events

Published through the outbox, in the same transaction as the state change:

| Subject | When | Payload |
|---|---|---|
| `sales.order.created.v1` | `ConfirmOrder` (not `CreateOrder`: a draft must not produce a receivable) | order ID and number, customer, items (product, qty, subtotal), total, version, `source_opportunity_id` (optional; empty for hand-entered orders) |
| `sales.order.cancelled.v1` | `CancelOrder` | order ID, reason |
| `sales.order.shipped.v1` | `ShipOrder` | order ID, items |

`erp/finance` consumes `created` (receivable, credit exposure). `crm/opportunity` consumes `created` too and, when `source_opportunity_id` is its opportunity, fills in the opportunity's `order_id`; this keeps CRM and ERP connected by events only, with no dependency edge in either direction. The field was added to the existing subject, not as a new event, so existing consumers are untouched.

Consumed:

| Subject | From | Effect |
|---|---|---|
| `finance.credit.rejected.v1` | `erp/finance` | the order becomes `SUSPENDED` with the exposure and limit in the reason; terminal orders are left alone |
| `mdm.customer.created.v1`, `.updated.v1` | `mdm/customer` | the credit limit copy; strictly increasing `version`; an update without `credit_limit` keeps the limit; an invalid limit rejects the event |
| `infra.workflow.task.completed.v1` | `infra/workflow` | only `source_component = erp/sales`, `source_aggregate = sales_order`, `action = APPROVED`: a suspended order whose reason is the compensation failure returns to `DRAFT`. `REJECTED` does nothing (its meaning for an exception is unclear); this component never calls `CloseTask` |
| `crm.opportunity.won.v1` | `crm/opportunity` | create and confirm an order (below) |

`finance.voucher.posted.v1` is not consumed: it has no customer ID, so it cannot say whose exposure changed.

**Won opportunity → order.** The handler runs with no user: clients use this component's own identity, and owner and department come from the event (`owner_id`, `dept_path` are required fields there). It reuses the confirmation chain's helpers, not the public `CreateOrder` / `ConfirmOrder`, which read the caller's scope. The idempotency key is derived from the opportunity ID, so the same event can produce only one order. The quoted unit price in the event is for reference only; the order is priced by this component. Any failure opens an exception task for the opportunity's owner (`source_aggregate = opportunity_conversion`, which nothing resumes automatically) and is never reported back to CRM: a won opportunity stays won whatever happens in ERP. The handler always returns success, because a returned error would only be logged, not redelivered.

## Dependencies

| Dependency | Used for | Called from |
|---|---|---|
| `mdm/customer` | `BatchGet`: active customer, name snapshot | create, confirm |
| `mdm/product` | `BatchGet`: active products, SKU / name / unit / category snapshot | create, confirm |
| `erp/inventory` | `Reserve`, `GetReservationStatus`, `CancelReservation`, `ConfirmIssue` | confirm, cancel, ship |
| `infra/workflow` (optional) | `CreateTask` for exception tasks | compensation, won-opportunity conversion |

When `infra/workflow` is absent the platform injects no address at all; the component checks `rt.Config.Endpoint` and skips the task (the order is still suspended, the failure logged). On a user request every call forwards the caller's token; only the event handler uses the system identity (`make gates` scans for `SystemClient` on request paths).

| Not a dependency | Why |
|---|---|
| `crm/opportunity` | CRM and ERP have no synchronous edge; won opportunities arrive as events, order IDs go back as events |
| `erp/finance` | nothing here calls it: finance listens to `sales.order.created.v1` and answers with `finance.credit.rejected.v1`. An edge with no call would only make every run of this component start finance too |
| `infra/iam-casdoor`, `infra/authz` | tokens and the permission bundle come from URLs in the configuration |

## Place in the synchronous call graph

A root: outgoing edges to `mdm/customer`, `mdm/product`, `erp/inventory` and the optional `infra/workflow`, none of which call back, so no path returns here. Incoming calls are from users (REST) and from other components' reads (`BatchGetOrder`, `GetOrderStatus`). It never points at a CRM component.

## Partitioning and archiving

`sales_orders` and `sales_order_items` are partitioned by month, items by the header's `created_at` so that the lines of one order always sit in the same partition as their header and leave together when it is archived. The component creates partitions three months ahead; the migration's initial partitions use the same names, so the loop finds them. `event_outbox` / `event_inbox` are weekly, four weeks ahead. Price lists and customer snapshots are small and permanent.

Archiving is designed but not implemented: an order moves to `erp_sales_archive` only when it is terminal (`COMPLETED`, `CANCELLED`, `CLOSED`) **and** older than 12 months — a 14-month-old order that has not shipped is still alive.

## Data scopes

Two dimensions on `sales_orders`: `org` (`dept_path` prefix) and `owner` (`owner_id` equals the caller), the two basic views of a sales organisation, "my department and below" and "my orders". The scope columns are a snapshot taken at creation, not a lookup of where the person is now; otherwise a salesperson's past orders would move with them when they change department, silently changing every report.

Lists and statistics choose one view with `view` (`dept` by default, or `mine`; any other value is `400`, so a misspelt `mine` never silently shows the whole department) and never AND or OR them (AND would narrow "my department" to "mine in my department"). A single order (get, confirm, cancel, ship) is visible when either matches; outside the scope a read answers `404` as if the order did not exist, a command `403`. Both operands always come from the caller's real scope (`besdk.ScopeOf`), and neither may default to "everything". The scope is pushed into the SQL `WHERE`, never filtered afterwards in Go (paging would break). Orders from won opportunities take owner and department from the event.

**A caller with no department** (the token's `dept_path` is empty: a new account not yet placed in the organisation) matches no `org` row: the SDK gives a department prefix that no real path starts with. Their `dept` view is an empty list and zero statistics; a single order is visible only if they own it. They may still create orders: the snapshot then has an empty `dept_path` and `dept_id`, and the order is visible only to its owner (nobody's department prefix, not even the root's, matches an empty path). The SDK's no-department value is never written into a row; the repository rejects a `dept_path` that is neither empty nor starts with `/`. Seeing the whole organisation means sitting in its root department, or holding the explicit root marker `/`, which as a prefix matches every order that has a department. The repository treats an empty prefix or owner as a caller's mistake (`InvalidArgument`), never as "no restriction".

## Reference implementations

| Project | Version | Module consulted | What was borrowed | License | Usage |
|---|---|---|---|---|---|
| Odoo | 17.0 | `addons/sale/models/sale_order.py` | quotation and order in one table with a state; the atomic boundary of confirmation | LGPL-3 | Borrowed reasoning |
| Odoo | 17.0 | `addons/sale/models/sale_order_line.py` | order lines fully snapshotted (computed once, stored) | LGPL-3 | Borrowed reasoning |
| Odoo | 17.0 | `addons/product/models/product_pricelist_item.py` | the price list as an ordered rule table where the first match wins; three scopes of application | LGPL-3 | Borrowed reasoning |
| metasfresh | master | `de.metas.business` pricing rules | `price_limit` (lowest price) as a first-class field, not a formula parameter: the line most often audited | GPL-2/3 | Borrowed reasoning |

**Deliberately avoided**: Odoo's `action_draft()` reviving a cancelled order; Odoo confirming an order with a partial reservation when stock is short; pricing logic spread over many computed fields; metasfresh's composable rule chains, too configurable to be set up in the field. The cross-service compensation chain has no reference: both systems are single-process with one transaction, so they neither have the problem nor its solution. Its correctness rests on fault-injection tests against real dependencies (real insufficient stock, real timeouts), not on mocks.

**Slot family**: four places where reasonable implementations differ by customer — reservation strictness (at confirmation / manual / by date), shortage policy (fail the order / partial shipment with back order / wait), invoicing basis (ordered / delivered quantity), pricing engine (ordered table / rule chain); Odoo itself makes three of them configuration options. They are not slots: other components depend on this one, and a slot is only possible where nobody holds a dependency edge. The answer is a fork of this component (same `metadata.id`) for a customer who needs the other choice. Chosen here: full reservation at confirmation, fail on shortage, no invoicing yet, ordered table.

## Open questions

| Question | Current answer |
|---|---|
| The local credit check knows the limit but not the exposure (`credit_exposure` stays `0`): `finance.voucher.posted.v1` has no customer ID, and there is no reconciliation with `erp/finance` | The authoritative decision is finance's `finance.credit.rejected.v1`, which suspends the order afterwards. Period checks before changing a historical order, or a reconciliation call, would bring back a synchronous edge to `erp/finance`; that is a new design decision, not a gap to fill |
| Rows in `sales_order_reconciliation_queue` are written, but nothing reads them | The caller gets `503` and retries with the same key, which settles the order; a reconciliation job is not built |
| `ShipOrder`: `ConfirmIssue` succeeded, the local `SHIPPED` failed | The order stays `CONFIRMED` while the stock is issued; a retry with the same key completes it (both sides are idempotent). Nothing does it automatically |
| `GetOrder` / `ListOrders` over gRPC need the caller's claims, which the gRPC server side of be-sdk-go does not establish | Such calls fail with an internal error; users read through REST. `BatchGetOrder` and `GetOrderStatus` have no scope by design (component protocols) |
| `COMPLETED` / `CLOSED` have no command; no invoicing | Not needed by any screen yet; invoicing is a slot-family choice (see above) |
| Price lists have no API | Rows inserted directly; a management API is an append-only contract change when a screen needs it |
| One warehouse for every confirmation (`DEFAULT_WAREHOUSE_ID`) | Choosing a warehouse per line or customer needs a warehouse query in `erp/inventory` and an allocation rule |
| The won-opportunity handler holds the inbox transaction's database connection while it calls dependencies | At most a few call timeouts per won event; acceptable at the expected volume |
| Statistics bucket days in UTC | A user in UTC+8 sees the day change at 08:00; a time-zone parameter is an additive change if the dashboard needs it |
| Archiving of terminal orders older than 12 months | Designed, not built |
