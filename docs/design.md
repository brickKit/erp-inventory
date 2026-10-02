[English](design.md) · [中文](design.zh.md)

# erp/inventory design

Conclusions only, for whoever changes this component's design. How to use it: `BRICKKIT.md`; how the code is laid out: `AGENTS.md`.

## Boundaries

This component is the one source of truth for "is this stock here, and may I take it": balances, the movement ledger, warehouses, reservations, and the oversell decision. It is one of the project's three hubs, the physical-command hub: the only writer of stock movements.

| Not here | Owner | Why |
|---|---|---|
| What a product is (SKU, name, unit, whether it is tracked by batch or serial) | `mdm/product` | This component answers "how many", not "what". It keeps a summary copy of the tracking type only (see Events) |
| Why stock moves (sales orders, work orders) | `erp/sales`, future manufacturing | It receives "reserve 3", not the order; `order_id` is stored only for tracing and never drives a decision |
| The value of stock, cost methods, accounting entries | `erp/finance` | Quantities and amounts are kept apart: movements carry quantities, finance prices them from `erp.inventory.adjusted.v1` |
| Purchase plans and suppliers | a future purchasing component | It only offers `Receive` and does not care where goods come from |

## Owned data

| Table | Partitioned | Notes |
|---|---|---|
| `warehouses` | no | tens of rows; `code` unique; seeded by migration `003_seed_warehouses` (`WH-EAST`, `WH-SOUTH`); no management API |
| `inventory_balances` | never | one row per (product, warehouse), unique; `on_hand_qty`, `reserved_qty` as `NUMERIC(18,6)`; `CHECK` both ≥ 0 and `reserved_qty ≤ on_hand_qty` as the database's backstop |
| `inventory_movements` | monthly, by `created_at` | append-only; signed `qty`; `reason` in `RECEIVE` / `ISSUE` / `ADJUST_GAIN` / `ADJUST_LOSS`; `note` (adjust reason), `order_id`, `batch_no`, `serial_no`; `CHECK serial_no = '' OR abs(qty) = 1` |
| `inventory_reservations` | no | one row per item of one `Reserve` call; `reservation_id` (from a sequence) groups the rows of a call; status `RESERVED` → `CONFIRMED` or `CANCELLED`, all rows of a group in one transaction |
| `product_tracking_snapshots` | no | `product_id`, `tracking_type`, `version`, from `mdm/product` events |
| `warehouse_access` | no | (`sub`, `warehouse_id`): who may see and move stock in which warehouse |
| `command_idempotency` | no | `idempotency_key` primary key, the command and its result; shared by the five write commands |
| `event_outbox`, `event_inbox` | weekly, by `created_at` | the outbox the pump publishes from, the inbox the consumer deduplicates with |

`warehouse_id` is a real foreign key inside the schema; `product_id` is `TEXT` with no foreign key, an opaque ID. Terminal states: every movement row is final when written; `CONFIRMED` and `CANCELLED` reservations are final.

**The ledger is append-only and balances are its projection**, updated in the same transaction. A correction is a new movement; backdating a receipt or issue is not supported, because rewriting history means recomputing every later row (the cost ERPNext's repost mechanism pays: advisory locks, checkpoints, unbounded cascades). When balance and ledger disagree, the ledger wins.

**Oversell protection is a conditional update**: `UPDATE inventory_balances SET reserved_qty = reserved_qty + $1 … WHERE product_id = $2 AND warehouse_id = $3 AND on_hand_qty - reserved_qty >= $1`. `RowsAffected() == 0` is insufficient stock (`FailedPrecondition`). Checking with a `SELECT` first leaves a window; the symptom is occasional overselling that no unit test shows. `Adjust` puts "not below zero, not below reserved" into its `WHERE` the same way. The unique constraint on (product, warehouse) is what keeps concurrent first receipts from creating duplicate balance rows (Odoo's `stock.quant` has no such constraint and merges duplicates afterwards).

## Contract surface

gRPC `erp.inventory.v1.InventoryService`:

| rpc | Kind | Idempotency | Permission key | Notes |
|---|---|---|---|---|
| `Reserve` | write | `idempotency_key` | `erp.inventory.reserve` | several items, all or nothing; the first step of the sales TCC |
| `CancelReservation` | write | `idempotency_key` | `erp.inventory.reserve` | the compensation; not found, cancelled or confirmed are reported as the status, not as errors |
| `ConfirmIssue` | write | `idempotency_key` | `erp.inventory.issue` | on hand and reserved both decrease, one movement per item; `batch_no` / `serial_no` apply to every item, so items needing different ones go in separate reservations |
| `GetReservationStatus` | read | — | `erp.inventory.view` | by `reservation_id`, or by the `idempotency_key` of the `Reserve` (only keys of `Reserve` count); returns the real `reservation_id` |
| `Receive`, `Adjust` | write | `idempotency_key` | `erp.inventory.receive`, `.adjust` | also on REST |
| `GetBalance` | read | — | `erp.inventory.view` | a missing row is zero, not an error |
| `BatchGetBalance` | read | — | `erp.inventory.view` | the batched read callers use instead of a loop of `GetBalance` |
| `ListMovements` | read | — | `erp.inventory.view` | cursor paging and the default 90-day window |

The four reservation calls are never on REST: they are the protocol between components; the human operation is a delivery note on the sales side. `BatchGetBalance` is gRPC only too. The permission keys of the reservation calls exist for role setup and audit; no REST route checks them.

**Why `GetReservationStatus` exists and must tell `NOT_FOUND` from `CANCELLED`**: after a `Reserve` times out, the caller does not know whether it landed. `NOT_FOUND` means it did not (safe to retry with the same key; claim-first idempotency makes the retry harmless); `CANCELLED` means it landed and was undone. Merging them makes the caller either double-reserve or wrongly give up. When `Reserve` itself timed out the caller has no `reservation_id`, so the status can be queried by the `Reserve`'s `idempotency_key`: the key → reservation mapping becomes visible only when the `Reserve` commits, so "found by key" equals "committed".

REST under `/erp/inventory` (all behind a key): `POST /movements/receive`, `POST /movements/adjust`, `GET /balances` (one row), `GET /balances/list`, `GET /movements` (`created_after` / `created_before`, RFC 3339; malformed → `400`), `GET /warehouses`, `GET /stats/summary`, and the warehouse-access administration `GET` / `POST /warehouse-access/{sub}`, `DELETE /warehouse-access/{sub}/{warehouse_id}` (`erp.inventory.manage_access`).

**The three read endpoints for the pages and the dashboard** (`erp.inventory.view`, all limited to the caller's warehouses):

- `GET /warehouses` → `{ warehouses: [{ id, code, name, status }] }`, ordered by `id`, not paged: warehouses are seeded master data of tens of rows, like units of measure. A user without grants gets an empty list.
- `GET /balances/list` → `{ balances: [Balance], next_cursor }`, optional `product_id` and `warehouse_id`, keyset paging on the balance row `id`. No default time window: balances are current state, bounded by products × warehouses, and a stale row is still stock. A `warehouse_id` the caller may not see gives an empty page (as for movements), not `403`. `GET /balances` keeps its single-row meaning.
- `GET /stats/summary` → `{ sku_count, total_on_hand_qty, low_stock: [{ product_id, warehouse_id, available_qty }], low_stock_count }`. `sku_count` counts distinct products with on hand > 0; `total_on_hand_qty` sums on hand; a row is low stock when on hand − reserved < `LOW_STOCK_THRESHOLD`, so an open reservation can push a well-stocked row below it. The threshold is a decimal string compared as `numeric` in SQL, never a float. The list is ordered by available quantity and capped at 50 rows; `low_stock_count` (beyond the original request) says how many there are, so a truncated list is visible as such. The threshold is a configuration key, not a table: one number per deployment is enough until someone asks for per-product thresholds.

## Events

Published, through the outbox in the same transaction as the movement:

| Subject | When | Payload |
|---|---|---|
| `erp.inventory.adjusted.v1` | `Receive`, `ConfirmIssue` (one per item), `Adjust` | product, warehouse, signed quantity, movement ID, batch, serial, reason |
| `erp.inventory.transferred.v1` | — | reserved for transfers, not published |

`aggregate_id` is the movement ID and `version` is always 1: every movement is a new aggregate that never changes. `erp/finance` consumes it to make inventory vouchers; quantities only, finance applies its own cost method.

Consumed: `mdm.product.created.v1` and `mdm.product.updated.v1`, only `tracking_type`, into `product_tracking_snapshots`. `besdk.Consume` deduplicates by (subject, aggregate, version) and accepts only a greater version per subject; the two subjects can arrive out of order relative to each other, so the upsert also applies only when the incoming version is greater than the stored one.

## Dependencies

None. Expected but absent:

| Not a dependency | Why |
|---|---|
| `mdm/product` | The most tempting edge to add. `product_id` is opaque here: the caller (`erp/sales`) validates products before it reserves, so checking again would be a second round trip. Tracking types come by event; the cost is a short window in which a new product's tracking type is not known yet (its first receipt may miss a required batch number, which a person can correct), while a synchronous edge would put the hub inside a call chain for good |
| `erp/sales`, `erp/finance` | This component is called and publishes events; it calls no one. Finance learns about quantities through events, not calls |
| `infra/iam-casdoor`, `infra/authz` | tokens are verified locally against `IAM_JWKS_URL` and permissions come from the bundle at `AUTHZ_BUNDLE_URL`: configuration, not edges |

## Place in the synchronous call graph

A leaf: `erp/sales` (and future purchasing and manufacturing) and the mobile BFF call it; it calls no one, so it can never be part of a cycle. Finance hears from it only through events: that edge is in the event graph, not the call graph, and the two graphs are not to be mixed. Inventory and finance have no edge at all; the same upstream (`erp/sales`) commands both side by side.

## Partitioning and archiving

| Data | Hot | Policy |
|---|---|---|
| `inventory_balances` | always | never partitioned, never archived: it is the busiest write table and bounded by products × warehouses; partitioning would make the oversell `UPDATE` search partitions |
| `inventory_movements` | the last 12 months | partitioned monthly; the initial months come from the migration, later ones from the component's own maintenance loop (current month and three ahead, checked daily). Both must name a partition `<table>_YYYY_MM_01`, the name `ensurePartition` produces, or the loop tries to create an overlapping partition. Whole months older than 12 months are meant to move to `erp_inventory_archive`; nothing detaches them yet (see Open questions) |
| `inventory_reservations` | `RESERVED` rows | final rows are meant to be deleted after 30 days (the ledger keeps the trace); not implemented yet |
| `warehouses`, `product_tracking_snapshots`, `warehouse_access` | always | small, never archived |
| `event_outbox`, `event_inbox` | weekly partitions | rolled forward four weeks ahead by the component |

## Data scopes

Dimension `warehouse` on `inventory_balances`, `inventory_movements` and `inventory_reservations`, column `warehouse_id`, mode `in`: in a customer that runs several warehouses, the manager of the south warehouse must not see the east warehouse's stock. `warehouse_id` is already part of the business key, so these tables need no department or owner columns; those belong to the `org` and `owner` dimensions, which this component does not use.

Who may see which warehouse is not in the token and not in `infra/authz`: it is data of the component that owns the warehouses, in `warehouse_access`, granted through the warehouse-access routes. The service looks up the caller's `sub` and passes the list into the SQL of every scoped read and write (`= ANY($n)`); an empty list matches nothing. Every scoped endpoint has a test with two users and two warehouses that asserts neither sees the other's rows. The reservation calls and `BatchGetBalance` are not scoped by caller: they are the protocol between components, and the calling component decides which warehouse an order draws from.

## Reference implementations

| Project | Version | Module consulted | What was borrowed | License | Usage |
|---|---|---|---|---|---|
| ERPNext | v15 | `erpnext/stock/stock_ledger.py`, `doctype/stock_ledger_entry/` | ledger plus balance cache (`Bin`); and the cost of its repost mechanism, which is why backdating is not supported here | GPL-3 | Borrowed reasoning |
| ERPNext | v15 | `doctype/repost_item_valuation/` | counter-example: locks, checkpoints and unbounded cascades bought by allowing history to be rewritten | GPL-3 | Borrowed reasoning |
| Odoo | 17.0 | `addons/stock/models/stock_quant.py` | `available = quantity − reserved`; and the counter-example of no unique constraint on the quant | LGPL-3 | Borrowed reasoning |
| Odoo | 17.0 | `addons/stock/models/stock_move.py` | the state split; only `done` and `cancel` are terminal | LGPL-3 | Borrowed reasoning |
| Odoo | 17.0 | `addons/stock_account/models/stock_valuation_layer.py` | quantity and value in separate layers; here value leaves the component entirely | LGPL-3 | Borrowed reasoning |

**Deliberately avoided**: ERPNext's "immutable" ledger whose rows repost rewrites; Odoo's duplicate quants merged after the fact; Odoo's locking of a serialisation token instead of the row being changed (here the conditional `UPDATE` locks the row it changes); "serial numbers have quantity 1" enforced only in code (here a `CHECK`).

**Not borrowed**: the TCC trio `Reserve` / `CancelReservation` / `GetReservationStatus` has no counterpart in either system (ERPNext's stock reservation has no TTL, no idempotency key and no timed release; Odoo's reservation methods decide when a reservation starts, never when it lapses). Its correctness rests on this component's own concurrency tests (`TestReserve_并发防超卖`, `TestProperty_Reserve并发同key仅执行一次`).

**Variants seen, no slot family**: cost methods (ERPNext per item, Odoo per category) belong to finance; backdating (ERPNext rewrites, Odoo appends a correcting layer) is decided as "append"; picking strategy (Odoo parameterises FIFO / LIFO / FEFO) has one strategy here. `erp/sales` has a required dependency on this component, so none of these can become a slot; if a customer needs one, it becomes an internal strategy of this component or a customer fork.

## Open questions

| Question | Current answer |
|---|---|
| Should reservations lapse on their own (a TTL)? | No. Neither reference system has it, and it would need a sweeper plus rules for the order whose reservation lapsed. Only the upstream's explicit cancel releases stock |
| Who reconciles balances against the ledger? | Planned: a periodic `SUM(qty)` per (product, warehouse) compared with the balance, alerting and rebuilding from the ledger. Not implemented |
| Archiving movement months older than 12 months and deleting final reservations after 30 days | The policy above; no code detaches partitions or deletes reservations yet |
| Should `Adjust` go through approval? | Not now: it applies directly and records the reason. The shape when needed: this component opens a task in `infra/workflow` and applies the adjustment on its completion event; workflow holds no business rule |
| Serial-tracked products: the `CHECK` only enforces "a row with a serial number has quantity ±1" | It cannot see the tracking type (that is in the summary copy), so "a row that should have a serial number but has none" is only caught in application code, within the event window. Accepted gap |
| Warehouse management | Seeded by migration; a management API when a customer needs to create warehouses often |
| Per-caller data scope over gRPC | gRPC calls carry no verified claims, so the scoped methods (`Receive`, `Adjust`, `GetBalance`, `ListMovements`) fail closed over gRPC (`Internal`). Passing the user's scope through gRPC is a platform-level change (the SDK would verify the forwarded token on the server side) |
| A per-product or per-warehouse low-stock threshold | One configuration value for now; a table when a customer asks |
