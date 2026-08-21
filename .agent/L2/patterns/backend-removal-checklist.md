---
layer: L2
type: pattern
tags: [deploy, backend-removal, disallowunknownfields, smoke]
created: 2026-08-11
confidence: high
verified_at: 2026-08-19
applies_to: HEAD (config 按服务 Load 重构后)
---

# 从 lark-bridge 移除一个后端的清单

适用于：删掉某个 `*-back` 业务后端（claude-back 为例；opencode-back/omp-back 为先例）。

## 关键认知
- **真正的危险在 deploy/config，不在 Go。** Go 层后端天然隔离（cmd/<x>-back + internal/<x> + internal/<x>bridge 三块，无兄弟后端 import）。删 Go 包后 `go build ./...` 唯一可能断点就是它自己的 `cmd/<x>-back/main.go`。
- **按服务 Load + 联合 known-key 集**（2026-08-19 config 重构后语义）：三个服务仍解析同一 base 配置（deploy.sh 从 config.example.json 派生各服务 config），但各服务只严格解码自己 owned 的 section，foreign section 跳过。**顶层键的 typo 检测依赖 `allKnownKeys()`——三个服务 struct 的并集（反射收集）**。base 里的 `<x>{}` 子块必须仍被至少一个 struct 的 owned 键覆盖，否则**所有**服务解析失败（unknown top-level key）。→ 删后端时**不要顺手删 XxxBackConfig struct**（或其 section 字段），除非同时重构 base 派生（属更激进的清理）。
- **owned section 内的拼写保护仍在**：`DisallowUnknownFields` 作用于过滤后的文档——本服务 owned section 内的 typo 键仍硬拒绝（例：miniagent-back 拒绝已删除的 `miniagent.mode`）。
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

> ⚠️ 历史注记（2026-08-19）：`internal/bridgebase/` 已整体并入 `internal/miniagent/`（虚共享层收口，见 decoupling-assessment P2）；下文 bridgebase 死叶/保留建议写于该层尚存时，现仅作历史参考。

## 保留 vs 清理的两档
- **留共享层（B）**：config.<X> 结构体、bridgebase 死叶符号、router 字段、文档全留作 dormant/stale。低风险。
- **全量清零（C）**：额外删 config.<X> + bridgebase 死叶 + router 字段 + 重写文档。**必须**同时重构 config.example.json base 派生；router 字段删除可能破坏已部署状态文件。高风险，仅在确认无隐藏引用时做。

## 深一层：删后端可能让整层抽象变死（不只死叶）
B 之后做 C 核验时发现：`bridgebase.Core`（含 NewCore/CoreConfig + ~24 方法 + cancelableWaitGroup）在包外**仅注释出现**——miniagent 明文 "has no bridgebase.Core"，自己用 PromptCancel + 包级 helper 实现同等生命周期。即删掉唯一消费者(claudebridge)后，整层 Core 抽象成死码，而不只是几个死叶符号。教训：
- 核验死码时，**除了查符号级死叶，还要查"类型/构造器"本身是否还有外部 new/嵌入点**（grep `NewXxx(`、`*Pkg.Type` 嵌入、`Pkg.Type{` 在包外）。
- miniagent 这种"用包级 helper 而非嵌入 Core"的写法是项目当前的范式；Core 是 claudebridge/opencode 时代的遗留。
- 摘除整层抽象是"分阶段"的重活：prod 5 文件手术 + 配套 5 测试文件重写/删，须 build-gate 逐文件验证。

## 参考
- 先例：opencode-back/omp-back 移除（CHANGELOG；cleanup_legacy 的现有段）。
- 配置派生流：`deploy/deploy.sh:stage_configs` + `inject_router_path`。
- 相关任务记录：[[claude-backend-removal]]。
