# Contributing

Thanks for helping make unring's coverage broader and more trustworthy.

The contribution we most want is a declarative adapter for a service unring does
not cover yet. Start with [docs/ADAPTERS.md](docs/ADAPTERS.md); built-in and
community adapters use the same YAML format and the same validation path. Include
tests for classification, staging or approval, idempotency, and any declared undo
boundary.

For Go changes, keep interception failures explicit, do not add an LLM to the
classification path, and preserve the single commit/discard decision. Before
opening a pull request, run:

```sh
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./...
make test-integration
```

Integration tests need PostgreSQL 14 or newer and fail instead of skipping when
run through `make test-integration`.
