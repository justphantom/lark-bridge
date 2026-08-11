---
layer: L1
type: task
status: done
created: 2026-08-11
updated: 2026-08-11T12:37:01+08:00
---

# 清理 Claude 对接（项目聚焦 miniagent）

> B 已提交（commit `d13898a`）；C 已提交（commit `0580361`）。

## 目标
移除 claude-back 对接，项目聚焦 miniagent。

## 范围决策（用户拍板）
- 方案 **B**：删 claude 后端 + 专属包，保留共享层（bridgebase/router/protocol/config 等）。
- **一并删除** `internal/backendhost`（仅 claude-back 在用的通用 CLIRunner，opencode/omp 二进制本仓不存在）。
- 增强项 **(a)**：staging 文件名 `claude-config.json` → `base-config.json`（纯改名）。
- 增强 (b) `STATE_DIR/claude/` 清理：**未授权，不做**（可能含用户数据，非破坏）。

## 已完成
- 删 5 包：`cmd/claude-back`、`internal/claude`、`internal/claudebridge`、`internal/clibase`、`internal/backendhost`（零外溢，import 闭环全在 claude 自身）。
- Makefile：去 build-claude-back 目标/.PHONY/build-services/build/pack；注释 six→five。
- deploy/lib-common.sh：svc_unit/svc_config/svc_cli 去 claude case。
- deploy/deploy.sh：选择层(SELECTED/enum/verify_artifacts/filter_cli/install_files)去 claude；cleanup_legacy 新增 claude 段（lark-claude-back 单元 + claude-router.json/usage-claude.json/claude-config.json）；staging 改名 base-config.json；注释清理。
- deploy/tests/smoke.sh：删 claude 断言 + 新增 svc_unit claude 失败；csv/parse/drop 改 miniagent。
- 验证全过：build / vet / deploy-smoke(34-0) / go test -race / lint(0 issue)。diffstat 57 文件 +83/−8306。

## 有意保留（B 档残留，非 bug）
- `config.Claude` 结构体 + `config.example.json` 的 `claude{}` 块 + `router.Binding` 的 `// claude` 字段：三后端共享 `Config` + `DisallowUnknownFields`（deploy.sh:513-517），删字段会让 feishu/miniagent 配置解析全崩。留作 dormant。
- `internal/bridgebase` 的 claude-only 死叶符号（StripThinking/MakeEnumPicker/ValidateAbsDir/AskPermission 等）：无 claude import，编译通过，成死码。
- `deploy.sh:475` router_path 值 `claude-router.json`：状态路径（非 staging 名），改它属行为变更、未在 (a) 范围。
- 文档 `CLAUDE_INTEGRATION_SPEC.md`/`ARCHITECTURE.md`/`README`：B 档保留，已 stale。
- `deploy-status.sh` 的 strip 循环：剥的是 `claude{}` JSON 键（保留中），正确无害。
- CHANGELOG：留作历史。

## 方案 C（已提交 — commit `0580361`，50 文件 +499/−3674）
用户拍板执行 C 的全部 4 阶段 + BackendType 字符串延后单独评估。
- **Phase 1a 安全死叶**：删 bridgebase 8 整死文件(prompt/enum_picker/dir_validate/periodic_reporter/singleflight_job/prompt_scaffold/prompt_slot/git_job)+各_test、eventmetrics 4 死计数器、logger 2 死常量；删 CLAUDE_INTEGRATION_SPEC.md + NOTICES.txt。
- **Phase 1b Core 摘除**（核验后发现的核心放大项）：bridgebase.Core/NewCore/CoreConfig 在包外仅注释出现（miniagent 明文 "has no bridgebase.Core"，自用 PromptCancel + 包级 helper）→ 整删 Core 类型 + ~24 方法 + cancelableWaitGroup；core.go 缩成仅 PromptCancel；interactive/prompt_result/commands_send/running 保留包级活函数、剔 Core 方法 + 相关测试重写。
- **Phase 2 config 原子组**：删 config.Claude 结构体+字段+defaults+validate+config_test claude 用例；config.example.json 去 claude{} 块 + backend_id 改 backend-1；deploy-status.sh 的 migrate_config removed_blocks 加 "claude"（迁移已部署 /etc 配置，防 DisallowUnknownFields 解析崩）。
- **Phase 3 router**：删 Binding.PermissionMode/EffortLevel/SettingsFile + Set* 访问器（router 裸 json.Unmarshal，旧状态文件未知键静默丢弃，升级兼容）。
- **Phase 4 文档**：README/RELEASING 定向修；ARCHITECTURE/deploy-README/CODING_STANDARDS 用 workflow 全量重写为 miniagent（file:line 逐项对抗式核验，残留 claude 全为合法历史注）。

## 仍保留（C 后刻意，非 bug）
- protocol.PromptPayload.{PermissionMode,EffortLevel,SettingsFile}：wire-format，feishufront 在用，未动。
- 运行期 "claude" BackendType 字符串（feishufront registry 等惰性元数据/测试夹具）：**用户决定 C 之后单独评估**。
- cleanup_legacy 不删 STATE_DIR/claude/（可能含用户数据）。

## 参考
- 选型测绘：workflow `map-claude-cleanup`；C 核验：workflow `eval-claude-option-c`；文档重写：workflow `rewrite-docs-to-miniagent`（journal 在 subagents/workflows/）。
- 沉淀：[[backend-removal-checklist]]。
