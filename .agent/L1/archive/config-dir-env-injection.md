---
layer: L1
type: task
status: done
created: 2026-08-11
updated: 2026-08-11T23:38:38+08:00
---

# /config 扫描路径可配置（MINIAGENT_CONFIG_DIR，部署注入）

## 背景
专用服务账号部署下执行 `/config` 报错：
`读取配置目录失败：open /var/lib/lark-bridge/.miniagent: no such file or directory`。
根因：`/config` 选择器 `listConfigFiles(h.configDir)` 扫描的 `configDir` 由
`ResolveConfigDir(cfg)` 解析，`config_dir` 为空时回退 `$HOME/.miniagent`，而专用账号下
`HOME=$STATE_DIR`（修复②注入），该目录从未创建。启动链路不受影响（`config_path` 是显式
绝对路径，从不扫描）。

## 目标
让 `/config` 扫描目录可经 `.env` 的 `MINIAGENT_CONFIG_DIR` 配置，部署时注入到
`miniagent-config.json` 的 `config_dir` 字段。Go 侧 `ResolveConfigDir` 已支持任意路径，无需改。

## 用户拍板
1. `.env` 未设 `MINIAGENT_CONFIG_DIR` → `config_dir` 保持空 → 回退 `$HOME/.miniagent`（不破坏 dev）。
2. 命名错配（选择器只认 `miniagent.json`/`*-miniagent.json`，部署文件是 `miniagent-cli.json`）
   **不在本次范围**——`miniagent-cli.json` 是 bridge 注入的 CLI 默认配置，按设计不进 `/config` 切换列表。

## 关键约束
- `config.expandEnvVars`（config.go:423）**严格模式**：`${VAR}` 未设/空 → fatal。
  `config_dir` 可选允许空，**不能**用 `${}` 占位符 → 必须 deploy.sh sed 注入字面量。
- **双层转义**（修正了 Plan agent 的方案 bug）：值穿过 sed replacement 再落地为合法 JSON。
  `\ → \\\\`、`" → \\"`、`& → \&`、`| → \|`（顺序敏感，`\` 先）。agent 原方案只 sed 层转义
  会产生非法 JSON（`\`/`"` 用例 smoke 测出）。
- **set -e**：调用点用 `if [[ -n ]]; then ...; fi` 而非 `[[ -n ]] && ...`——后者空值时返回 1，
  set -e 语境（尤其函数尾语句）可能误退出。smoke 测出。

## 改动
- `deploy/deploy.sh`：新增 `inject_config_dir()`（inject_router_path 后，双层转义）；
  调用点在 `inject_router_path miniagent` 后（deploy.sh:471 后），`env_get MINIAGENT_CONFIG_DIR`
  非空才注入，仅作用 `$STAGE/miniagent-config.json`。
- `deploy/env.example`：miniagent 段新增 `MINIAGENT_CONFIG_DIR=` + 注释。
- `deploy/tests/smoke.sh`：+6 断言（普通/`/`/空格/`&|`\\/双引号 转义 + 空值跳过）。
- `deploy/README.md`：默认值表加 `miniagent.config_dir`（`~/.miniagent`，.env 注入）。
- 不改：`config.example.json`（保持 `""`）、任何 Go 文件、lib-common.sh（复用 env_get）。

## 验证
- `bash -n` 全过；`shellcheck -S warning` 清零；`make deploy-smoke` **36/36**（原 30 +6）；
  `make test` Go 全绿（无 Go 改动）。

## 已交付
- 已提交（含于 `89e29e6`）。L0#9 全绿：bash -n / shellcheck / smoke 36/36 / Go 测试全绿。
