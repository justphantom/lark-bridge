---
updated: 2026-08-11T23:38:38+08:00
---

# 会话状态

## 当前任务
**彻底移除 agnes-back（C 档·全量清零）—— 全部实施 + 验证通过，未提交**（待用户决定 commit）。详见 [tasks/agnes-backend-removal.md](tasks/agnes-backend-removal.md)。

继 claude-back（B+C）、deploy-monitor 之后第三个移除的后端，后端收敛到 **miniagent + status-monitor**。用户拍板：彻底移除 + C 档全量清零（接受永久丢失图片/视频生成——agnes 是唯一媒体后端，无法迁移）。前置评估 `.claude/wf-agnes-removal-eval.js`（76 agent 对抗式核验，65 confirmed）。
- 改动：删 Go 包(8 文件/2494 行) + config 全量清零(struct/字段/ComponentLogLevel.Agnes/defaults/validate/example/test) + Makefile(目标/列表/.PHONY、计数 4→3) + deploy(cleanup_legacy 加 lark-agnes-back+agnes-back-config.json、deploy-status removed_blocks 加 "agnes"、删 deploy-agnes.sh、env.example/smoke.sh/deploy-README §6.7/lib-common 注释) + 共享注释(registry/cardkit/sink) + 文档(README/ARCHITECTURE §5.6 整删+§5.7–5.12→5.6–5.11 重编号/CODING_STANDARDS/RELEASING) + .agent(new-backend-skeleton 死链重指 status-monitor、diagnostic-logging/multi-question-card 案例标历史)。
- 验证全绿：build/vet/test -race ./...、golangci-lint 0 issue、deploy-smoke 34/0、shellcheck -S warning 清零、bash -n 全过。grep 残留全合法（向后兼容迁移 / 历史 / 再对接知识 / 测试夹具标签）。
- 关键坑：`ComponentLogLevel.Agnes` 嵌套于 DisallowUnknownFields（claude 没有的 footgun）→ 靠 deploy-status removed_blocks 加 "agnes" 迁移。
- 运维收尾：纯 agnes 主机手动清 lark-agnes-back 单元/config/二进制；AGNES_API_KEY 去控制台吊销 + 手动剔 .env 的 AGNES_* 行（backfill_env 只增不删）。

## 前序：`/config` 扫描路径可配置（`MINIAGENT_CONFIG_DIR` 部署注入）—— 已完成，未提交
详见 [tasks/config-dir-env-injection.md](tasks/config-dir-env-injection.md)。
- 4 文件改动：`deploy.sh`（`inject_config_dir` + 调用点，**双层转义**）、`env.example`、`smoke.sh`(+6 断言)、`README.md`。
- Go 零改动（`ResolveConfigDir` 已就绪）。验证：bash -n / shellcheck clean / deploy-smoke **36/36** / go test 全绿。
- 决策：`.env` 未设 → `config_dir` 空 → 回退 `$HOME/.miniagent`；命名错配（`miniagent-cli.json` 不匹配过滤）不在范围。
- 关键坑：`expandEnvVars` 严格模式 → 不能用 `${}` 占位符，必须 sed 注入字面量；双层转义（值穿 sed 再落 JSON）；`set -e` 下用 `if` 非 `[[ -n ]] &&`。
- 待续：用户确认提交；可选手动端到端（`./deploy/deploy.sh --services miniagent` 后查 `/etc/lark-bridge/miniagent-config.json`）。

## 前序任务：deploy 改造三件套（已完成，待提交）
deploy 改造三件套，**全部实施 + 验证通过，未提交**（待用户决定 commit）：
1. RUN_USER 解耦（前序，未提交）
2. **彻底移除 deploy-monitor**（飞书 /deploy 触发下线，改为仅手动部署）
3. **三项零风险权限修复**

### 改动概览（本次 deploy-monitor 移除 + 权限修复）
- **删除**：`cmd/deploy-monitor/`、`internal/deploymonitor/`、`deploy/deploy-monitor.sh`（8 文件 ~2365 行）。
- **Go**：`internal/config`（删 DeployMonitor 字段/struct/ComponentLogLevel/默认/校验）；`internal/feishufront`（删 turn.go typeResolver + InFlight 排除、ipcserver_preflight/control 排除、feishu-front/main.go 注入）；多处注释去 deploy-monitor 措辞；测试同步（删 2 测试、重写 4 处）。
- **DisallowUnknownFields 级联**：`config.example.json` 删 deploy_monitor 块；`deploy-status.sh`/`deploy-agnes.sh` 的 `removed_blocks` 追加 `deploy_monitor`（向后兼容已部署 config）。
- **死代码清理**：`lib-common.sh` 删 run_mode()；`env.example` 删 LARK_RUN_MODE 块；`smoke.sh` 删 run_mode/guard_pro_mode 段（断言 38→30）；Makefile 删 build-deploy-monitor/deploy-monitor 目标（5→4 二进制）；.gitignore 删 /deploy-monitor。
- **权限修复**（deploy.sh）：① install_files 给 miniagent CLI 兜底 `chmod 0755`（保 other-x）；② write_units 条件注入 `Environment=HOME=$STATE_DIR`（**仅 RUN_USER != INVOKER_USER** 时——dev 下 no-op，保留 ~/.miniagent）；③ deploy/README.md 新增「WORKSPACE_ROOT 属主前提」小节。
- **文档**：README/ARCHITECTURE/CODING_STANDARDS 清零 deploy-monitor 引用（ARCHITECTURE §5.x/§8.x 重排号）。

### 验证（全绿）
go build/vet/test -race ./...、golangci-lint 0 issues、make deploy-smoke 30/30、shellcheck -S warning 清零、bash -n 全过。全仓 grep 仅余 2 处 `removed_blocks` 里的 deploy_monitor（向后兼容，预期）。

### ⚠️ 工作树有先于本会话的未提交改动（非本任务）
`internal/log/base_logger.go` 删除、`internal/usage/` 删除、`router/`(accessors/binding/router/persistence)、`lark/`(websocket/ws)、`cmdutil`、`bridgebase/interactive.go`、`eventmetrics` 等修改——compile/test/lint 全过、无引用，属前序重构遗留。提交时按需选择性 `git add`。

## 活跃文件（本次核心）
deploy/{deploy.sh,lib-common.sh,deploy-status.sh,deploy-agnes.sh,env.example,tests/smoke.sh,README.md}、config.example.json、Makefile、internal/{config,feishufront}/*、cmd/feishu-front/main.go、README.md/ARCHITECTURE.md/CODING_STANDARDS.md、.gitignore。

## 决策与理由
- 用户拍板：RUN_USER 保持 dev；**彻底移除 deploy-monitor**（非 pro 开关）；落地**三项零风险权限修复**。
- 飞书 /deploy 与 RUN_USER 解耦本质冲突：monitor 以服务用户 uid exec make deploy → 内嵌 sudo 要求服务用户自身有 sudo → 与「运行用户无需 sudo」矛盾。移除即消解。
- 修复2 用条件式（RUN_USER != INVOKER_USER）而非无条件：CLI 总拿显式 -config（配置不受 HOME 影响），但 /config 选择器扫 $HOME/.miniagent——无条件注入会迁走 dev 现有缓存，故 dev 下 no-op、仅专用账号生效。

## 待续步骤
- 用户确认后提交（L0#9）。注意与先序未提交改动区分 staging。
- 开放项：合并脚本 M2（3 卫星脚本抽 deploy-svc.sh）。V1/V2 随 deploy-monitor 移除自然消失。
