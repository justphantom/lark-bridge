# lark-bridge build and test entry points.
#
# Targets:
#   build       compile the five binaries into bin/ (version-stamped)
#   build-check go build ./... (catch internal-package compile errors)
#   vet         go vet ./...
#   fmt         gofmt -s -w .
#   test        build-check + vet + go test -race ./...
#   deploy      build, then install as systemd services via deploy/deploy.sh
#   pack        build all five binaries and bundle into a distributable tarball
#               (bin/lark-bridge-<ver>-<goos>-<goarch>.tar.gz); cross-compile via
#               GOOS=/GOARCH= on the command line
#   clean       rm -rf bin/
#
# Deploy:
#   make deploy             # use existing repo-root config.json + .env
#   make deploy ARGS=--init # first-time: generate config.json + .env from examples
#
# Deploy optional env vars:
#   IPC_ADDR   IPC listen address (default localhost:6060)
#   STATE_DIR  persistence dir (default /var/lib/lark-bridge)

.PHONY: build build-check test vet fmt clean deploy upgrade-monitor pack

# Default to `build` so a bare `make` produces the five binaries.
.DEFAULT_GOAL := build

# VERSION is the short commit hash (dirty-suffixed when the worktree has
# uncommitted changes). := evaluates once at Make startup; with = it would
# re-run git describe on every reference (3x per build).
VERSION := $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)

# pack 的目标平台；命令行覆盖：make pack GOOS=linux GOARCH=arm64
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

# build-check compiles every package (not just the five cmds) so a syntax/type
# error in an internal package fails fast instead of surfacing only under test.
build-check:
	go build ./...

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-feishu-front ./cmd/feishu-front
	go build -ldflags "$(LDFLAGS)" -o bin/lark-claude-back ./cmd/claude-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-opencode-back ./cmd/opencode-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-miniagent-back ./cmd/miniagent-back
	go build -ldflags "$(LDFLAGS)" -o bin/lark-deploy-monitor ./cmd/deploy-monitor

# pack 交叉编译五个二进制 + VERSION 标记，打成一个可分发的 tarball。
# 在临时 staging 目录构建，避免 bin/ 里已有的旧 tarball/二进制被卷进新包。
# 输出 bin/lark-bridge-<version>-<goos>-<goarch>.tar.gz，解包后顶层即各二进制。
pack:
	@tmp=$$(mktemp -d) && trap "rm -rf $$tmp" EXIT; \
	mkdir -p bin; \
	for name in lark-feishu-front:cmd/feishu-front lark-claude-back:cmd/claude-back lark-opencode-back:cmd/opencode-back lark-miniagent-back:cmd/miniagent-back lark-deploy-monitor:cmd/deploy-monitor; do \
		out=$${name%%:*}; src=./$${name##*:}; \
		echo "build  $$out ($(GOOS)/$(GOARCH))"; \
		GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o $$tmp/$$out $$src; \
	done; \
	printf '%s\n' '$(VERSION)' > $$tmp/VERSION; \
	cp config.example.json $$tmp/ 2>/dev/null || true; \
	cp deploy/env.example $$tmp/env.example 2>/dev/null || true; \
	out=bin/lark-bridge-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz; \
	tar -C $$tmp -czf $$out .; \
	echo "packed $$out"

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
# Note: deploy.sh only manages the 4 business services. lark-deploy-monitor
# is managed independently by upgrade-monitor.sh (it triggers deploy, so
# self-managing would be a circular dependency).
deploy:
	./deploy/deploy.sh $(ARGS)

# upgrade-monitor builds and restarts ONLY lark-deploy-monitor, decoupled
# from deploy.sh. Use --init for first-time install (creates config + unit).
upgrade-monitor:
	./deploy/upgrade-monitor.sh $(ARGS)
