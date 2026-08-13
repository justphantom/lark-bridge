# lark-bridge Agent 指令

> 本项目使用 `.agent/` 目录作为项目级记忆与约束系统（分层时态记忆）。会话开始时必须执行下面的加载协议。

## 加载协议（会话开始）

1. **全量读入** `L0/*.md`（永久约束，常驻上下文）。
2. **读入** `L1/active/session.md`（当前任务与最近提交）。
3. **按需检索** `L2/`（不盲加载）：先查 `index.md` 的主题/标签定位，再用关键词或语义检索命中文件；未命中不加载。
4. 读取 `.agent/index.md` 了解全部条目布局。

## 写盘协议

- `.agent/` 下所有文件 Agent 与用户均可读写。
- **L0 更新需用户显式授权或手动编辑**，其余层 Agent 可自主更新。
- 引用 `.agent/` 内文件时直接给出相对路径；引用未跟踪路径时内联说明或删除。
- `git`/`go`/`tree`/`ls` 等命令使用 `rtk` 前缀以节省 token（见 L0 constraints #7）。

## 任务状态机（L1）

任务状态变更时（开始 / 聚焦转移 / 完成 / 归档），同步更新 `L1/active/session.md` 与对应任务文件。普通问答轮次不写盘。提交、推送等版本库操作不必记录。

**提交后归档原子流程**（任务完成时必须执行，不可跨会话推迟）：

| 步骤 | 动作 |
|---|---|
| 1 | 翻转任务文件 frontmatter：`status: in_progress` → `status: done` |
| 2 | 正文把"待续 / 未提交"段改写为"已交付"，注明 commit hash |
| 3 | `git mv .agent/L1/tasks/<x>.md .agent/L1/archive/<x>.md` |
| 4 | 更新 `.agent/index.md`：从 tasks → archive 段，补 commit |
| 5 | 更新 `L1/active/session.md`：从"当前任务"移除，可选记入"最近提交" |

## L2 检索与维护

- 检索优先用精确关键词与标签（frontmatter 的 `tags` 字段），必要时辅以语义搜索。
- 任务结束后评估是否沉淀到 L2（`patterns/` / `decisions/` / `incidents/`）。
- 新建 L2 条目前先检索已有内容，避免重复。
- 每个文件须在 frontmatter 含 `verified_at` + `applies_to`（commit 或版本）；
  内容随代码失效时，加 `status: historical` 并在正文顶部加 `> ⚠️ 历史参考：<原因>` 横幅。
- **代码变更同步**（L0 policies #6）：改动 `feishufront`/`feishu`/`lark`/`router`/`config`/`deploy` 核心逻辑后，`grep -rl '<符号/路径>' .agent/L2/` 检查相关条目是否需标 `historical` 或更新引用。
