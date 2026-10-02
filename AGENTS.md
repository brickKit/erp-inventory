[English](AGENTS.md) · [中文](AGENTS.zh.md)

# erp/inventory

The AI guide to developing this component. How to use it, its boundaries and contracts: BRICKKIT.md. Why it is shaped this way: `docs/design.md`. Dependencies, configuration and deployment: component.yaml.

## Code map

| Path | Owns |
|---|---|
| `backend/module/module.go` | The only entry, `New(ctx, rt)`: reads `PG_SCHEMA` and `LOW_STOCK_THRESHOLD`, builds repo → service → HTTP + gRPC, starts four background loops (outbox pump, weekly and monthly partitions, product-event consumer). Same function standalone and in a shell |
| `backend/cmd/server/main.go` | One line, `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | One line, `migrate.Main(migrations.FS)`: the migration container's entry (`./migrate up`) |
| `backend/internal/repo/repo.go` | Sentinel errors, `Repo`, the claim-first idempotency helpers (`claimIdempotency`, `finalizeIdempotency`) |
| `backend/internal/repo/reservation.go` | `Reserve`, `CancelReservation`, `ConfirmIssue`, `GetReservationStatus`: the conditional updates that prevent overselling |
| `backend/internal/repo/movement.go` | `Receive` and `Adjust` through one transaction shape (`applyStockChange`), the `erp.inventory.adjusted.v1` outbox write, `ListMovements` |
| `backend/internal/repo/balance.go` | `GetBalance`, `BatchGetBalance`, `ListBalances` |
| `backend/internal/repo/stats.go` | `StockSummary`: SKU count, total on hand, low-stock rows against the threshold, compared as `numeric` |
| `backend/internal/repo/warehouse.go` | `ListWarehouses` |
| `backend/internal/repo/access.go` | `warehouse_access`: the IDs a caller may see, grant, revoke |
| `backend/internal/repo/cursor.go` | List cursors: `created_at` + `id` for movements, `id` for balances |
| `backend/internal/repo/snapshot.go` | The product tracking-type summary copy, upserted only with a greater version |
| `backend/internal/service/` | Input validation, the caller's allowed warehouses (`allowedWarehouseIDs`), the low-stock threshold (`stats.go`), the error → gRPC status mapping (`status.go`) |
| `backend/internal/http/http.go` | REST routes, each registered with its permission key |
| `backend/internal/grpc/grpc.go` | `erp.inventory.v1.InventoryService` |
| `backend/internal/consumer/consumer.go` | Subscribes to `mdm.product.created.v1` / `.updated.v1` |
| `backend/internal/partition/` | Weekly partitions of `event_outbox` / `event_inbox`, monthly partitions of `inventory_movements` |
| `migrations/` | SQL migrations (warehouses are seeded by `003_seed_warehouses`), embedded by `migrations/embed.go` |
| `contracts/` | proto, OpenAPI, event schema |
| `gen/erp/inventory/` | Generated Go code: a nested Go module, tagged on its own as gen/erp/inventory/v1.x.y; never edited by hand |
| `scripts/seed.sh` | Local demo data through the real APIs |

| Feature | Start here | Then |
|---|---|---|
| A rule about how much may be reserved or issued | `backend/internal/repo/reservation.go` (the `WHERE` of the conditional `UPDATE`) | `backend/internal/repo/repo_test.go` (`TestReserve_并发防超卖`), `backend/internal/repo/property_test.go` |
| A new filter or column on a list | `backend/internal/repo/balance.go` or `backend/internal/repo/movement.go` (the static SQL constant) | `backend/internal/http/http.go`, `contracts/inventory.openapi.yaml`, `backend/internal/service/stats_test.go` |
| A dashboard figure | `backend/internal/repo/stats.go` | `backend/internal/service/stats.go`, `backend/internal/http/http.go` (`statsSummaryHandler`) |
| A new configuration key | `component.yaml` (`configSchema`) | `backend/module/module.go`, `backend/module/config_test.go`, BRICKKIT.md "Configuration" |
| Who may see which warehouse | `backend/internal/repo/access.go` | `backend/internal/service/service.go` (`allowedWarehouseIDs`) |
| An error answering with the wrong status | `backend/internal/service/status.go` | the sentinel errors in `backend/internal/repo/repo.go` |

## Build and test

```bash
# tests run against the test database brickkit_test_db, never brickkit_db (make test-db-init at the project root)
export TEST_PG_DSN="postgres://postgres:<POSTGRES_PASSWORD from the project .env>@localhost:5432/brickkit_test_db?sslmode=disable"
export TEST_NATS_URL=nats://localhost:4222
make test                    # every package ends in "ok"; refuses to run without TEST_PG_DSN
go test ./... -count=1 -v | grep -c -- '--- SKIP'   # 0: no test skipped
make check-version dag-check contract-check import-scan module-check   # each prints one ✓ line
make docs-check              # "0 with errors, 0 warnings"
# migrations, as the login role (at the project root, make test-db-init ID=erp/inventory does this for you):
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=erp_inventory_rw \
  PG_PASSWORD=<ERP_INVENTORY_DB_PASSWORD> PG_SCHEMA=erp_inventory make migrate-idempotent   # ✓ 迁移幂等
```

`TestReserve_并发防超卖` (100 goroutines on 30 units: exactly 30 succeed) is the only test that proves the conditional update is atomic; rerun it after touching anything in `reservation.go`. After `buf generate` (contracts changed), the contract package needs a new tag: see Pitfalls. On a real machine, from the project root: `make verify ID=erp/inventory ROUTE=/erp/inventory/warehouses FOCUS=1` builds the image, starts only what this component needs, checks migration, health and the permission check, runs a focus run, and tears down.

## Design decisions

- **Called by others, calls no one.** No dependencies; product tracking types arrive by event. `make dag-check` fails on any dependency.
- **Oversell protection is one statement**: `UPDATE … WHERE on_hand_qty - reserved_qty >= $qty`; `RowsAffected() == 0` is "insufficient stock". Two `CHECK` constraints on `inventory_balances` are the database's backstop.
- **Writes claim their idempotency key first** (`INSERT … ON CONFLICT DO NOTHING` on `command_idempotency`) and only then do the work; `GetReservationStatus` by key only accepts keys of `Reserve`.
- **The ledger is append-only**; balances are its projection, updated in the same transaction.
- **Warehouse scope comes from this component's own table**, not from the token: the service looks up the caller's `sub` in `warehouse_access` and pushes the list into every query's `WHERE`.
- **Lists are static SQL** with "empty parameter means no filter"; balances page by `id` with no time window, movements by `created_at` + `id` with the default 90-day window.
- **The low-stock threshold is a decimal string compared in SQL**, read once at start-up; an invalid value stops the component.

## Pitfalls

| Never | Symptom | Why |
|---|---|---|
| Check stock with a `SELECT` and then `UPDATE` | Unit tests stay green, a load test fails now and then, production oversells a few orders a day | There is a window between the two statements; the condition must be in the `UPDATE`'s `WHERE` |
| Add an entry to `dependencies.components`, especially `mdm/product` | Nothing breaks; the hub stops being a leaf of the call graph and the next edge can close a cycle | Callers validate products before reserving; tracking types come by event |
| Filter by allowed warehouses in Go after the query | Pages come back short or empty while `next_cursor` is set | The allowed list must be part of the SQL `WHERE` |
| Treat an empty allowed list as "no filter" | A user with no grants sees every warehouse | `= ANY('{}')` matches nothing; that is the intended fail-closed result |
| Name a new `inventory_movements` partition in a migration differently from `ensurePartition` (`<table>_YYYY_MM_01`) | Migrations pass; the maintenance loop later tries to create an overlapping partition and fails | `to_regclass` looks the partition up by that exact name |
| Merge `NOT_FOUND` and `CANCELLED` in `GetReservationStatus` | An upstream retry after a timeout either double-reserves or wrongly gives up | `NOT_FOUND` means the request never landed (safe to retry); `CANCELLED` means it landed and was undone |
| Partition or archive `inventory_balances` | Migrations pass; the oversell `UPDATE` has to search partitions on the busiest table | Its size is bounded by products × warehouses |
| Change a comment or option in `contracts/erp/inventory/v1/inventory.proto` without tagging the contract package | Builds pass locally (the `replace` hides it); a shell fetching `erp-inventory/v2` compiles against the old `gen/` | Any change under `gen/erp/inventory/` needs a new tag `gen/erp/inventory/v1.x.y` and the root `go.mod` requiring it |

## Before changing code

1. Is this about how much stock there is (here), or about why it moves (the order, the purchase)? The second never goes here.
2. Does a new condition on stock live in the `WHERE` of the `UPDATE` that changes it?
3. Does every new read or write go through `allowedWarehouseIDs` and push the list into SQL?
4. Contract change? Append only: a new field, rpc, path or query parameter. Never remove or retype a field, rpc, path or event subject. Did `gen/` change? Then the contract package needs a new tag.
5. A new REST route is registered with `besdk.GET` / `POST` / `DELETE` and a permission key from `assembly.yaml`.
6. New rule → write the failing test first against the real test database (two users, two warehouses for anything scoped), then the code.
7. Bump `metadata.version` before the first change after a release; update BRICKKIT.md and `docs/design.md` in the same commit as the code.

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
