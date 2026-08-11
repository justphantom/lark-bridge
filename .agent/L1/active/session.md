---
updated: 2026-08-12T02:30:00+08:00
---

# 会话状态

## 当前任务
**激进清零某已移除后端的全部残留引用** —— 源码早在 `89e29e6` 整目录删除（8 文件 ~2365 行 + config/Makefile/docs 全清零，工作树已无该后端任何源码），本次扫尾抹除非源码残留：CHANGELOG 历史、`.agent` 记忆、`deploy-status.sh` 迁移数组（用户拍板放弃该后端的旧 config 自动迁移能力）。
- 目标：全仓 grep 该后端名（含下划线/连字符/旧脚本名变体）零命中。git commit message 属不可改写历史，不在范围。
- 改动：`deploy/deploy-status.sh`（迁移数组删一项）、`CHANGELOG.md`（纯该后端条目整删、并列提及去 token）、`.agent` 6 文件（genericize/删）。
- ⚠️ 运维后果：`deploy-status.sh` 迁移数组去掉该项后，仍带该 config 块的已部署 `status-monitor-config.json` 下次升级不再自动剥离 → `DisallowUnknownFields` 严格解析崩。升级前需手动确认/清理此类旧 config。
- 验证全绿：grep 零命中（deploy-monitor/deploymonitor/deploy_monitor/upgrade-monitor 全变体）、bash -n/shellcheck clean、make deploy-smoke 34/0、对抗式核验 workflow（3 agent）0 blocker/high、3 low（binary 计数 + 孤悬 summary + 残留二进制，均已修）。待用户确认提交。

## 前序：miniagent CLI workdir 契约钉死（已提交 `0494eed`）
非空 + 绝对路径 + 只认 `-workdir` flag（跨仓 `/opt/code/miniagent`）。起因是排查「工作目录飘到 /home/dev」，结论为 conflation（systemd 下无真实漂移，`/home/dev` 是 `-config` 路径被相邻行误读），顺带收紧 CLI workdir 契约。验证全绿。**在线 CLI 仍 v4.4.0，需重装才生效**。详见 [tasks/miniagent-workdir-pin.md](tasks/miniagent-workdir-pin.md)。

## 前序：agnes-back 彻底移除（C 档全量清零，已提交 `89e29e6`）
继 claude-back（B+C）之后第二个移除的后端，后端收敛到 **miniagent + status-monitor**。用户拍板彻底移除 + C 档全量清零（接受永久丢失图片/视频生成——agnes 是唯一媒体后端，无法迁移）。前置评估 `.claude/wf-agnes-removal-eval.js`（76 agent 对抗式核验，65 confirmed）。详见 [tasks/agnes-backend-removal.md](tasks/agnes-backend-removal.md)。
- 关键坑：`ComponentLogLevel.Agnes` 嵌套于 `DisallowUnknownFields` → 靠 `deploy-status.sh` removed_blocks 加 `"agnes"` 迁移。
- 运维收尾：纯 agnes 主机手动清单元 + AGNES_API_KEY 吊销 + 手动剔 `.env` 的 `AGNES_*` 行。

## 前序：`/config` 扫描路径可配置（`MINIAGENT_CONFIG_DIR`，已提交）
4 文件改动：`deploy.sh`（`inject_config_dir` + 调用点，双层转义）、`env.example`、`smoke.sh`(+6 断言)、`README.md`。Go 零改动（`ResolveConfigDir` 已就绪）。决策：`.env` 未设 → 回退 `$HOME/.miniagent`。详见 [tasks/config-dir-env-injection.md](tasks/config-dir-env-injection.md)。

## 前序：RUN_USER 解耦 + deploy 权限修复 + run_mode 死代码清理（已提交 `89e29e6`）
- **RUN_USER** 从 `.env` 注入（systemd `User=`/chown），与部署调用者解耦；`deploy_sudo_check` 改校验真正执行内嵌 sudo的**调用者**（INVOKER_USER）免密 sudo，运行用户本身无需 sudo。
- **三项零风险权限修复**（deploy.sh）：① install_files 给 miniagent CLI 兜底 `chmod 0755`；② write_units 条件注入 `Environment=HOME=$STATE_DIR`（**仅 RUN_USER != INVOKER_USER** 时，dev 下 no-op 保留 ~/.miniagent）；③ deploy/README.md 新增「WORKSPACE_ROOT 属主前提」小节。
- **run_mode 死代码清理**：`lib-common.sh` 删 `run_mode()`；`env.example` 删 `LARK_RUN_MODE` 块；`smoke.sh` 删 run_mode/guard_pro_mode 段（断言 38→30）。
- 详见 [tasks/run-user-env-injection.md](tasks/run-user-env-injection.md)。
- 开放项：合并脚本 M2（3 卫星脚本抽 deploy-svc.sh）。
