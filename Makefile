# Portcullis build entry points. Run `make help` for a list.
MODULE    := github.com/AaronRaj06/portcullis
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X $(MODULE)/internal.Version=$(VERSION)
BINARIES  := portcullis portcullis-controller
FUZZTIME  ?= 10s
.DEFAULT_GOAL := help

# Pinned tool versions, run via `go run` so nobody has to install them (see docs/adr/0002).
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.10.0

# Project-local CLI tools in bin/tools: nothing global (no brew, no system Go upgrade). See docs/adr/0003.
TOOLS_BIN := $(CURDIR)/bin/tools
KIND      := sigs.k8s.io/kind@v0.33.0
VEGETA    := github.com/tsenart/vegeta/v12@v12.13.0
export PATH := $(TOOLS_BIN):$(PATH)

.PHONY: help all build test lint fmt vet staticcheck golangci vuln fuzz docker tools bench bench-quick clean

tools: ## Install kind and vegeta into bin/tools (project-local)
	GOBIN=$(TOOLS_BIN) go install $(KIND)
	GOBIN=$(TOOLS_BIN) go install $(VEGETA)


help: ## Show targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

all: lint test build ## Lint, test and build

build: ## Build binaries into bin/
	@mkdir -p bin
	@for b in $(BINARIES); do CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; done

test: ## Run all tests with the race detector
	go test -race -count=1 ./...

lint: fmt vet staticcheck golangci ## All static checks

fmt: ## Fail if any file is not gofmt'ed
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

staticcheck:
	go run $(STATICCHECK) ./...

golangci:
	golangci-lint run ./...

vuln: ## Check dependencies for known vulnerabilities
	go run $(GOVULNCHECK) ./...

fuzz: ## Run every Fuzz* target for FUZZTIME each (smoke test)
	@for pkg in $$(go list ./...); do \
	  for f in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
	    echo "fuzz $$pkg $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) $$pkg || exit 1; \
	  done; \
	done

docker: ## Build both container images
	@for b in $(BINARIES); do docker build -f deploy/Dockerfile --build-arg BIN=$$b --build-arg VERSION=$(VERSION) -t $$b:$(VERSION) . || exit 1; done

bench: tools ## Full load-test matrix in Docker (about 30 min); results in bench/results/<time>
	go run ./bench/run $(BENCH_ARGS)

bench-quick: tools ## Small matrix, 1 run of 5s (for optimisation rounds)
	go run ./bench/run -quick -runs 1 -duration 5s -failover=false $(BENCH_ARGS)

clean:
	rm -rf bin
