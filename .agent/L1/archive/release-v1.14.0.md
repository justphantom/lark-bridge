---
layer: L1
type: task
created: 2026-08-10T11:30:00+08:00
status: done
archived: 2026-08-10
---

# 发版前评估（v1.13.0 → v1.14.0）

## 结论
- 构建/测试/lint 全绿（1438 passed，0 issues），工作区干净。
- 23 commits 未发版，含 feat（agnes-back、视频发送、SendControl 重试）→ 升 minor → v1.14.0。

## 待完善
1. CHANGELOG [Unreleased] 为空，需对照 v1.13.0..HEAD 逐条补记（含 breaking：schema 2.0 全量切换 b214834；P0：e223940 卡片回调）。
2. ARCHITECTURE.md 头部 v1.13.0/2026-08-09 过期，需刷新版本+统计。
3. deploy/README.md 需核对 agnes-back 新配置项；发版顺序提示含新后端。
4. 风险：form_submit 未实测；agnes CDN TLS 偶发 reset（建议入 Notes）。

## 进展
- 2026-08-10 完成评估；CHANGELOG [Unreleased] 已起草写入（16/23 code commits，breaking/P0/P1 均显式标注）；ARCHITECTURE.md 头部+§1.3+附录已刷新（316 文件/66,950 行/26 包）。
- 剩余：CHANGELOG 切割 prep 提交 → annotated tag v1.14.0 → push origin main --tags → 按序部署。
