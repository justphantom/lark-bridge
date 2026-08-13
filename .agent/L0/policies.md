---
layer: L0
version: 2
updated: 2026-08-13
---

# 流程策略

1. 所有代码改动须通过 `go test ./...` 或等价验证。
2. 修改 Feishu / Lark 相关逻辑时，须检查 `internal/feishu` 及其调用方。
3. 修改 API 或配置行为后，同步更新文档与测试。
4. 新增后端服务时，必须固定 `BackendToken` 并使用 `RunWithClient`，否则状态卡片可能出现 403。
5. 不确定实现路径时，先给出方案与利弊，待用户确认后再写代码。
6. **L2 知识同步**：修改或删除 `internal/feishufront`、`internal/feishu`、`internal/lark`、`internal/router`、`internal/config`、`deploy/` 核心逻辑后，须 `grep -rl '<符号名/路径>' .agent/L2/` 检查是否有相关 L2 条目需同步标记 `status: historical` 或更新引用。
