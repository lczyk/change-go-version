.SUFFIXES:

SRCS := $(shell find . -maxdepth 2 -name '*.go')
BIN  := ./bin/change-go-version

help:  ## Show this help
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: $(BIN)  ## Build binary into ./bin

$(BIN): $(SRCS) VERSION go.mod go.sum makefile
	mkdir -p ./bin
	go build -o $(BIN) .

.PHONY: test
test:  ## Run test suite with race detector
	@if command -v gotest >/dev/null 2>&1; then \
		gotest -race ./...; \
	else \
		go test -race ./...; \
	fi

.PHONY: lint
lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then \
		echo "Unformatted files:"; echo "$$out"; exit 1; \
	fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w .

.PHONY: spellcheck
spellcheck:  ## Spellcheck sources and docs with cspell (via npx)
	npx --yes cspell --no-progress --gitignore "**/*.go" "**/*.md" "Makefile"

.PHONY: verify
verify: lint test spellcheck  ## Pre-PR gate: lint, test, spellcheck
	@echo "All checks passed."

.PHONY: clean
clean:  ## Remove build artifacts and generated files
	rm -f $(BIN)
	rm -f change-go-version
