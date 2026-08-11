---
layer: L2
type: pattern
tags: [deploy, removal, backend, disallowunknownfields, ghost-unit, smoke]
created: 2026-08-11
confidence: high
---

# 从 lark-bridge 移除一个后端的清单

适用于：删掉某个 `*-back` 业务后端（claude-back 为例；opencode-back/omp-back 为先例）。

## 关键认知
- **真正的危险在 deploy/config，不在 Go。** Go 层后端天然隔离（cmd/<x>-back + internal/<x> + internal/<x>bridge 三块，无兄弟后端 import）。删 Go 包后 `go build ./...` 唯一可能断点就是它自己的 `cmd/<x>-back/main.go`。
- **共享 `Config` + `DisallowUnknownFields`**：三个后端解析同一 base 配置（deploy.sh 的 `stage_configs` 从 config.example.json 派生 feishu/miniagent/<x> config）。base 里的 `<x>{}` 子块必须被 `config.<X>` 结构体识别，否则**所有**后端配置解析失败。→ 删后端时**不要顺手删 config.<X> 结构体**，除非同时重构 base 派生（属更激进的清理）。
- **staging 文件名是 base 模板的内部名**（曾叫 `claude-config.json`），不是随服务发布的文件；改名安全（只影响 STAGE 临时目录），但配置**内容**（claude{} 块）须保留直到结构体也删。

## 删除清单（按层）
1. **Go 包**：`cmd/<x>-back/`、`internal/<x>/`、`internal/<x>bridge/`；连带孤儿 `internal/clibase`、`internal/backendhost`（删前 grep 确认仅 claude 引用）。
2. **Makefile**：`.PHONY`、`build-<x>-back` 目标、`build-services`/`build`/`pack` 列表、注释里的二进制计数。
3. **deploy/lib-common.sh**：`svc_unit`/`svc_config`/`svc_cli` 的 `<x>` case（`svc_depends`/`svc_privileged` 是 feishu-vs-其余泛型，不动）。
4. **deploy/deploy.sh**：`SELECTED` 默认值、错误串、`verify_artifacts`、`filter_cli_ready` 的 case、`install_files` 的 mkdir、staging 改名、注释。
5. **cleanup_legacy 必须新增段**（最重要，否则旧部署 ghost 单元 5s 崩溃循环）：把 `<unit>` 加进 `legacy_unit` 循环；router/usage/CONFIG_DIR 三个 state 循环各加对应文件。**不要** `rm -rf STATE_DIR/<x>/`——可能含用户工作数据，留手动清理并注释说明。
6. **deploy/tests/smoke.sh**：删 `<x>` 断言；**加** `svc_unit <x>` 应失败（仿 opencode/omp）；把 SELECTED/rebuild/drop_service/csv/parse_args 用例切到仍有效的服务。

## 验证门（全过才算完，见 L0 #9）
`go build ./...` → `go vet ./...` → `./deploy/tests/smoke.sh` → `go test -race ./...` → `golangci-lint run ./...`（0 issue）。

## 保留 vs 清理的两档
- **留共享层（B）**：config.<X> 结构体、bridgebase 死叶符号、router 字段、文档全留作 dormant/stale。低风险。
- **全量清零（C）**：额外删 config.<X> + bridgebase 死叶 + router 字段 + 重写文档。**必须**同时重构 config.example.json base 派生；router 字段删除可能破坏已部署状态文件。高风险，仅在确认无隐藏引用时做。

## 参考
- 先例：opencode-back/omp-back 移除（CHANGELOG；cleanup_legacy 的现有段）。
- 配置派生流：`deploy/deploy.sh:stage_configs` + `inject_router_path`。
- 相关任务记录：[[claude-backend-removal]]。
