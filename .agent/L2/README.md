# L2 — 经验教训

按主题存放：

- `patterns/`：可复用的设计模式、实现套路与最佳实践。
- `decisions/`：架构决策记录（ADR）。
- `incidents/`：线上/本地问题根因、排查过程与修复记录。

## 写入规则

1. 新建条目前先检索已有内容，避免重复。
2. 使用 Markdown + YAML frontmatter，至少包含 `layer`、`type`、`tags`、`created`。
3. 条目应包含“现象/背景、根因/理由、做法、参考”。
4. 与 minimem 互补：minimem 用于快速语义检索，L2 用于结构化可 diff 的项目知识库。
