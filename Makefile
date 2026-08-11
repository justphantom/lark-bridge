# lark-bridge build and test entry points.
#
# Targets:
#   build       compile the five binaries into bin/ (version-stamped)
#   build-check go build ./... (catch internal-package compile errors)
#   vet         go vet ./...
#   fmt         gofmt -s -w .
#   lint        golangci-lint run ./... (the 0-issues quality gate)
#   test        build-check + vet + deploy-smoke + go test -race ./...
#   prerelease  test + lint — the pre-tag gate, run before `git tag v1.x.0`
#   deploy-smoke bash helper unit tests (deploy/tests/smoke.sh)
#   deploy      build, then install as systemd services via deploy/deploy.sh
#   deploy-bg   same as deploy, but setsid-detached so an SSH drop / Ctrl-C
#               cannot kill a manual deploy mid-flight (manual path only)
#   pack        build all six binaries and bundle into a distributable tarball
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

.PHONY: build build-services build-feishu-front build-miniagent-back build-agnes-back build-deploy-monitor build-status-monitor build-check test vet fmt lint prerelease clean deploy deploy-bg deploy-monitor deploy-status deploy-agnes pack

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

# Per-binary targets compile a single binary into bin/; deploy scripts call
# only the target(s) they need (deploy.sh → build-services, deploy-monitor.sh →
# build-deploy-monitor), avoiding wasted cross-binary builds.
build-feishu-front:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-feishu-front ./cmd/feishu-front

build-miniagent-back:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-miniagent-back ./cmd/miniagent-back

build-agnes-back:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-agnes-back ./cmd/agnes-back

build-deploy-monitor:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-deploy-monitor ./cmd/deploy-monitor

build-status-monitor:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/lark-status-monitor ./cmd/status-monitor

# build-services compiles only the 2 business services that deploy.sh manages;
# the two monitors are deployed independently by deploy-monitor.sh / deploy-status.sh.
build-services: build-feishu-front build-miniagent-back

# build compiles all five binaries (version-stamped).
build: build-feishu-front build-miniagent-back build-agnes-back build-deploy-monitor build-status-monitor

# pack 交叉编译七个二进制 + VERSION 标记，打成一个可分发的 tarball。
# 在临时 staging 目录构建，避免 bin/ 里已有的旧 tarball/二进制被卷进新包。
# 输出 bin/lark-bridge-<version>-<goos>-<goarch>.tar.gz，解包后顶层即各二进制。
pack:
	@tmp=$$(mktemp -d) && trap "rm -rf $$tmp" EXIT; \
	mkdir -p bin; \
	for name in lark-feishu-front:cmd/feishu-front lark-miniagent-back:cmd/miniagent-back lark-deploy-monitor:cmd/deploy-monitor lark-status-monitor:cmd/status-monitor lark-agnes-back:cmd/agnes-back; do \
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

# lint runs golangci-lint over the whole module. The repo's quality baseline is
# 0 issues; this target makes it enforceable from one command and is the gate
# prerelease runs before tagging. Requires golangci-lint on PATH.
lint:
	golangci-lint run ./...

# test runs build-check + vet as gates, then the full suite under the race
# detector. -race needs CGO_ENABLED=1, which is the default on Linux.
test: build-check vet deploy-smoke
	go test -race ./...

# prerelease is the pre-tag gate: the full test suite (build-check + vet +
# deploy-smoke + go test -race) AND a clean golangci-lint pass. Run this before
# `git tag v1.x.0` so a tag never cuts a build with lint issues or a failing test.
prerelease: test lint

# deploy-smoke unit-tests the bash deploy helpers (lib-common.sh + deploy.sh's
# source guard) without systemd/sudo; catches mapping-table and escaping
# regressions that `bash -n` cannot.
deploy-smoke:
	./deploy/tests/smoke.sh

clean:
	rm -rf bin/

# deploy hands off to the systemd deploy script, which runs `make build`
# internally. deploy.sh is also runnable standalone (./deploy/deploy.sh).
# Note: deploy.sh manages the 2 business services (feishu / miniagent).
# lark-deploy-monitor is managed independently
# by deploy-monitor.sh (it triggers deploy, so self-managing would be a
# circular dependency).
deploy:
	./deploy/deploy.sh $(ARGS)

# deploy-bg runs the SAME deploy as `deploy`, but detached via setsid so an SSH
# disconnect (SIGHUP) or a stray Ctrl-C (SIGINT) in the now-dead terminal cannot
# abort a deploy mid-way — critically between stop_services and start_services
# (deploy.sh:629→:922), where services are already stopped and the EXIT trap
# (deploy.sh:427) only cleans temp files, it does NOT restart them.
#
# setsid (NOT nohup): a new session with no controlling tty removes the deploy
# from the SSH session's foreground process group, defeating BOTH the disconnect
# SIGHUP AND a Ctrl-C from the original terminal; nohup only ignores SIGHUP,
# leaving SIGINT live. Mirrors the chat path's own Setpgid grouping
# (internal/cmdutil/spawn_group.go).
#
# MANUAL-ONLY. The chat path (/deploy -> lark-deploy-monitor) execs `make deploy`
# SYNCHRONOUSLY and captures stdout/stderr into a 1 MiB buffer to render the
# Feishu progress card (spawn_group.go RunCombinedBounded); it is already
# daemonized (GoSafe + context.Background + Setpgid). Do NOT route the chat path
# here: a self-detach makes the call return at once with empty capture, clearing
# the single-flight slot early (a racing /deploy could double-deploy).
deploy-bg:
	@log="deploy-$$(date +%Y%m%d-%H%M%S).log"; \
	echo "[deploy-bg] detached via setsid; log: $$log"; \
	echo "[deploy-bg] watch: tail -f $$log   |   stop: pkill -f deploy.sh"; \
	setsid ./deploy/deploy.sh $(ARGS) >"$$log" 2>&1 </dev/null &

# deploy-monitor builds and restarts ONLY lark-deploy-monitor, decoupled
# from deploy.sh. Use --init for first-time install (creates config + unit).
# In pro mode (LARK_RUN_MODE=pro) this target is a no-op: deploy-monitor is
# intentionally not deployed.
deploy-monitor:
	./deploy/deploy-monitor.sh $(ARGS)

# deploy-status builds and restarts ONLY lark-status-monitor (the periodic
# overview-card pusher), decoupled from deploy.sh for the same reason monitor
# is. Use --init for first-time install (creates config + unit).
deploy-status:
	./deploy/deploy-status.sh $(ARGS)

# deploy-agnes builds and restarts ONLY lark-agnes-back (the Agnes AI image/
# video generation backend), decoupled from deploy.sh. Use --init for
# first-time install (creates config + unit).
deploy-agnes:
	./deploy/deploy-agnes.sh $(ARGS)
