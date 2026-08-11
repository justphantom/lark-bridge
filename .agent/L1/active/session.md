---
updated: 2026-08-11T16:55:00+08:00
---

# 会话状态

## 当前任务
deploy 运行用户 .env 注入（RUN_USER 解耦改造）—— 已实施 + 验证，**未提交**（待用户决定 commit）。

改动：`deploy/lib-common.sh`（INVOKER_USER + resolve_run_user：env > .env > 调用者，root 守卫，env_get 可选文件参）；`deploy/deploy.sh`（deploy_sudo_check 改校验 INVOKER_USER 的 `sudo -n true`，sync_env 回写 RUN_USER）；`deploy/env.example`/`deploy/README.md`（RUN_USER 文档）；`deploy/tests/smoke.sh`（+5 断言）；本地 `.env` 补 RUN_USER=dev。
验证：`bash -n` 全过、`shellcheck -S warning` 清零、`deploy-smoke` 38/38、实测 env 覆盖生效。

## 活跃文件
- deploy/lib-common.sh（INVOKER_USER/resolve_run_user/env_get，:44-:90）
- deploy/deploy.sh（deploy_sudo_check :115-:134、sync_env :673-）
- deploy/env.example（RUN_USER，:30-）
- deploy/README.md（§2 运行用户小节，:109-）
- deploy/tests/smoke.sh（RUN_USER 断言，:79-）
- .env（gitignore 本地，RUN_USER=dev）

## 决策与理由
用户选定「解耦，校验调用者」：RUN_USER（.env/环境变量/回退调用者）决定 systemd User= 与目录属主，
本身无需 sudo；部署调用者须具备免密 sudo（deploy_sudo_check 步骤 0 校验，防无 tty 部署挂起）。

## 待续步骤
- 用户确认后提交（L0#9）。
- 开放项未变：**V1/V2**（deploy-monitor 10m 作业超时可配 + unit `TimeoutStopSec=10`→~30m）；**合并脚本 M2**（3 卫星脚本抽 deploy-svc.sh）。
