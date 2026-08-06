set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

# Build the jig binary. Node is never required.
build:
    mkdir -p bin
    go build -o bin/jig ./cmd/jig

# Run all Go tests.
test:
    go test -timeout 5m ./...

# Run the claim/heartbeat/sweep interleaving suite under the race detector
# (U2 verification: every controlplane test, -race on).
test-race:
    go test -race -timeout 10m ./internal/controlplane/...

# Run Go static analysis.
vet:
    go vet ./...

# Report Go files that need formatting.
format-check:
    @test -z "$(gofmt -l cmd internal migrations)"

# Prove the worker never imports control-plane implementation code (KTD1).
boundary:
    @! go list -deps ./internal/worker | grep -qx 'github.com/StructuPath/jig/internal/controlplane'

# Run the local and CI checks.
check: format-check vet boundary test build
