.SUFFIXES:

SRCS    := $(shell find . -maxdepth 2 -name '*.go')
BIN     := ./bin/change-go-version
PY_SRCS := main.py main_test.py
UV_RUN  := uv run --quiet

help:  ## Show this help
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: $(BIN)  ## Build binary into ./bin

$(BIN): $(SRCS) VERSION go.mod go.sum makefile
	mkdir -p ./bin
	go build -o $(BIN) .

.PHONY: test go-test py-test
test: go-test py-test  ## Run all tests (Go + Python)

go-test:  ## Run Go test suite with race detector
	@if command -v gotest >/dev/null 2>&1; then \
		gotest -race ./...; \
	else \
		go test -race ./...; \
	fi

py-test:  ## Run Python test suite with pytest (via uv)
	uvx pytest $(PY_SRCS)

.PHONY: lint go-lint py-lint
lint: go-lint py-lint  ## Lint all (Go + Python)

go-lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then \
		echo "Unformatted files:"; echo "$$out"; exit 1; \
	fi

py-lint:  ## ruff check + format check (no writes)
	uvx ruff check $(PY_SRCS)
	uvx ruff format --check $(PY_SRCS)

.PHONY: format go-format py-format
format: go-format py-format  ## Format all (Go + Python) in place

go-format:  ## gofmt the tree in place
	gofmt -s -w .

py-format:  ## ruff format + autofix in place
	uvx ruff format $(PY_SRCS)
	uvx ruff check --fix $(PY_SRCS)

.PHONY: spellcheck
spellcheck:  ## Spellcheck sources and docs with cspell (via npx)
	npx --yes cspell --no-progress --gitignore "**/*.go" "**/*.py" "**/*.md" "Makefile"

.PHONY: verify
verify: lint test spellcheck  ## Pre-PR gate: lint, test, spellcheck
	@echo "All checks passed."

.PHONY: clean
clean:  ## Remove build artifacts and generated files
	rm -f $(BIN)
	rm -rf __pycache__ .pytest_cache .ruff_cache
	rm -f change-go-version