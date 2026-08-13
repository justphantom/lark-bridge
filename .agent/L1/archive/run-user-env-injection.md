---
layer: L1
type: task
status: done
created: 2026-08-11
updated: 2026-08-11T16:55:00+08:00
---

# 部署脚本：服务运行用户从 .env 注入（RUN_USER）

## 目标
服务运行用户由 `.env` 注入（systemd `User=` 与目录属主），并与部署调用者解耦。

## 用户拍板
「解耦，校验调用者」：`RUN_USER` 决定 `User=`/`chown`；`deploy_sudo_check` 改校验
真正执行内嵌 sudo 的**调用者**（`INVOKER_USER`）免密 sudo。运行用户本身无需 sudo。

## 改动
- `deploy/lib-common.sh`：`INVOKER_USER`（SUDO_USER/whoami，保留 root 禁令）；`resolve_run_user()`
  解析 `RUN_USER`：环境变量 > repo 根 `.env` > 调用者；root 守卫；`env_get` 加可选文件参数
  （冒烟测试用）。顶层赋值必须放在 `env_get` 定义之后（bash 顶层语句边读边执行）。
- `deploy/env.example`：新增 `RUN_USER=`（留空=调用者，注释说明优先级/root 禁令/解耦语义）。
- `deploy/deploy.sh`：`deploy_sudo_check` 改 `sudo -n true`（INVOKER_USER）+ 文案；`sync_env`
  回写 `RUN_USER`（与 IPC_ADDR/STATE_DIR 同源）。
- `deploy/README.md`：§2 新增「服务运行用户（RUN_USER）」小节。
- `deploy/tests/smoke.sh`：+5 断言（默认=调用者 / .env / env 覆盖 / root 守卫）。
- `.env`（gitignore，本地）：补 `RUN_USER=dev`。

## 验证
- `bash -n` 全过；`shellcheck -S warning` 清零（SC2120 行内豁免）；smoke 38/38。
- 实测：无 RUN_USER → 回退 invoker(dev)；`RUN_USER=svc` env 覆盖生效。

## 已交付
- 已提交（`3d31396` 为 RUN_USER 主体，后续清理含于 `89e29e6`）。L0#9 全绿：bash -n / shellcheck / smoke 38/38 / build/vet/test -race/lint。
- 用户已拍板并落地：RUN_USER 保持 dev；后端收敛（已移除后端的 cmd/internal/deploy 脚本全删 + config/Makefile/docs 清零）；**三项零风险权限修复**（CLI chmod 0755 / 条件 HOME=$STATE_DIR / README WORKSPACE 属主小节）。
- 开放项：合并脚本 M2（3 卫星脚本抽 deploy-svc.sh）。V1/V2 随后端收敛自然消失。
