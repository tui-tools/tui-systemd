# tui-systemd — build, test and lint.

GO      ?= go
BIN     ?= bin
TOOL    := tui-systemd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# The screenshot renderer is shared by the whole family and ships with the
# kit, which is already a dependency: ask the module cache where it landed.
KIT     = $(shell $(GO) list -m -f '{{.Dir}}' github.com/tui-tools/tui-kit)

.PHONY: all build test vet fmt fmt-check lint check demo clean tidy install screenshots

all: check build

## build: compile the tool into $(BIN) as a static binary.
build:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN)/$(TOOL) ./cmd/$(TOOL)

## test: run the unit tests.
test:
	$(GO) test ./...

## vet: run the standard static checks.
vet:
	$(GO) vet ./...

## fmt: rewrite the sources with gofmt.
fmt:
	gofmt -w .

## fmt-check: fail when something is not gofmt-clean.
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "these files need gofmt:"; echo "$$out"; exit 1; \
	fi

## lint: fmt-check plus vet. golangci-lint is used when installed.
lint: fmt-check vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, skipping"; \
	fi

## check: everything CI runs.
check: lint test

## demo: run against the in-memory sample machine.
demo:
	$(GO) run ./cmd/$(TOOL) --demo

## install: install the tool into GOBIN.
install:
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/$(TOOL)

## tidy: prune and refresh go.mod / go.sum.
tidy:
	$(GO) mod tidy

## screenshots: re-render the README frames from --demo (needs chrome/chromium).
screenshots: build
	python3 $(KIT)/tools/render-screenshots.py \
		--bin $(BIN)/$(TOOL) --name $(TOOL) --out docs/screenshots \
		--screen main= --screen failed=\\t --screen restart=r \
		--screen journal=j --screen timers=t --screen boot=b --screen help=?

## clean: remove build output.
clean:
	rm -rf $(BIN) dist
