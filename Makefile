# lark-bridge build and test entry points.
#
# Targets:
#   build       compile the five binaries into bin/ (version-stamped)
#   build-check go build ./... (catch internal-package compile errors)
#   vet         go vet ./...
#   fmt         gofmt -s -w .
#   test        build-check + vet + go test -race ./...
#   deploy      build, then install as systemd services via deploy/deploy.sh
#   clean       rm -rf bin/
#
# Deploy:
#   make deploy             # use existing repo-root config.json + .env
#   make deploy ARGS=--init # first-time: generate config.json + .env from examples
#
# Deploy optional env vars:
#   IPC_ADDR   IPC listen address (default localhost:6060)
#   STATE_DIR  persistence dir (default /var/lib/lark-bridge)

.PHONY: build build-check test vet fmt clean deploy

# Default to `build` so a bare `make` produces the five binaries.
.DEFAULT_GOAL := build

# VERSION is the short commit hash (dirty-suffixed when the worktree has
# uncommitted changes). := evaluates once at Make startup; with = it would
# re-run git describe on every reference (3x per build).
VERSION := $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)

# build-check compiles every package (not just the three cmds) so a syntax/type
# error in an internal package fails fast instead of surfacing only under test.
build-check:
	go build ./...

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-feishu-front ./cmd/feishu-front
	go build -ldflags "$(LDFLAGS)" -o bin/lark-claude-back ./cmd/claude-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-opencode-back ./cmd/opencode-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-peri-back ./cmd/peri-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-deploy-monitor ./cmd/deploy-monitor

vet:
	go vet ./...

# fmt applies gofmt with -s (simplify) to every .go file under the repo.
fmt:
	gofmt -s -w .

# test runs build-check + vet as gates, then the full suite under the race
# detector. -race needs CGO_ENABLED=1, which is the default on Linux.
test: build-check vet
	go test -race ./...

clean:
	rm -rf bin/

# deploy hands off to the systemd deploy script, which runs `make build`
# internally. deploy.sh is also runnable standalone (./deploy/deploy.sh).
deploy:
	./deploy/deploy.sh $(ARGS)
