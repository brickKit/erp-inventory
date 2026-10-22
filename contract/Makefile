# The inventory contract, a module of its own inside the component's repository (tags contract/v3.…).
# buf runs in a throwaway container (nothing installed locally); its remote plugins are pinned in buf.gen.yaml.
# `make check` is what a change to this repository must pass.

BUF := bufbuild/buf:1.74.0@sha256:621f108266b19236906aa328e64b7c50340738472c5da4d4b25104f01aa8d204
RUN := docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e BUF_CACHE_DIR=/tmp/buf -v $(CURDIR):/w -w /w

.PHONY: check lint gen gen-check build breaking

check: lint gen-check build

lint:  ## the protobuf style (buf STANDARD)
	$(RUN) $(BUF) lint

gen:  ## regenerate gen/go and python/be (committed)
	rm -rf gen/go python/be
	$(RUN) $(BUF) generate

gen-check:  ## the committed code is exactly what buf generates from proto/
	@T=$$(mktemp -d) && cp -r gen python $$T/ && $(MAKE) -s gen >/dev/null && \
	  diff -r $$T/gen gen && diff -r $$T/python python && rm -rf $$T && echo "generated code up to date"

build:  ## the Go module builds and vets
	go vet ./... && go build ./... && test -z "$$(gofmt -l .)"

breaking:  ## additive only against a release: make breaking BASE=contract/v3.0.0-alpha.1
	docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e BUF_CACHE_DIR=/tmp/buf -v $(CURDIR)/..:/w -w /w/contract \
	  $(BUF) breaking --against '../.git#tag=$(BASE),subdir=contract'
