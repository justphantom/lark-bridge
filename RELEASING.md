# 发版规范（Releasing）

> 本文档归纳自 v1.0.0–v1.7.0 共 8 次发版的实际提交记录，是本仓库发版操作的唯一规范。
> 版本号遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)，CHANGELOG 遵循
> [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

---

## 1. 版本号规则

- 格式 `vMAJOR.MINOR.PATCH`，git tag 即版本真源（见 §4）。
- 历史全部为 minor 递增（1.0.0 → 1.7.0）：**新增功能一律升 minor**。
- 含 breaking change 时仍升 minor（当前 1.x 阶段惯例，如 v1.6.0 含 2 处 breaking），
  但必须在 tag message 与 CHANGELOG 中显式标注 breaking 内容。
- PATCH 预留给纯修复发版（历史尚无实例）。
- 无协议层 breaking change、仅修复 → 可 PATCH；有新增功能 → 至少 minor。

## 2. 发版前置检查（Release Readiness）

发版前必须全部满足：

1. `go build ./...` 通过；`go test -count=1 ./...` 全绿（`-race` 下无 flake）。
2. `golangci-lint run ./...` 0 issues（回归修复须先于发版落地，如 v1.8.0 修 23 处）。
3. CHANGELOG `[Unreleased]` 已完整覆盖本版全部 commit——逐条对照
   `git log <上个tag>..HEAD` 核对，漏记是大忌（v1.5.0 曾因漏记 5 个 commit 返工）。
4. 发版前没有未解决的安全问题和规范性问题。
5. `ARCHITECTURE.md` 头部版本号、调查日期、规模统计（Go 文件数/行数/包数）刷新。

## 3. 发版提交（Release Prep Commit）

只做一个提交，message 格式：

```
chore(release): vX.Y.Z prep — <一句话内容，如 changelog backfill, doc refresh>
```

内容固定三件事：

1. **CHANGELOG 切割**：把 `[Unreleased]` 的内容落到 `## [X.Y.Z] - YYYY-MM-DD`，
   顶部留一个新的空 `[Unreleased]`。
2. **文档刷新**：`ARCHITECTURE.md` 版本号/统计、`deploy/README.md` 新增配置项等。
3. **收尾小修**：仅允许测试修复、注释修正一类零行为改动（如 v1.4.0 修 flaky ping
   test、v1.7.0 补 omp-back main_test）；**功能代码必须在 prep 提交之前独立成提交**。

## 4. 打 Tag

- **annotated tag**（`git tag -a`），不用轻量 tag；tag 名为 `vX.Y.Z`。
- tag message 格式：

```
vX.Y.Z: <主线一句话（逗号分隔的主题列表）>

<2-5 句正文：本版主线、关键加固/重构。>
<有无 breaking change；有则列明。>
<发版顺序提示（见 §5）。>
详见 CHANGELOG.md。
```

- **版本号不进代码**：`Makefile` 用 `git describe --tags --always --dirty` 经
  `-X main.version=$(VERSION)` 注入二进制，tag 一打版本即生效，无需改任何源文件。

## 5. 发版顺序

**先 feishu-front，后各 backend**（claude / opencode / omp / miniagent）。
`deploy.sh` 的服务部署顺序天然满足该约束；前端先升级可保证新协议字段（Event/Control）
对旧后端向后兼容，后端后升级再启用新能力。

## 6. CHANGELOG 编写细则

- 节区固定顺序：`Added` / `Changed` / `Fixed` / `Notes`（无内容则省略该节）。
- 每条目注明来源 commit 短哈希（反引号包裹，如 `（6a15532）`）。
- 开头一段「增量主线」：一句话总述 + 本版主题加粗列出 + semver 判断依据 +
  发版顺序提示。
- P0/P1 级 Fixed 条目需写清：根因、现状（修复后行为）、涉及文件、测试名。
- 破坏性变更、配置新增、部署行为变化必须显式出现，不得埋在叙述里。
- 禁止引用非版本跟踪的文件。

## 7. 提交信息风格（与发版相关的约定）

- 格式：`<type>(<scope>): <主题>`，scope 为包名（feishufront/ompbridge/protocol/…）。
- type：`feat` / `fix` / `refactor` / `style` / `docs` / `chore`；一个提交只做一件事，
  跨主题的改动拆多个提交（参考 v1.8.0 期间 fix/style/docs 三分提交）。
- 主题行后可接 `—` 补充细节；正文写动机与关键设计取舍，不重复 diff。

## 8. 发版检查单（速查）

- [ ] 测试全绿 + lint 0 issues
- [ ] CHANGELOG `[Unreleased]` 完整、无漏记（对照 git log）
- [ ] `chore(release): vX.Y.Z prep` 提交（changelog 切割 + 文档刷新）
- [ ] annotated tag `vX.Y.Z`，message 含主线/breaking/发版顺序
- [ ] `git push origin main --tags`
- [ ] 部署按序：feishu-front → 各 backend
