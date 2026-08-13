---
layer: L2
type: incident
tags: [feishu, cardkit, card-update, race-condition, terminal-state, fingerprint]
created: 2026-08-09
verified_at: 2026-08-13
applies_to: pre-b214834
status: historical
confidence: high
---

# /send 文件卡片“回弹”问题

## 现象
用户在群里点击 `/send` 文件后，卡片先变绿“已发送”，约几秒后回弹成灰“已选择”。文件本身发送成功。

## 根因
`/send` 选文件后向后端发出两个 control，目标为同一张卡：
- `emitSelectedCard` 走 `UpdateMessageID` 延迟 goroutine，约 5s 后 PATCH“已选择”灰卡。
- `emitSendFile` 走 `reflectFileOutcome` 立即 PATCH“已发送”绿卡。

绿卡先到、灰卡后到，灰卡覆盖了绿卡。

## 修复
结果 PATCH 成功后把 `msgID` 记入 `terminalCards`（带 TTL）；延迟 goroutine 醒来、PATCH 前检查目标是否已终态，命中则自弃该 PATCH。

关键实现：
- `internal/feishufront/dispatcher.go`：`terminalCards` + `markCardTerminal` / `isCardTerminal`
- `internal/feishufront/dispatcher_file_send.go`：结果 PATCH 成功后标记终态
- `internal/feishufront/dispatcher_interactive.go`：延迟 PATCH 前检查终态

## 可复用经验

1. **飞书 read-back 会深度重写卡片 JSON**：header 剥离、markdown/div 拍平为 text、button 剥 value、action 容器拍平、元素可能乱序、同一文本可能重复渲染。发送↔读回比对必须基于**归一化后的可见文本**（去 markdown 标记 + 去重 + 排序连接），不要比对结构。
2. **没有诊断日志就没有根因**：前两轮靠猜全错；加入 want/got 指纹日志后一次命中。对平台行为未知类问题，先把对比双方原貌打进日志。
3. **“该发生的日志没发生”是关键信号**：verify 日志消失说明问题在另一条路径；把用户操作时间戳与日志时间戳逐条对齐，是定位“哪条路径干的”的最快办法。
4. **防御机制本身可能是竞态源**：`cardPatchDelay` 原本用于防点击窗口，但它改变了 PATCH 落地顺序。每加一个延迟/兜底/重试，都要问它和既有机制在同一资源上的相对时序。
5. **终态标记是处理延迟写覆盖新写的通用模式**：写者延迟不可控时，让后到写者落地前检查目标是否已被更新的写终结，比重排时序和读回判断都干净。→ 该模式在 CardKit 全量切换（`b214834`）后因延迟 PATCH 防御整体删除而**暂停使用**（见 [patterns/card-terminal-state-guard.md](../patterns/card-terminal-state-guard.md)，已标 historical）。

## 参考
- 修复提交：`36538f1`
- 诊断提交：`e171d91`、`2942fb3`、`812ea2f`
- 指纹修复：`3918a84`
