---
updated: 2026-08-12T10:20:00+08:00
---

# 会话状态

## 当前任务
**dormant config 迁移：删 3 个 Timeouts 死字段**（`PromptTimeout`/`IdleTimeout`/`UsageSessionTTL`——分别标注「consumed by claude/opencode/usage store」，三者皆随后端移除而死）。净 −119 LOC（6 文件 +4/−123）。
- 迁移安全：`deploy.sh` 每次部署从 `config.example.json` 全量重生成 feishu/miniagent config（line 672「Configs are deploy artifacts; each run copies…」）→ 删 example 条目即安全，无需 deploy.sh 迁移。`deploy-status.sh` **保留**已部署 status-monitor config + 跑 `migrate_config` → `removed_keys` 加 `prompt_timeout`/`idle_timeout`/`usage_session_ttl` 三叶字段，升级时剥离。
- 同步删：`config_defaults`（UsageSessionTTL 默认）、`config_validate`（PromptTimeout/UsageSessionTTL 下限）、`config.example.json`（idle_timeout/usage_session_ttl）、5 个测试函数 + 2 表项。`Timeouts` 仅留 `BackendHealth`（feishu-front live）。
- ⚠️ **有意保留 dormant**：`ComponentLogLevels`——其迁移有形式依赖（空 `{}` 单行 vs 操作员多行块），`removed_blocks`(sed range) 与 `removed_keys`(line regex) 都不能同时干净处理两种形态；~15 LOC 全惰性（parsed+validated+never applied），按项目既有「保留 dormant」安全模式留下。
- 验证全绿：build/vet/test -race/golangci-lint 0 issue、deploy-smoke 34/0、deploy-status.sh bash -n/shellcheck clean、config.example.json JSON 合法。**6 文件未提交**。
- 仍开放：`backendrpc.Run`（转 `RunWithClient` 后删）；前端 `ackTerminal` 孤儿发送方；`ComponentLogLevels`（需更健壮的 block stripper 才能安全删）。

## 前序：结构化重构（已提交 `be5131f`）
`GoSafe` 抽 leaf `internal/gosafe.Go`（删 bridgebase+backendrpc 两副本，feishufront no-log 变体保留）；`Commands[H]` 去泛型——命令调度机械从 bridgebase 迁到唯一消费者 miniagent（具体 `*Handler`），测试随之搬迁（dummy `int`→`&Handler{}`）。LOC 中性、结构收益。

## 前序：Tier-2 dead-code 扫尾（已提交 `7f5c85c`+`477fdd9`）
9 项跨 7 包：strutil `Stringify*`、cmdutil `ErrorResult`/`ChangeResult`、eventmetrics `UnknownEvent`/`Overflow()`、router `Binding.Agent`+`Bind(agent)` 参、protocol `PromptPayload.Agent`、`WithLogLevel` 全链、pptx Go 字段、backendrpc `Client.backendType`、miniclient `rawEvent.CallID`（config dormant 字段按 DisallowUnknownFields 保留）。

## 前序：移除 SubagentSummary 机制（已提交 `7f8e172`）
~1.1K 行（15 文件 +141/−1218，含整删 `progress_subagent.go`+test 658 行）。protocol 去 `SubagentSummary`+`IsSubagent`/`TaskID`/`Subagent` 字段+3 校验器；renderer 去 subagent zone + `AddToolUse/AddToolResult` 去 `isSubagent`/`taskID` 参；dispatcher 去 Subagent!=nil 路由。protocol 走裸 `json.Decode` → 删字段对已部署 wire payload 向后兼容。

## 前序：激进清零某已移除后端的残留引用（已提交 `ae071d1`）
CHANGELOG 历史 + `.agent` 记忆 + `deploy-status.sh` 迁移数组全清零。⚠️ 运维后果：迁移数组删该项后，仍带该 config 块的已部署 `status-monitor-config.json` 下次升级不再自动剥离 → 严格解析崩，升级前需手动清理。

## 前序：代码简化 quick wins（已提交 `f8a7239`）
后端收敛评估产出 35 个简化机会（4 项已对抗式核验）。用户选「低风险 quick wins」批次，已执行 + L0#9 全绿：**净 −926 LOC**（11 文件 +36/−962）。
- 删除：`bridgebase/util.go`(+test，156 行)、`ack_registry.go`(+test，204 行) + `EmitTerminalControl` 去 always-nil `acks` 参 + ACK 分支（坍缩为纯重试）、`interactive.go` 剪到仅 `PickAnswerValue`+`EmitFunc`（删 AskAndWait/AskCardUpdate/StaticOptions/newRequestID + 4 常量，~386 行）、`deploy.sh` 死的 `inject_router_path` 行。
- 顺带修了 `TestTerminalRetryBackoff` 一处预存 printf bug（漏 `got` 参，gate 抓出）。
- **有意推迟**：`backendrpc.Run`（reviewer 低估 test 涟漪——5 个 `TestRun_*` 经 Run 覆盖重连/放弃/重置路径，非机械删除；留作后续转 RunWithClient 再删）。
- 开放（已识别未做）：Tier-2 中量级（Commands[H] 去泛型、GoSafe 三处去重、需迁移的 config dormant 字段 `Timeouts.*`/`ComponentLogLevels` 等）；backendrpc.Run（转 RunWithClient 后删）。
- 残留孤儿：前端 `feishufront/dispatcher_control.go` 的 ackTerminal 发送方现无消费者（无害，单独小清理）。
- 验证全绿：build/vet/test -race/golangci-lint 0 issue/deploy-smoke 34/0。

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
