.PHONY: fmt vet lint build test test-integration check

fmt:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "The following files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

vet:
	go vet ./...

lint: fmt vet

build:
	go build ./...

test:
	go test ./...

test-integration:
	UNRING_REQUIRE_POSTGRES=1 go test -count=1 ./...

check: fmt vet build test
