.PHONY: test test-integration lint seed

test:  ## unit tests: the manifest, the OpenAPI document and the routes, the event payloads, merge safety
	go vet ./... && go test ./...

test-integration:  ## L2 against PostgreSQL and the bus (BE_SDK_TEST_PG_ADMIN, BE_SDK_TEST_NATS_URL)
	go test -count=1 -tags integration ./inventory/

lint:
	brickkit lint

seed:  ## the seed data for people, through the running component's user surface (SEED_PASSWORD; see cmd/seed)
	$(MAKE) -C ../../mdm/product seed   # the declared dependencies first; the rest come through them (seed and test data conventions)
	go run ./cmd/seed
