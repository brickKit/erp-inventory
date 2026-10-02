[English](README.md) · [中文](README.zh.md)

# erp/inventory

Stock balances, movements and reservations, with oversell protection; the only writer of physical stock movements.

## Use it in a project

```bash
brickkit add erp/inventory@2.0.0
brickkit up
```

Prepare first: see "Before you deploy" in BRICKKIT.md (a PostgreSQL schema and login role, NATS, the authorization and JWKS URLs, and warehouse access for your users).

## Documentation

| To find out | Read |
|---|---|
| What it does and does not do, how to configure it, what to prepare | [BRICKKIT.md](BRICKKIT.md) |
| Its gRPC, REST and event contracts | [contracts/](contracts/) |
| Why it is designed this way: boundary, oversell protection, reservations, warehouse scope, open questions | [docs/design.md](docs/design.md) |
| What it depends on (nothing) and its configuration keys | [component.yaml](component.yaml) |
| How to develop it: code map, tests, pitfalls | [AGENTS.md](AGENTS.md) |

## Development

Go 1.25, Gin, `database/sql` with pgx, golang-migrate through be-sdk-go. Tests need a real PostgreSQL (`TEST_PG_DSN`); the commands and what success looks like are in AGENTS.md, section "Build and test". Inside the BrickEnterprise project, `make verify ID=erp/inventory FOCUS=1` at the project root runs it for real, in a container and as a local process, and tears down afterwards.
