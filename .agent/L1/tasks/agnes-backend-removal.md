---
layer: L1
type: task
status: done
created: 2026-08-12
updated: 2026-08-12T00:13:00+08:00
---

# 彻底移除 agnes-back（C 档·全量清零）

## 目标
移除 agnes-back（Agnes AI 图片/视频生成后端），项目后端收敛到 miniagent + status-monitor。继 claude-back（B+C）之后第二个移除的后端。

## 用户拍板
- **彻底移除**（非保留/非内部清理）：接受永久丢失图片/视频生成能力（agnes 是唯一媒体后端，无法迁移）。
- **C 档·全量清零**（与 claude 一致）。

## 前置评估
`.claude/wf-agnes-removal-eval.js`（76 agent 对抗式核验，65 confirmed / 6 blocker / 5 high）。结论：Go 层零风险（最干净的后端边界，仅 `cmd/agnes-back/main.go` 引用 `internal/agnesback`，无死共享抽象，异于 claude 的 bridgebase.Core）；功能丢失是产品门（用户已接受）。所有 edit site 已对当前工作树逐行复核。

## 已完成（C 档全量）
- **Go 包**：`git rm -rf cmd/agnes-back internal/agnesback`（8 文件 / 2494 行，整目录）。
- **config 全量清零**：`config.go` 删 `AgnesBack` struct/`Config.AgnesBack` 字段/`ComponentLogLevel.Agnes`；`config_defaults.go` 删 AgnesBack defaults 块；`config_validate.go` 删 `"agnes"` key；`config.example.json` 删 `agnes{}` 块；`config_test.go` 删 2 测试 + `TestLoadConfigExample` 的 `AGNES_*` env + 文档注释。
- **Makefile**：删 `build-agnes-back` 目标/`build:` 列表/`pack` 循环/`deploy-agnes` 目标/`.PHONY`；二进制计数 4→3（5 处注释）。
- **deploy**：`deploy.sh` cleanup_legacy 加 `lark-agnes-back`（legacy_unit）+ `agnes-back-config.json`（legacy_cfg）+ 注释；`deploy-status.sh` `removed_blocks` 加 `"agnes"` + 注释；删 `deploy-agnes.sh`；`env.example` 删 AGNES 块；`smoke.sh` 删 deploy-agnes source-guard（2 check）；`deploy/README.md` 删 §6.7 + §6.8 重编号 + bin 列表；`lib-common.sh` 注释去 deploy-agnes.sh。
- **共享注释**：`registry.go`/`cardkit.go`/`sink.go` 注释去 `agnes` 示例。
- **文档**：README/ARCHITECTURE/CODING_STANDARDS/RELEASING 全量清零（ARCHITECTURE §5.6 整删 + §5.7–5.12 重编号为 5.6–5.11）。
- **.agent**：`new-backend-skeleton.md` 死链 agnes-back → 重指 status-monitor；`diagnostic-logging.md`/`multi-question-card.md` 案例标为历史；保留 agnes 专属 L2 + L1 archive 历史。

## 验证（L0#9 全过）
go build/vet/test -race ./...、golangci-lint 0 issue、deploy-smoke 34/0（删 2 后）、shellcheck -S warning 清零、bash -n 全过。`grep -rni agnes` 残留全为合法（向后兼容迁移 / 历史 / 再对接知识 / 测试夹具标签）。

## 关键坑（agnes 独有）
- `ComponentLogLevel.Agnes`（嵌套 struct 字段，在 `DisallowUnknownFields` 下）—— claude 没有的 footgun：已部署 config 的 `component_log_levels.agnes` 会让**所有**后端解析失败 → 靠 `deploy-status.sh` removed_blocks 加 `"agnes"` 迁移。
- cleanup_legacy 只在也跑 deploy.sh 的主机收敛 agnes ghost；纯 agnes 主机需手动清理。
- agnes 无 router/usage state（deploy-agnes.sh strip 了 router_path；usage 包已删）→ legacy_state 循环不加 agnes。

## 运维收尾（写入交付说明，非代码）
1. 纯 agnes 主机手动清理：`systemctl disable --now lark-agnes-back; rm -f /etc/systemd/system/lark-agnes-back.service /etc/lark-bridge/agnes-back-config.json /opt/lark-bridge/bin/lark-agnes-back`。
2. `AGNES_API_KEY` 吊销：去 Agnes AI 控制台 revoke + 手动剔除已部署 `/etc/lark-bridge/.env` 的 `AGNES_*` 行（`backfill_env` 只增不删）。

## 有意保留（非 bug）
- CHANGELOG 历史、`.agent/L1/archive/*agnes*`、`.agent/L2/patterns/agnes-*.md`（Agnes API 踩坑作再对接资产）、`.agent/index.md` 指针。
- 测试夹具 `"agnes-1"` 字符串标签（与 claude/opencode 移除后保留其标签的先例一致，泛型夹具）。
- `removed_blocks` 的 `"agnes"` + cleanup_legacy 的 `lark-agnes-back`（向后兼容迁移，预期）。

## 参考
- 评估 workflow：`.claude/wf-agnes-removal-eval.js`（journal 在 subagents/workflows/wf_48f2f19c-9cd/）。
- 沉淀：[[backend-removal-checklist]]（agnes 的独立部署 deploy-agnes.sh 例外已在该清单隐含）。
- 先例：[[claude-backend-removal]]。
