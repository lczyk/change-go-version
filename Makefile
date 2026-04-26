.SUFFIXES:

SRCS := $(shell find . -maxdepth 2 -name '*.go')
BIN  := ./bin/change-go-version

help:  ## Show this help
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test:  ## Run the test suite with race detector
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

PY_SRCS := main.py main_test.py
UV_RUN  := uv run --quiet

.PHONY: py-test
py-test:  ## Run python test suite with pytest (via uv)
	$(UV_RUN) --with pytest pytest $(PY_SRCS)

.PHONY: py-lint
py-lint:  ## ruff check + format check (no writes)
	$(UV_RUN) --with ruff ruff check $(PY_SRCS)
	$(UV_RUN) --with ruff ruff format --check $(PY_SRCS)

.PHONY: py-format
py-format:  ## ruff format + autofix in place
	$(UV_RUN) --with ruff ruff format $(PY_SRCS)
	$(UV_RUN) --with ruff ruff check --fix $(PY_SRCS)

.PHONY: clean
clean:  ## Remove build artifacts and generated files
	rm -f $(BIN)
	rm -f cover.out cover.html
	rm -rf __pycache__ .pytest_cache .ruff_cache
