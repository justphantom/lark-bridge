---
layer: L1
type: task
status: done
created: 2026-08-12
updated: 2026-08-12T01:45:00+08:00
repo: /opt/code/miniagent  # 跨仓改动：miniagent CLI（独立 Go module）
---

# 钉死 miniagent CLI 的 workdir 契约

## 背景
排查"miniagent-back 工作目录飘到 /home/dev"得出结论：systemd 部署下**无真实漂移**，
`/home/dev` 仅是 `-config`（配置文件）路径，被 `/current` 里相邻行误读为工作目录（conflation）。
顺手发现 CLI 的 workdir 契约偏松：config `run.workdir` 可覆盖 flag、`absWorkdir` 空→`os.Getwd()` 回退、
仅 default 模式校验。用户拍板「钉死」：非空 + 绝对路径 + 只认 `-workdir` flag + 不从其它地方取值。

## 改动（全在 /opt/code/miniagent，选项 A：连 config 字段一起删）
- `cmd/miniagent/setup.go`：`effectiveWorkdir` 只返回 `*f.workdir`（去 resolved 形参与 config 分支）；
  `validateConversation` 无条件校验非空 + `filepath.IsAbs`；`collectOverrides` 删 workdir override。
- `cmd/miniagent/main.go`：`effectiveWorkdir(f)` 调用点 + `-workdir` flag 文案标 REQUIRED/绝对。
- `internal/miniagent/config/resolve.go`：删 `CLIOverrides.Workdir` + `ResolvedRun.Workdir` + 三态仲裁里的 workdir 段。
- `internal/miniagent/config/config.go`：删 `RunConfig.Workdir`（`run.workdir` 配置项）。无 DisallowUnknownFields → 旧配置残留键静默忽略。
- `cmd/miniagent/session.go`：`absWorkdir` 空返回 `""`，删 `os.Getwd()` 回退。
- 文档：`config.example.json` / `README.md` 删 `run.workdir`；README flag/错误码/退出码/示例（相对路径→`"$PWD"`）同步。
- 测试：所有 e2e 子进程用例显式补 `-workdir <tmp>`（auto 模式不再豁免）；负向 `TestCLI_DefaultModeRequiresWorkdir` 保留无 workdir。

## 验证（全绿）
go build / vet / test -race ./...、golangci-lint 0 issue。smoke：auto+default 无 workdir→`required`；
相对路径→`must be absolute`；绝对路径→放行至 API key 校验；`--version`/`-replay` 旁路不受影响。

## 待续 / 注意
- **未提交**（miniagent 独立仓，待用户 commit）。
- **部署未生效**：在线 `/usr/local/bin/miniagent` 仍是 v4.4.0；需 `make build` 重装 CLI 后 miniagent-back 才用上新契约。
- bridge 侧零改动（恒传绝对 `-workdir`，本就合规）。
- 关联：本次"漂移"根因是 conflation，见 [[miniagent-integration]] 新增缺口 6。
