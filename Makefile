# Common tasks. Every target is a plain go command underneath, so on systems
# without make you can run the commands directly (see CONTRIBUTING.md).

GO      ?= go
GOLANGCI_LINT ?= golangci-lint
EVAL    := -corpus testdata/corpus -cases testdata/eval.jsonl -web testdata/web.json \
           -deny-domains evil.example.com -allow-domains iso.org

.PHONY: help build test race cover lint fmt fmt-check vet eval fuzz vuln snapshot check clean

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## Build ./ragloop
	$(GO) build -trimpath -o ragloop ./cmd/ragloop

test: ## Run tests
	$(GO) test ./...

race: ## Run tests with the race detector (needs cgo)
	$(GO) test -race ./...

cover: ## Run tests and write coverage.out
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fmt: ## Format code
	gofmt -w .

fmt-check: ## Fail if any file needs formatting
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint (install: https://golangci-lint.run/welcome/install/)
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo "$(GOLANGCI_LINT) not found; see https://golangci-lint.run/welcome/install/"; exit 1; }
	$(GOLANGCI_LINT) run ./...

eval: ## Run the evaluation gate
	$(GO) run ./cmd/ragloop eval $(EVAL)

fuzz: ## Short fuzz run of every target
	$(GO) test ./internal/rag -run '^$$' -fuzz '^FuzzScreen$$' -fuzztime 50000x
	$(GO) test ./internal/rag -run '^$$' -fuzz '^FuzzParseAndChunk$$' -fuzztime 5000x
	$(GO) test ./internal/rag -run '^$$' -fuzz '^FuzzCheckGrounded$$' -fuzztime 30000x

vuln: ## govulncheck (needs network; builds with Go 1.26+)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

snapshot: ## Local release build into dist/ (needs goreleaser)
	goreleaser release --snapshot --clean

check: fmt-check vet lint test eval ## Everything CI checks except fuzz and vuln

clean: ## Remove build output
	rm -f ragloop ragloop.exe coverage.out
	rm -rf dist
