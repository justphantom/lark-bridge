---
layer: L1
type: task
status: in_progress
created: 2026-08-12
updated: 2026-08-13T17:36:00+08:00
---

# deploy 脚本简化

## 目标
简化 deploy 脚本，收口后端收敛以来的 deploy 残留：migrate_config 形态无关化、
cleanup_legacy 精简、ComponentLogLevels/SERVICES 影子数组删除。

## 用户拍板
- 对抗式分析 workflow（4 reviewer + 5 verify，22 机会 / 3 verified-safe / 2 rejected）。
- 用户选「低风险 quick wins」批次执行。

## 已完成
- **migrate_config 统一**（deploy-status.sh）：sed-范围删块（缩进脆弱）+ 行正则删叶子
  双机制 → **单次 python3 pass**（json.load → 删顶层 blocks 任意形态 + 递归删 keys
  → 仅变更时 dump）。形态无关，且 json.load 对损坏 config fail-fast。
  ⚠️ 解锁 `ComponentLogLevels` 删除。
- **cleanup_legacy 精简**（deploy.sh）：5 次 `systemctl list-unit-files` → 1 次
  （捕获后 per-unit grep）；daemon-reload 从每匹配 1 次 → 任意匹配后 1 次；
  state+config 两个 rm 循环 → 扁平 `rm -f`。
- **ComponentLogLevels 删除**：删 Go struct+field+validation+example+test。
  零 dormant config 字段剩余——后端收敛以来的 config 清零完成。
- **SERVICES 影子数组移除**：删 `SERVICES` 全局 + `rebuild_services`；6 个读取点
  改为就地 `SELECTED` + `svc_unit` 派生；`drop_service` 简化为纯 SELECTED 过滤。
  smoke.sh 同步更新（34→33 断言）。
- **杂项**：svc_* 表注释更正（去掉虚假「零接触扩展」承诺）；init_status 删死掉的
  `claude` 块迭代；3 处过期注释。
- 验证：bash -n / shellcheck / deploy-smoke 33/0 / Go 全绿。净 −6 LOC
  （migrate_config 是健壮性收益非 LOC，cleanup_legacy 缩）。

## 对抗式否决（不做）
- 删 preflight_inflight_check_legacy（404 旧前端守卫，正是过渡部署需要的）。
- HOME/PATH 移入 svc_* 表 hooks（load-bearing + 无测试网）。

## 待续
- ~15 项低收益清理（helper 抽取 atomic_install/install_env_file、注释瘦身等）。
- **3 文件未提交**：deploy.sh / deploy-status.sh / lib-common.sh。
