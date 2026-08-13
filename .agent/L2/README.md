---
layer: L2
type: readme
created: 2026-08-09
updated: 2026-08-13T18:00:00+08:00
---

# L2 — 经验教训

按主题存放：

- `patterns/`：可复用的设计模式、实现套路与最佳实践。
- `decisions/`：架构决策记录（ADR）。
- `incidents/`：线上/本地问题根因、排查过程与修复记录。

## 写入规则

1. 新建条目前先检索已有内容，避免重复。
2. 使用 Markdown + YAML frontmatter，至少包含 `layer`、`type`、`tags`、`created`。
3. 条目应包含"现象/背景、根因/理由、做法、参考"。
4. 与 minimem 互补：minimem 用于快速语义检索，L2 用于结构化可 diff 的项目知识库。

## frontmatter schema

| 字段 | 必填 | 取值 |
|---|---|---|
| `layer` | 是 | `L2` |
| `type` | 是 | `pattern` / `decision` / `incident` |
| `status` | 否 | `active`（默认）/ `historical`（代码已删，留作参考） |
| `tags` | 是 | 关键词数组，用于检索 |
| `created` | 是 | `YYYY-MM-DD` |
| `updated` | 否 | `YYYY-MM-DDThh:mm:ss+08:00` |
| `verified_at` | 否 | 最后一次与代码核对日期 |
| `applies_to` | 否 | 适用 commit/tag；`historical` 条目必填 |
| `confidence` | 否 | `high` / `medium` / `low` |

## 历史标记

当引用的代码/能力已被移除（如已下线的后端），**必须**：

1. frontmatter 加 `status: historical` + `applies_to: pre-<commit>`。
2. 正文标题下方加 `> ⚠️ 历史参考：...` 横幅，注明移除 commit 与原因。
3. 保留为"再对接资产"或"一般性范式参考"，不删除。
