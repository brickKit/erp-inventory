# erp/inventory

## Purpose

The one answer to "how much of this product is in this warehouse right now, and may I take some": stock balances, the stock movement ledger, reservations with oversell protection, and the warehouses they live in. It is the only writer of physical stock movements; sales and other components ask it to reserve, release and issue stock and never change stock themselves.

**Owns**

- Warehouses (seeded by a migration: `WH-EAST`, `WH-SOUTH`; no management API) and who may access which warehouse (`warehouse_access`).
- Balances per (product, warehouse): on hand, reserved, available = on hand − reserved, all decimal strings. A reservation can never exceed what is on hand: the check and the lock are one conditional `UPDATE`.
- The movement ledger: receipts, issues, gains and losses from stock-taking. Append-only; a correction is a new movement, never an edit.
- Reservations: `Reserve` → `ConfirmIssue` or `CancelReservation`, with `GetReservationStatus` to find out after a timeout whether a request took effect.
- A summary copy of each product's tracking type (none / batch / serial), kept from `mdm/product` events.
- The event `erp.inventory.adjusted.v1`; the subject `erp.inventory.transferred.v1` is reserved for transfers, not published yet.

**Does not own**

- What a product is (SKU, name, unit, tracking type): `mdm/product`. A `product_id` here is an opaque ID; checking that it exists is the caller's job.
- Why stock moves (orders, work orders, purchase receipts): `erp/sales` and other ERP components. This component receives "reserve 3", not the order.
- The value of stock and its accounting: `erp/finance`, which consumes `erp.inventory.adjusted.v1`. Movements carry quantities, never amounts.

## Before you deploy

- **PostgreSQL**: a schema `erp_inventory`, plus `erp_inventory_archive` if your schema convention creates one (this component never writes to it). A login role `erp_inventory_rw` with `USAGE` and `CREATE` on `erp_inventory`, and its password. The migration runs as this role and creates the tables, so the role owns them; the running component creates monthly partitions of `inventory_movements` and weekly partitions of `event_outbox` / `event_inbox` itself, which needs that ownership. The migration also creates the two seeded warehouses. BrickKit creates none of this; in the BrickEnterprise assembly project `make dev-env` writes the password into `.env` and `make db-init` creates the schemas, role and grants.
- **NATS** reachable at `NATS_URL`: the component publishes through an outbox table and a background pump, and consumes `mdm.product.created.v1` / `mdm.product.updated.v1`. It starts without NATS reachable; events wait in the outbox.
- **Authorization** (`infra/authz`) and **identity** (`infra/iam-casdoor`, or any IAM serving a JWKS) reachable at `AUTHZ_BUNDLE_URL` and `IAM_JWKS_URL` for the REST routes to answer anything but errors. They are configuration, not dependencies: the component starts without them.
- **Warehouse access**: a user sees and moves stock only in the warehouses granted to them. Grant them with `POST /erp/inventory/warehouse-access/{sub}` (permission `erp.inventory.manage_access`); a user with no grants sees no warehouse, no balance and no movement.
- Demo data (optional): `make seed` in the component directory, once this component, `infra/iam-casdoor` and `infra/authz` are running. It receives, adjusts, reserves and issues stock through the real APIs across both warehouses, including rows below the low-stock threshold.

## Dependencies

None. This component is called, and calls no one: `erp/sales` (and the mobile BFF) call it over gRPC and REST; it never calls another component, so it can never sit in the middle of a call chain or a cycle. In particular it does not depend on `mdm/product`: product tracking types arrive as events and are kept as a summary copy.

The authorization bundle and the JWKS are fetched from the URLs in the configuration, not through a dependency edge. Without them every protected route fails closed: no or invalid token → `401`; `IAM_JWKS_URL` empty or unreachable → `403`; the bundle never fetched yet → `503`; a valid user without the permission key → `403`. `/healthz` stays `200` throughout: it reports only that this process is alive.

## Configuration

| Variable | Meaning |
|---|---|
| `PG_HOST` | PostgreSQL host. Usually the project's shared value (`$var:PG_HOST`). |
| `PG_PORT` | PostgreSQL port; default `5432`. |
| `PG_DATABASE` | The database holding the `erp_inventory` schema (`$var:PG_DATABASE`). |
| `PG_USER` | The login role, `erp_inventory_rw` as a literal. Inside a shell the shell logs in with its own role and switches to this one per transaction (`SET LOCAL ROLE`), so the role name must be `<PG_SCHEMA>_rw`. |
| `PG_PASSWORD` | Password of `PG_USER`. Secret: write `${ERP_INVENTORY_DB_PASSWORD}` (or your secret store's reference), never the value. |
| `PG_SCHEMA` | Schema of all tables, the outbox and the migration state table (`schema_migrations_erp_inventory`); default `erp_inventory`. Write the literal anyway so every component's schema is visible in one place. |
| `NATS_URL` | NATS server for the outbox pump and the product-event consumer (`$var:NATS_URL`). |
| `OTEL_BASE_URL` | OpenTelemetry collector base URL; empty (the default) exports nothing. |
| `AUTHZ_BUNDLE_URL` | URL of the authorization bundle the permission check polls, for example `http://infra-authz-2-0-0:8223/authz/bundle`. Required: without it every protected route answers `503`. Keep it in step with the authz version the project runs. |
| `IAM_JWKS_URL` | URL of the JWKS used to verify user tokens locally, for example `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`. Required: without it every protected route answers `403`. |
| `LOW_STOCK_THRESHOLD` | A balance row counts as low stock on `GET /stats/summary` when its available quantity is below this number. A non-negative decimal (`10`, `2.5`); default `10`, which an empty value also means. Anything else (a negative number, scientific notation, text) stops the component at start-up instead of silently falling back. Leave it commented out to follow the component's default; the value in effect is logged at start-up. |

## Contracts

- `contracts/erp/inventory/v1/inventory.proto` — gRPC `erp.inventory.v1.InventoryService` for other components: `Reserve`, `CancelReservation`, `ConfirmIssue` (each takes an `idempotency_key`; a reservation holds several (product, warehouse, quantity) items, all or nothing), `GetReservationStatus` (by `reservation_id`, or by the `idempotency_key` of the `Reserve` when that call timed out; `RESERVATION_STATUS_UNSPECIFIED` means not found, safe to retry), `Receive`, `Adjust`, `GetBalance`, `BatchGetBalance` (the batched read callers use instead of a loop of `GetBalance`; missing rows mean zero), `ListMovements` (cursor paging, no offset). The Go package is the separate module `github.com/brickKit/erp-inventory/gen/erp/inventory`. `Receive`, `Adjust`, `GetBalance` and `ListMovements` filter by the caller's warehouse access, which only a verified REST request carries; called over gRPC directly they fail with `Internal`.
- `contracts/inventory.openapi.yaml` — REST under `/erp/inventory`, every route behind a permission key: `POST /movements/receive` (`erp.inventory.receive`), `POST /movements/adjust` (`erp.inventory.adjust`), `GET /balances` (one balance; `erp.inventory.view`), `GET /balances/list` (`erp.inventory.view`; optional `product_id`, `warehouse_id`, cursor paging), `GET /movements` (`erp.inventory.view`; `created_after` / `created_before`, default the last 90 days), `GET /warehouses` (`erp.inventory.view`), `GET /stats/summary` (`erp.inventory.view`; `sku_count`, `total_on_hand_qty`, `low_stock` up to 50 rows, `low_stock_count`), and `GET` / `POST /warehouse-access/{sub}`, `DELETE /warehouse-access/{sub}/{warehouse_id}` (`erp.inventory.manage_access`). Every read and write except the warehouse-access routes counts only the warehouses granted to the caller. The reservation calls and `BatchGetBalance` are not on REST.
- `contracts/events/inventory.events.json` — published through the outbox: `erp.inventory.adjusted.v1` on every receipt, issue and adjustment (product, warehouse, signed quantity, movement ID, batch, serial, reason; `aggregate_id` is the movement ID, `version` is always 1). `erp.inventory.transferred.v1` is reserved and not published. Consumed: `mdm.product.created.v1`, `mdm.product.updated.v1` (only the tracking type, applied when the version is greater than the one held).
- `assembly.yaml` — this project's metadata: the six permission keys (`erp.inventory.reserve` and `.issue` exist for role setup and audit; their calls are gRPC only), the menu entry, the edge route `/erp/inventory/**`, the schema and role, and the data scope `warehouse` (column `warehouse_id`, mode `in`) on balances, movements and reservations.

## Shell declaration

Not a shell. It can be hosted in a Go shell (in the BrickEnterprise project, `be/go-core`) or run on its own; the code is the same either way.
