# .agent — 项目级 Agent 记忆

> 所有协议规则（加载/写盘/归档/L2 检索）见根目录 `CLAUDE.md`，此处不重复。

## 目录结构

本目录存放 `lark-bridge` 项目级 Agent 记忆，分三层：

- **`L0/`** — 永久约束（persona / constraints / policies），每次会话全量加载。
- **`L1/`** — 过程上下文（`active/session.md` + `tasks/` + `archive/`），任务完成归档。
- **`L2/`** — 经验教训与可复用知识（`patterns/` / `decisions/` / `incidents/`），按需检索。
- **`index.md`** — 全部条目的检索索引（L1 当前/归档 + L2 主题 tag 映射）。
