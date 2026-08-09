---
layer: L0
version: 1
updated: 2026-08-09
---

# 流程策略

1. 所有代码改动须通过 `go test ./...` 或等价验证。
2. 修改 Feishu / Lark 相关逻辑时，须检查 `internal/pkg/feishu` 及其调用方。
3. 修改 API 或配置行为后，同步更新文档与测试。
4. 新增后端服务时，必须固定 `BackendToken` 并使用 `RunWithClient`，否则状态卡片可能出现 403。
5. 不确定实现路径时，先给出方案与利弊，待用户确认后再写代码。
