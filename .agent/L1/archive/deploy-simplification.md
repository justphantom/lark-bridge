---
layer: L1
type: task
status: done
created: 2026-08-12
updated: 2026-08-14T08:30:00+08:00
---

# deploy 脚本简化 — 已完成

## 目标
简化 deploy 脚本，收口后端收敛以来的 deploy 残留：migrate_config 形态无关化、
cleanup_legacy 精简、ComponentLogLevels/SERVICES 影子数组删除。

## 用户拍板
- 对抗式分析 workflow（4 reviewer + 5 verify，22 机会 / 3 verified-safe / 2 rejected）。
- 用户选「低风险 quick wins」批次执行。

## 已交付
核心提交：`520a11c`（ComponentLogLevels + SERVICES 删除）、`7cd9996`（状态逻辑合并、
deploy-status.sh 并入 deploy.sh）、`d396df7`（build/deploy 分离）。

- **migrate_config 统一**：sed-范围删块 + 行正则删叶子双机制 → **单次 python3 pass**
  （json.load → 删顶层 blocks + 递归删 keys → 仅变更时 dump），形态无关。
- **cleanup_legacy 精简**：5 次 `systemctl list-unit-files` → 1 次；daemon-reload
  合并；state+config rm 循环 → 扁平 `rm -f`。
- **ComponentLogLevels 删除**：Go struct+field+validation+example+test 全清。
- **SERVICES 影子数组移除**：6 个读取点改为就地 `SELECTED` + `svc_unit` 派生。
- **deploy-status.sh 合并入 deploy.sh**（`7cd9996`，该文件已删除）。
- 验证：bash -n / shellcheck / deploy-smoke 33/0 / Go 全绿。

## 对抗式否决（不做）
- 删 preflight_inflight_check_legacy（404 旧前端守卫，正是过渡部署需要的）。
- HOME/PATH 移入 svc_* 表 hooks（load-bearing + 无测试网）。

## 未做的低收益项
~15 项低收益清理（helper 抽取 atomic_install/install_env_file、注释瘦身等）。
