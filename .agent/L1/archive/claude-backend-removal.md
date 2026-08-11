---
layer: L1
type: task
status: done
created: 2026-08-11
updated: 2026-08-11T10:37:34+08:00
---

# 清理 Claude 对接（项目聚焦 miniagent）

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
- `deploy-{monitor,status,agnes}.sh` 的 strip 循环：剥的是 `claude{}` JSON 键（保留中），正确无害。
- CHANGELOG：留作历史。

## 待续（如需进一步清理，即当时的方案 C）
- 删 config.Claude + claude{} 块（须同时重构 config.example.json base 派生）。
- 剪 bridgebase 死叶符号。
- 删 router.Binding 的 claude 字段（注意已部署 router-claude.v5.json 兼容）。
- 重写 ARCHITECTURE/README；归档 CLAUDE_INTEGRATION_SPEC。

## 参考
- 选型测绘：本会话 workflow `map-claude-cleanup`（journal 在 subagents/workflows/）。
- 沉淀：[[backend-removal-checklist]]。
