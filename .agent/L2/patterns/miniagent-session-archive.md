---
layer: L2
type: pattern
tags: [miniagent, session, jsonl, sidecar, history, usage]
created: 2026-08-09
confidence: high
---

# Miniagent 会话 JSONL 解析与清理

## 文件结构
miniagent 会话文件位于 `~/.miniagent/.sessions/<id>.jsonl`，首行为 session 头，后续为 message 行。

```
首行: {"type":"session","id":"...","model":"...","workdir":"...","provider":"...","created":"..."}
user:      {"type":"message","role":"user","content":"...","ts":<ms>}
assistant: {"type":"message","role":"assistant","content":"...","reasoning":"...","tool_calls":[...],"usage":{...},"ts":<ms>}
tool:      {"type":"message","role":"tool","tool_call_id":"call_<hex>","content":"...","ts":<ms>}
```

## Sidecar 机制
- 工具输出超过阈值（约 2000 字符）时，完整输出写入：
  `~/.miniagent/.sessions/<id>.tool-output/tool_{step}_call_{hex32}_{seq}.txt`
- jsonl 里的 tool 消息 `content` 尾部带引用：
  `…[完整输出已保存：/path/to/xxx.txt；用 read(offset/limit) 或 grep 回读，勿整文件 read]`
- 提取正则：`完整输出已保存：([^\s；]+\.txt)`
- `step` 仅用于 miniagent 内部去重；bridge 配对应始终用 `tool_call_id`。

## Tool 输出三种形态
| 形态 | 判据 | sidecar |
|---|---|---|
| 完整输出 | 长度低于阈值、无引用后缀 | 无 |
| 截断摘要 | 尾部含 sidecar 引用 | 有 |
| 空串 | `content == ""` | 无（多为 shell 成功无回显）|

## 解析规范

1. **轮次切分**：每个 `role=user` 行开启新一轮；到下一个 `role=user` 之间的 assistant/tool 行归属该轮。
2. **工具配对**：一条 assistant 可含多个 `tool_calls`，结果 tool 消息按 `tool_call_id` 配对（并行返回顺序不定）。
3. **时间戳**：`ts` 为毫秒级 Unix 时间戳；`session.created` 为 RFC3339。跨会话排序用 `session.created` 更稳。
4. **并发读取**：JSONL 是整行追加，逐行读不会读到半行；`/history` 只渲染到最后一个完整轮次。

## bridge 侧应做能力
1. **感知 `session.dir`**：默认探测 `~/.miniagent/.sessions`。
2. **`/new` 清理 orphan**：删除 `.id` 映射的同时，删除对应 `.jsonl` 与 `.tool-output/`（best-effort）。
3. **`/history`**：按轮次展示最近 N 轮摘要。
4. **`/usage`**：聚合 `assistant.usage` 的输入/输出 token。
5. **`/search`**：在 user/assistant/tool 内容中子串匹配。

## 参考
- 分析文档：`docs/miniagent-session-archive-optimization.md`
