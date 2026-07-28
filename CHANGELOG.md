# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格，版本号
遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

无。下一版起步请在此追加。

## [1.5.0] - 2026-07-28

v1.4.0 之后的增量完善。无协议层破坏性改动：`PromptPayload.CardMessageID` 为
omitempty 新字段（非 frontend-override），`/v1/metrics/<id>` 与
`/v1/deploy-preflight` 对旧组件静默降级。**发版顺序**：先 feishu-front 后
各 backend（deploy.sh 顺序天然满足）。

### Added

- **总览卡主机/进程监控（多机部署）**：status-monitor 总览卡新增「主机」
  （load / 内存 / state_dir 磁盘占用）与「进程」（各 backend 版本号 +
  systemd cgroup 内存）两个 section。设计文档见
  `docs/status-monitor-metrics-design.md`（push-via-frontend 方案，D1/D2
  已定）。
  - 新增 `internal/hostmetrics` 包：纯函数采集 `/proc/loadavg`、
    `/proc/meminfo`、`statfs`（state_dir 挂载点，失败 fallback `/`）、
    cgroup v2 `memory.current`（读不到 → 卡片显示 `—`），6 个 binary 共用；
    `OutboundIP` 经 UDP 路由查询探测 frontend 实际看到的本机 IP。
  - 协议扩展（全部 `omitempty`，新旧任意组合零影响）：`HostStats` /
    `ServiceStat` / `MetricsReport`；`StatusSnapshot` 与
    `StatusReportPayload` 各加 `Hosts` / `Services`。
  - 上行通道：版本走 SSE handshake `?version=`（一次性）；指标走新
    endpoint `POST /v1/metrics/<backendID>`（周期推送，复用 IPC_SECRET
    Bearer 认证，64KB body 上限，仅接受已注册 SSE 的 backendID）。旧
    frontend 返回 404，backend 静默跳过——非破坏性。
  - `backendrpc.Connect` 改 `ConnectOptions` struct（D1）；新增
    `Client.PushMetrics` 与 `backendrpc.StartMetricsLoop`（推送周期绑死
    `status_monitor.interval`，单一真源）。
  - frontend：`BackendConn` 加 version/metrics 原子字段，`/v1/status`
    按 IP 去重聚合主机行；feishu-front 不 POST 自己，在 handler 内直读
    本机数据合并（D4）；frontend 不持久化 metrics（D5）。
  - 渲染（cardkit `StatusReportInput` options struct，D2）：版本漂移
    （落后于众数版本）行标 `🔴 版本漂移` + 卡头 blue→orange；超过
    `3 × interval` 未更新的行标 `(stale)`；`unknown` 版本不参与漂移判定。
  - **发版顺序约束**：先部署 feishu-front 再部署各 backend（deploy.sh
    顺序天然满足；反序时 metrics 在旧 frontend 上 404，主机/进程 section
    空白至 frontend 升级完成，业务路径不受影响）。

- **总览卡移动端分组布局**：飞书 markdown 用比例字体，原空格对齐的列布局
  在手机上永远错位，~70 字符行会在数字中间折行。改为按主机/服务分块：粗体
  身份行带版本漂移 / stale 标记，下面每指标一行缩进；版本号不再截断，
  summary 拆为两行。

- **deploy-monitor terminal notice 原地 patch 命令卡**：`/deploy` 会重启
  feishu-front，内存中的 promptID→turn 映射被清空，terminal notice 此前
  回退为独立卡，导致原 "⏳ 部署执行中…" 进度卡永久卡死。现在前端在 Prompt
  事件里盖进度卡的 message_id（`PromptPayload.CardMessageID`，omitempty，
  非 override），deploy-monitor 在每个 terminal notice 上原样回带
  （`Notice.UpdateMessageID`），frontend 的 UpdateMessageID 快路径（裸
  message_id 跨重启存活）即可原地 patch 原卡。同时补两个洞：快路径现在会
  释放该 prompt 的 turn（否则 feishu-front 未重启场景如 `/pull` 会留下活
  turn 污染 `/running`），引用卡被撤回时回退 `SendCard`（部署结果不再丢失）。

### Changed

- **deploy.sh 拆 `lib-common.sh` + preflight 冲突规则前置到 frontend**：
  - A) deploy.sh（935 行多职责混杂）重构：共享样板（路径 / 颜色 / RUN_USER /
    env+systemd helper / svc 映射表）移到 `deploy/lib-common.sh`，三个脚本
    source 它；主流程改为 source guard 后面的具名 step 函数，helper 可单元
    测试（`deploy/tests/smoke.sh`，19 条断言，接入 `make test`）。同时修复
    长期存在的 shellcheck SC1073 解析错误，四个脚本 shellcheck-clean。
  - B) 在途 preflight 规则离开 bash 的 sed/grep JSON 解析，改走前端新端点
    `GET /v1/deploy-preflight?services=...`：200 安全 / 409 + 受影响
    backends + 预渲染原因。deploy-monitor 自身的 turn 被排除，远端 `/deploy`
    不会自死锁。deploy.sh 收敛为一次 curl + status-code 分支；前端老于该
    端点时保守回退（任何 inflight 都阻断）。
- **`idle_timeout` 示例改为 `600s`**：`config.example.json` 的
  `timeouts.idle_timeout` 从零值默认（禁用）改为推荐的 `600s`，与 README
  建议对齐；零值依然表示禁用，旧部署行为不变。

### Fixed

- **loopback frontend 探测回退 `PrimaryIPv4`**：与前端同机部署
  （`frontend_url=localhost/127.0.0.1/[::1]`）时，`probeOutboundIP` 报出
  loopback IP，永远匹配不上 feishu-front 自报的 `PrimaryIPv4` ——
  `mergeHostByIP` 遂把一台物理机拆成两行（如 `10.0.2.15` vs `::1`）。
  探测结果为 loopback 时回退 `PrimaryIPv4`；远端前端仍走真实路由 IP。

## [1.4.0] - 2026-07-28

file upload (pandoc) / post rich-text / opencode idle watchdog / deploy-monitor
cgroup 内存回收。所有改动 opt-in：缺依赖 / 权限时降级通知，旧部署行为不变。

### Added

- **opencode-back 空闲看门狗（idle watchdog，opt-in）**：当 opencode
  子进程在 `idle_timeout` 内未吐出任何 stdout 事件时，判定为卡死（上游
  LLM 卡顿 / 内部死锁 / tool 等待 stdin），SIGKILL 整个进程组并向飞书
  发"响应超时"通知，避免用户永久等待却收不到回复（根因：观察到的
  glm-5.2 build agent 偶发于某个 step 中途停滞，`streamRun` 永久阻塞在
  `for ev := range events`，永不 `emitTerminal`）。
  - 与 `prompt_timeout`（总墙钟）互补：`idle_timeout` 每收到一个事件就
    重置，长但活跃的 turn 不会被误杀。
  - 配置：`timeouts.idle_timeout`（0 = 禁用，默认；建议 `120s`）。
  - 实现：`runPrompt` 起一个 `time.AfterFunc`，经 `onActivity` 回调由
    `streamRun` 每事件 reset；触发时 `cancel(errIdleTimeout)` → 进程组被
    `ApplyGroupCancel` SIGKILL → `streamRun` 检测 `context.Cause` 返回
    `isIdleTimeout`，`emitTerminal` 据此发"响应超时"而非"已取消"。
  - 回归测试：`TestStreamRun_IdleTimeoutMarked` /
    `TestStreamRun_UserCancelNotIdle` / `TestLoad_IdleTimeout`。

- **飞书富文本消息（post）支持 + 内联图片下载（opt-in）**：feishu-front
  现可接收 `MsgType=post` 的富文本消息，转换成 Markdown 物化到 inbox。
  设计文档见 `docs/post-rich-text-design.md`（方案 C，已按推荐定档全部
  5 个决策点）。
  - 新增 `internal/feishu/post.go`：`Post` / `PostNode` AST +
    `ParsePost(content)`（locale 兜底）+ `RenderNodeToMarkdown`（纯函数）+
    `RenderPostToMarkdown`（含占位符完整渲染）+ `StripBotMentionsFromPost`
    （@bot / @all 剔除）。
  - 新增 `internal/feishufront/dispatcher_post.go`：`handlePostMessage` 串联
    AST 遍历 → 图片下载 → body.md 落盘 → post 模板渲染 → dispatchPrompt。
  - 内联图片：`tag=img` 下载到 `{inbox}/{chatID}/{promptID}/img-NNN.<ext>`，
    扩展名按响应 `Content-Type` 推断（png/jpg/gif/webp，默认 png）。
  - 错误降级矩阵：单图失败转占位符（`[图片下载失败]` / `[图片过大]` /
    `[图片存储失败]`），整条消息不阻塞；解析失败才整体阻塞。
  - `tag=media`（视频）渲染为 `[视频]` 占位符（agent 读不了视频，不下载）。
  - inbox 布局：扁平（`body.md` + `img-NNN.<ext>` + `raw/post.json`），
    与单文件场景统一。
  - 顺序下载（95% post ≤ 3 图，<3s），错误聚合无需锁。
  - 配置：新增 `file_convert.post_prompt_template`（**可选**，留空时走
    "降级路径"——post 转 Markdown 文本直接发，不下载图）。启用时若配错
    语法在启动时 fail-fast。
  - `IncomingMessage` 加 `Post *Post` 字段；`feishu.buildIncomingMessage`
    在 `case "post"` 时调用 `ParsePost` 一次，dispatcher 直接消费 AST。
  - 安全：复用 `inboxMaxSize` 单图大小上限；`@bot` / `@all` 在渲染前
    剔除；AST 深拷贝避免污染原始事件。
  - 协议层零改动：post 最终仍以纯文本 prompt 抵达后端（含 body.md 路径）。

- **飞书文件上传 → pandoc → markdown 管线（opt-in）**：feishu-front 现可接收
  群聊中上传的 `file` 类型消息（docx/md/markdown/txt），通过自实现的 IM resources
  端点下载二进制，pandoc 转换为 GitHub-flavoured Markdown，落地到
  `{inbox_dir}/{chatID}/{promptID}/{base}.md`，并把绝对路径以指令形式塞进
  prompt 文本发送给绑定的后端 agent（agent 用 Read 工具读取）。
  - 新增 `internal/fileconvert/` 包：docx 走外部 pandoc 子进程
    （`cmdutil.ApplyGroupCancel` 同款进程组 + SIGKILL 超时保护），md/txt 直拷。
  - 新增 `lark.Client.DownloadResource` 与 `feishu.Bot.DownloadFile`。
  - 新增 `config.FileConvert` 段（`enabled` / `pandoc_path` / `inbox_dir` /
    `max_file_size` / `convert_timeout` / `retention` / `prompt_template`），
    默认全部关闭，旧部署行为不变。`prompt_template` 为 Go `text/template`
    字符串（变量 `{{.FileName}}`/`{{.Path}}`/`{{.UserText}}`），启用时必填、
    启动时语法预检；代码层不内置默认文案，默认模板写在示例配置里供操作员
    直接编辑。
  - dispatcher 新增 `SetFilePipeline` + `PruneInbox`；inbox 启动时按 retention
    一次性裁剪。
  - 部署侧 `deploy.sh` 加 pandoc 软预检（warn 不 fail），feishu-front 启动时
    硬预检 `pandoc --version`，缺失且启用时拒绝启动。
  - 安全：inbox 目录 0700、文件 0600、路径元素白名单清洗（防 `..` 穿越），
    下载字节上限 30 MiB（可配），转换超时 60s（可配）。

### Removed

- **删除 `deploy/{claude,opencode,feishu}-config.json` 死模板**：这三个文件
  是 init commit 带进来的早期方案，自 `2f23369` 起 deploy.sh 改为从
  `config.example.json` 统一派生，再无任何 `.go`/`.sh` 消费它们（grep 验证）。
  保留反而误导操作员以为编辑它们会影响 deploy.sh 流程（曾因 schema drift
  触发 DisallowUnknownFields 反复 crash），且与 `Makefile:64` pack 行为
  不一致（tarball 从不打包它们）。`config.example.json` 成为唯一真源。

### Fixed

- **deploy-monitor cgroup 内存回收（MemoryHigh/MemoryMax）**：观察到
  `systemctl status` 报告的 idle Memory 长期 88M+ 不回落。根因是 `make deploy`
  子进程读取的文件页（go-build 缓存、源码、docker 层）作为 `inactive_file`
  留在 cgroup 内核记账里（进程本体 `anon` 仅 7M，`VmHWM` 仅 14M）。
  - `upgrade-monitor.sh` 的 unit 模板加 `MemoryHigh=50M` / `MemoryMax=300M`，
    新部署自动生效；已部署 unit 由新增的 `migrate_unit` 幂等注入。
  - 实测：idle Memory 88.1M → 3.0M，进程 anon 无变化。
  - `MemoryMax=300M` 是硬上限（实测 MemoryPeak ~207M，余量到 300M），
    防一次失控 deploy 把整机吃满。

- **upgrade-monitor EXIT trap 引用已出作用域的 local 变量**：`init_monitor`
  中 `stage` 为 `local`，函数返回后 EXIT trap 访问它会触发 `unbound variable`，
  临时目录泄漏。改用全局 `INIT_STAGE` 替代，trap 在任意路径都能正确清理。

- **修 flaky 测试 `TestWSClient_PingSent`**：`PingInterval: 80ms` 在 `-race`
  下调度抖动，约 10% 概率在 3s 窗口内抢不到时间片而失败（20 次跑出 2 次）。
  interval 提升到 `200ms`（窗口内仍有 ~15 次 ping 机会），`-race -count=30`
  全绿。

- **ParsePost 支持 flat 形态的 post 富文本**：新版飞书客户端发的 post 是
  flat 形态（`title`/`content`/`content_v2` 直接挂在顶层，无 `{"zh_cn":{...}}`
  locale 包裹），旧解析器走 locale-wrapped 分支失败，整个 content 作为
  fallback 文本下发——观察到的现象是 UserText 里塞了完整 content_v2 JSON。
  `ParsePost` 改为顶层有 `content` 键即按 flat 解析，`content_v2` 一律忽略；
  `bot_dispatch` 解析失败时日志补 `raw_content`（1024 rune 截断）便于排查。


- **飞书权限**：使用本功能需在开发者后台为自建应用追加 `im:resource` 权限并
  经管理员审批；缺权限时下载返回 403，前端按 `下载失败` 通知用户。
- **协议层未变**：file 消息最终仍以纯文本 prompt 的形式抵达后端（文本里包含
  绝对路径 + 强指令），`PromptPayload` 结构未改，三个 agent 后端零改动。

## [1.3.0] - 2026-07-28

飞书客户端完全自实现（移除 `oapi-sdk-go` 全部依赖），并新增 status-monitor 总览卡
后端。同步落地 P0–P3 一轮代码评审加固（资源边界 / 鉴权 / 子进程组 / WS 健壮性 /
systemd hardening）。所有改动向后兼容：对外协议字段不变；移除的内部 SDK 类型仅影响
`internal/feishu/`。

### Added

- **status-monitor 后端**（`cmd/status-monitor` + `internal/statusmonitor/`）：
  以 backendType `status-monitor` 注册到前端，按 `status_monitor.interval`（默认
  60s）轮询 `GET /v1/status` 并发 `TypeStatusReport`；前端向每个绑定此后端的群推送
  一张常驻总览卡（在线后端 + 运行中会话数与时长），有则 PATCH、被删则重发。
  push-only；与 deploy-monitor 同样解耦于 `deploy.sh` 之外，由 `make upgrade-status`
  独立管理。
- **renderer subagent zone**：进度卡为 agent 委派（sub-agent delegation）新增专属
  呈现区域，与主 turn 的工具行分离。
- **架构与规范文档**：新增 `ARCHITECTURE.md`（仓库级架构真源）与 `CODING_STANDARDS.md`；
  补充飞书开放平台 API 参考文档。
- **renderer `max_thinking_runes` 配置项**：进度卡"思考中"区的 rune 上限从硬编码 50
  改为可配置（`renderer.max_thinking_runes`，默认仍 50）。同时 Claude 的 thinking
  内容块接入 `TypeThinking` zone（`Replace: true`，与 opencode reasoning 行为对齐）。

### Changed

- **飞书客户端完全自实现**：移除 `github.com/larksuite/oapi-sdk-go/v3` 及其间接依赖
  `gorilla/websocket`、`gogo/protobuf`。`go.mod` 现仅含标准库。新增 `internal/lark/`
  （RFC 6455 WebSocket 客户端 + 手写 protobuf 帧编解码 + 鉴权/REST/重连/分片重组），
  `internal/feishu/` 改为对 `*lark.Client` 的业务封装层。
- **`feishu.Bot.Restart` / `feishu.ErrTooManyRestarts` / `restartMax` 删除**：新客户端
  自管重连、无 goroutine 泄漏，软重启机制不再需要。`cmd/feishu-front` 看门狗
  简化为「超 fatalAfter 仍不健康 → 退出交 supervisor 拉起」。
- **`feishu.IncomingMessage.Mentions` 类型替换**：由 SDK 的 `sdktypes.Mention` 改为
  本包 `Mention`（字段集不变，下游 `internal/feishufront` 零改动）。
- **P1 资源/鉴权加固**：补全各类上界（buffer / 重连预算 / sweep）、IPC 鉴权强度提升、
  子进程 env 收敛到白名单传递。
- **P2 清理**：死代码删除、flap 状态机竞态修复、deploymonitor graceful drain、
  wsclient 按职责拆分。
- **P3 工程化**：build tags、systemd unit hardening、sudoers 指引、补测试。

### Fixed

- **WS `card.action.trigger` ACK 3 秒回滚**：ACK 现把业务字段置于顶层并携带 `card`，
  阻止飞书侧 3 秒未应答导致的卡片状态回滚。
- **picker 点击无更新**：卡片 schema 回退到 v1 让 PATCH 持久化，点击改为延迟 PATCH
  （`context.WithoutCancel` 脱离请求生命周期），并删除失效的 ACK 死代码。
- **claude `server_tool_use` 不再丢失**：服务端工具调用现正常呈现，`task_kind`
  在整个 turn 生命周期内稳定。
- **P0 修复**：飞书 mention bot 识别字段名修正、子进程漏接进程组 kill 致
  `cmd.Wait()` 永久阻塞、WS receiveLoop 半开连接（新增读超时）。
- **status-monitor 多行 `base` 配置校验**：`deploy.sh` 在多行 base 下不再误判。
- **deploy.sh 配置字段迁移**：升级时在重启 monitor 前迁移已移除的 config 字段。

## [1.2.0] - 2026-07-26

opencode-back 与 claude-back 的工具事件呈现重构，外加 `claude-go-sdk` 内联
消除一项直接 module 依赖。所有改动向后兼容（`TypeTodo` 协议早就有，本次只是
首次接入；`Mention` 类型替换在内部包，对外 API 不变）。

> 本段同时补记 v1.1.0 期间合入但当时未在 CHANGELOG 注明的两项
> （`opencode-serve-back` 整体移除、`opencode-back /session-use`）。

### Added

- **进度卡 Zone 3.5 todo 清单渲染**：opencode-back 的 `todowrite` 与
  claude-back 的 `TodoWrite` 工具事件现路由到 `TypeTodo`，进度卡片原生展示
  ✅/⏳/⬜/✘ 清单（≤10 项展开、>10 项折叠为 `清单 N/M · ✅a ⏳b ⬜c ✘d`、
  cancelled 灰显），不再以 raw-JSON 工具行呈现。失败 fallback 仍走 `TypeToolResult`
  以保留可见性。
- **`/backend` picker 10min TTL**：未被点击的选择卡 10 分钟后自动翻"已失效"，
  与后端 interactive 卡的 TTL 行为对齐；点击即取消定时器。
- **opencode-back 新增 `/session-use`**（v1.1.0 期间合入，补记）：从
  opencode-serve-back 移植，CLI 模式通过 `--session <id>` 续接历史会话。

### Changed

- **`claude-go-sdk` 内联**：从外部 module 依赖变为 `internal/claude/` 子包
  （9 个非测试文件 + 10 个测试，~2200 行）。lark-bridge 的 `require` 从 2 个
  缩减到 1 个（仅剩 `larksuite/oapi-sdk-go/v3`）。`NOTICES.txt` 记录上游 commit。
  Logger 通过 `log.Logger = slog.Logger` type alias 零适配。
- **`/backend` picker 一卡片原则**：离线/`router.Set` 失败路径改为原地 `UpdateCard`
  翻红（不再发独立 notice），与成功路径的绿色翻红对称。`renderBackendResult`
  泛化为 `renderBackendOutcome(level)`，picker footer status 从「选择后端」改
  为「待确认」让 `RenderInteractiveExpired` 的 footer 翻转生效。
- **进度卡工具行字段上限 50 runes**：`name` 与 `desc` 各自独立常量
  （`maxToolNameLen` / `maxToolDescLen`，与已有 `maxToolOutputLen` 同值但独立
  命名保留语义），通过 `truncateRunes` 截断 + `…` 后缀。` ×N` 计数后缀不算
  入 50 runes 预算。

### Fixed

- **opencode `edit` 工具的 title 缺失不再 dump 完整 input JSON**：
  opencode CLI 部分版本不在 edit 工具事件里填 `part.title`，旧 fallback
  `stringifyJSON(state.input)` 会把整个 input（含 `oldString`/`newString`
  数百字符）当作工具行 desc，且后续 `(+N -M)` diffstat 拼接破坏 JSON 结构
  让下游 `SummarizeToolInput` 无法二次提取。新增 `extractToolInputField`
  按优先级表（`file_path`/`filePath`/`command`/`pattern`/`path`/`query`/
  `description`）提取单字段，仅当无匹配时落回 `stringifyJSON`。
- **`/backend` 提示文案错误**：`dispatcher.go` 的"后端离线"提示指向不存在的
  `/backend use {id}` 子命令（README 同样过时），改为「请用 `/backend`
  重新选择在线后端」。

### Removed

- **`opencode-serve-back` 整体移除**（v1.1.0 期间合入，补记）：CLI 模式
  （`opencode-back`）功能已对齐，独立维护两套 opencode 对接代码（CLI 子进程
  vs `opencode serve` HTTP）的成本超过收益。本次移除包括：
  - `cmd/opencode-serve-back/` 与 `internal/opencodeservebridge/`（约 7800 行）。
  - `opencode-go-sdk-lite` Go 依赖（仅此包使用）。
  - `config.OpencodeServe` 字段/默认值/校验/测试。
  - `deploy/opencode-serve-config.json`、`Makefile` build target、`deploy.sh`
    的 `probe_opencode_serve` / `svc_unit` 分支 / SELECTED 默认数组 / 派生 config。
  - `deploy.sh` 新增遗留清理：升级时自动 `disable --now` 并删除已部署的
    `lark-opencode-serve-back.service` unit、`/etc/lark-bridge/opencode-serve-config.json`
    以及 `STATE_DIR` 下的 `opencode-serve-router.json` / `usage-opencode-serve.json`。

## [1.1.0] - 2026-07-25

交互卡片与 git/deploy 命令的呈现与生命周期重构。所有改动向后兼容（新增协议字段均
omitempty；旧前端遇 null 自动回退）。

### Added

- **进度卡交互门横幅**：mid-turn permission/question 阻塞态、picker 加载态现以
  `TypeProgress` banner 呈现在流式进度卡顶部（`ProgressPayload.Gate`/`Description`），
  不再走被 dispatcher 丢弃的 `TypeText`。banner 四态：⏸ waiting / ✓ answered / ✗
  denied / • loading。
- **权限卡结构化正文**：`PermissionPayload` 增 `Type/Title/Detail`，渲染为徽标 + 标题
  + 代码块详情；`Title` 空时自动回退 `Message`。
- **question 自适应渲染**：单问、单选、≤4 项、无自定义输入 → 即时按钮卡（免下拉+提
  交两步）；其余仍走表单。
- **/deploy-force 二次确认门**：destructive 部署现需 TypePermission 卡片确认（复用
  `bridgebase.AnswerBroker` + TypeAnswer 路由）；普通 /deploy 不加门。
- SSE 静默时 `OnIdle` 兜底取回最终回复。

### Fixed

- **turn 泄漏修复**：`/pull` `/push` `/deploy` `/deploy-force` 终态现绑定 replyToID，
  进度卡不再卡死"处理中"、`/v1/status InFlight` 不再虚高（原会阻塞 `deploy.sh`）。
  命令改为单卡生命周期（非终态 banner → 终态 in-place patch）。
- **已应答卡推进到"已完成"**：submit 不再删缓存+摘绑，`finalizeLinkedInteractive` 能
  把同一张卡从 submitted 推进到 finalized，保留"✓ 已回答"echo（C5）；`rewriteFooterStatus`
  推广为 `待确认|处理中 → X`，终态粘住不可回退。
- **死回显清理**：删除桥层发出的、被 dispatcher 丢弃的 `TypeText` 应答回显；picker 加载
  文案迁到 `TypeProgress.Description`（opencode-back / opencode-serve-back / miniagent）。
- **tail 输出按 rune+行边界截断**：修中文日志（3 字节/字）被字节截断产生的乱码与半行。
- **"始终允许"标注全局作用域**：`PermissionReplyAlways` 的全局持久授权在按钮上显式标
  注「（全局）」。

### Changed

- `bridgebase.AskPermission` 签名收 `protocol.PermissionMessage`（结构化正文载体）。
- `bridgebase.WithReplyToID` 导出（`ReplyToID` 的逆，供测试/直驱命令 handler 用）。
- `bridgebase.GitRunner.AcquireAndRun` 改返回 `bool`，不再自发"已触发"（caller 发 banner）。
- deploymonitor 拆 `confirm.go`（force 确认门）/`render.go`（格式化）以守住 300 行上限。
- `opencode-go-sdk-lite` → v0.2.0；ListModels/ListAgents 缓存移至 `lists.go`。

## [1.0.0] - 2026-07-25

首次正式发布。

### 架构

1 前端 + N 后端的拆分架构：飞书 WebSocket 机器人 + IPC 服务（SSE + Control POST）
作为前端，按 chatID 绑定一个后端；后端按场景分四类。

- **feishu-front**：飞书 WS 长连接 + IPC 服务 + 路由 + 分发器
- **claude-back**：每个 prompt fork `claude` CLI
- **opencode-back**：每个 prompt fork `opencode` CLI
- **opencode-serve-back**：连常驻 `opencode serve` HTTP server，长连接复用，适合长期高并发
- **miniagent-back**：每个 prompt fork miniagent 二进制（自带 ReAct 循环 + LLM 直调）
- **deploy-monitor**：接收飞书群 `/deploy` `/pull` `/push`，独立部署（避免循环依赖）

### 关键能力

- **协议层**：纯结构 + Validate 的 Event/Control 双向协议
- **并发安全**：router/usage 双锁、backendrpc 原子重连、SDK 子进程组 SIGKILL（cmdutil.ApplyGroupCancel）
- **鉴权**：IPC 共享 Bearer + `subtle.ConstantTimeCompare` 防 timing attack
- **资源管理**：bridgebase.Close 顺序幂等、atomicwrite 原子落盘、streamarchive 路径净化
- **卡片渲染**：飞书卡片 progress / question / permission / result 四类渲染
- **可观测**：所有进程统一 slog 配置，支持级别 / 输出 / 格式 / 分组件级别
- **部署**：systemd unit + `deploy.sh`（含 `--binaries` `--services` `--init`）+ 二进制 tarball 分发

### 测试

测试代码占比 50.4%（18,975 / 37,686 行），644 个测试函数，`go test -race ./...`
全绿，`go vet` 干净。`cmd/*/main_test.go` 覆盖各二进制入口的错误路径，
`internal/protocol`/`internal/config`/`internal/feishufront` 表驱动覆盖每条
validate 与 enum 校验路径。

### 1.0.0 发布前审计修复

发布前完成全部 P0 与 P1 阻断/严重项，以及 P2 大部分打磨项，主要落地：

- **稳定性**：abort 后子进程组 SIGKILL（`cmdutil.ApplyGroupCancel`/`RunCombinedBounded`）、
  关键路径 panic recover（control pump + 3 个 SDK 入口）、picker RPC 30s 超时、
  `bot.Restart` 串行化、卡片 element 50 上限防御、`PromptPayload` 覆写字段协议级拒绝。
- **协议**：`todo.status`/`priority`/`notice.level` enum 硬校验。
- **资源**：`wasOffline` 上限触发全量重置、`opencode-serve` Close 与 LRU 并发守门、
  `accText` 峰值减半、`MaxStreams` 从 `AgentConfig` 注入、`DispatchCardAction`
  单 Lock 段防双 finalize。
- **配置/部署**：`OpencodeServe.MaxConcurrent` defaults+validate 对齐、
  `deploy/*.json` 与 `.env` 经 `${VAR}` 联动、deploy.sh 二进制存在性全检 +
  `log_level` 占位符 regex 容错、upgrade-monitor.sh SC2015 修正。
- **文档**：Makefile/README/deploy 二进制与服务数真源统一（6 个二进制 / 5 个业务
  systemd 服务）、deploy/README 双重真源警示、补 LICENSE（MIT）与 CHANGELOG。
