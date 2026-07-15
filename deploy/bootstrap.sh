#!/usr/bin/env bash
#
# lark-bridge 全新 Ubuntu 机器一键引导脚本。
#
# 职责（顺序执行，任一失败即中止）：
#   1. apt 安装基础工具（git / curl / build-essential / jq）
#   2. 二进制部署 Go 1.25
#   3. 二进制部署 Node 20（含 npm）
#   4. 二进制部署 Bun
#   5. npm 全局安装 opencode-ai
#   6. clone 仓库
#   7. 交互式收集飞书 App ID / App Secret，写入 .env（自动生成 IPC_SECRET）
#   8. make build 产出三进制
#   9. 自动衔接 ./deploy/deploy.sh --init，装 systemd 三服务并启动
#
# 用法：
#   ./deploy/bootstrap.sh                 # 在全新机器上直接跑
#   INSTALL_DIR=/srv ./deploy/bootstrap.sh
#
# 可选环境变量：
#   INSTALL_DIR   clone 目标父目录（默认 /opt）
#   GIT_URL       仓库地址（默认内网地址，外网机器需改）
#   GO_VERSION    默认 1.25.0
#   NODE_VERSION  默认 20.18.1
#   BUN_VERSION   默认 1.1.429
#
set -euo pipefail

# ── 可配参数 ─────────────────────────────────────────
INSTALL_DIR="${INSTALL_DIR:-/opt}"
GIT_URL="${GIT_URL:-https://github.com/justphantom/lark-bridge.git}"
GO_VERSION="${GO_VERSION:-1.25.0}"
NODE_VERSION="${NODE_VERSION:-20.18.1}"
BUN_VERSION="${BUN_VERSION:-1.1.429}"

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH=amd64; NARCH=x64;   BARCH=x64 ;;
    aarch64) ARCH=arm64; NARCH=arm64; BARCH=aarch64 ;;
    *) echo "[FAIL] 不支持的架构 $(uname -m)" >&2; exit 1 ;;
esac

# ── 颜色 ─────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail() { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# ── sudo 提示：bootstrap 需 apt + 部署，整段以非 root 运行 ──
[[ "$EUID" -eq 0 ]] && fail "请勿直接以 root 运行；脚本会在需要时自行 sudo"
info "测试 sudo 可用性（本脚本需 apt 安装与 systemd 操作）..."
sudo -n true 2>/dev/null || warn "后续可能多次提示输入 sudo 密码"

# ── 步骤 1：apt 基础工具 ─────────────────────────────
info "apt 安装基础工具（git / curl / build-essential / jq）..."
sudo apt-get update -y
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git curl ca-certificates build-essential jq

# ── 步骤 2：二进制部署 Go ────────────────────────────
install_go() {
    if command -v go >/dev/null && go version | grep -q "go1.25"; then
        info "Go 1.25 已存在，跳过"; return
    fi
    local tarball="go${GO_VERSION}.linux-${ARCH}.tar.gz"
    local url="https://go.dev/dl/$tarball"
    info "下载 $url"
    curl -fsSL "$url" -o "/tmp/$tarball"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/$tarball"
    rm -f "/tmp/$tarball"
    info "Go 装在 /usr/local/go，软链 /usr/local/bin/go"
    sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
    sudo ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}
install_go
go version | grep -q "go1.25" || fail "Go 版本不是 1.25"

# ── 步骤 3：二进制部署 Node 20（含 npm）──────────────
install_node() {
    if command -v node >/dev/null && node -v | grep -qE '^v20\.'; then
        info "Node 20 已存在，跳过"; return
    fi
    local dir="node-v${NODE_VERSION}-linux-${NARCH}"
    local tarball="$dir.tar.xz"
    local url="https://nodejs.org/dist/v${NODE_VERSION}/$tarball"
    info "下载 $url"
    curl -fsSL "$url" -o "/tmp/$tarball"
    sudo rm -rf "/usr/local/node-v${NODE_VERSION}"
    sudo tar -C /usr/local -xJf "/tmp/$tarball"
    sudo mv "/usr/local/$dir" "/usr/local/node-v${NODE_VERSION}"
    rm -f "/tmp/$tarball"
    info "Node 装在 /usr/local/node-v${NODE_VERSION}，软链 node/npm/npx"
    sudo ln -sf "/usr/local/node-v${NODE_VERSION}/bin/node" /usr/local/bin/node
    sudo ln -sf "/usr/local/node-v${NODE_VERSION}/bin/npm"  /usr/local/bin/npm
    sudo ln -sf "/usr/local/node-v${NODE_VERSION}/bin/npx"  /usr/local/bin/npx
}
install_node
node -v | grep -qE '^v20\.' || fail "Node 版本不是 20"

# ── 步骤 4：二进制部署 Bun ───────────────────────────
install_bun() {
    if command -v bun >/dev/null; then
        info "bun 已存在，跳过"; return
    fi
    local url="https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${BARCH}.zip"
    info "下载 $url"
    curl -fsSL "$url" -o /tmp/bun.zip
    if ! command -v unzip >/dev/null; then
        sudo apt-get install -y --no-install-recommends unzip >/dev/null
    fi
    rm -rf /tmp/bun-extract && mkdir -p /tmp/bun-extract
    unzip -q /tmp/bun.zip -d /tmp/bun-extract
    sudo install -m 0755 "/tmp/bun-extract/bun-linux-${BARCH}/bun" /usr/local/bin/bun
    rm -rf /tmp/bun.zip /tmp/bun-extract
}
install_bun
bun --version >/dev/null || fail "bun 不可用"

# ── 步骤 5：npm 全局安装 opencode-ai ─────────────────
info "npm 全局安装 opencode-ai..."
# 全局 bin 默认在 /usr/local/node-vX/bin，已在 PATH（软链到 /usr/local/bin）
sudo npm install -g opencode-ai
command -v opencode >/dev/null || fail "opencode 安装后不在 PATH"
info "opencode 版本：$(opencode --version 2>&1 | head -1)"

# ── 步骤 6：clone 仓库 ───────────────────────────────
info "clone $GIT_URL → $INSTALL_DIR/lark-bridge"
sudo mkdir -p "$INSTALL_DIR"
sudo chown -R "$(whoami)" "$INSTALL_DIR"
if [[ -d "$INSTALL_DIR/lark-bridge/.git" ]]; then
    warn "$INSTALL_DIR/lark-bridge 已存在，执行 git pull"
    git -C "$INSTALL_DIR/lark-bridge" pull --ff-only
else
    git clone "$GIT_URL" "$INSTALL_DIR/lark-bridge"
fi
cd "$INSTALL_DIR/lark-bridge"

# ── 步骤 7：交互收集飞书凭证，写 .env ────────────────
# .env 含密钥，不在脚本里硬编码也不落盘到日志。IPC_SECRET 自动随机生成。
info "需要飞书应用凭证（在飞书开放平台 → 应用详情可查）"
echo
read -r -p "FEISHU_APP_ID (形如 cli_xxxxxxxx): " APP_ID
read -r -s -p "FEISHU_APP_SECRET: " APP_SECRET; echo
[[ -n "$APP_ID" && -n "$APP_SECRET" ]] || fail "App ID / Secret 不能为空"

IPC_SECRET="$(openssl rand -hex 32)"
ENV_FILE="$INSTALL_DIR/lark-bridge/.env"
cat > "$ENV_FILE" <<EOF
FEISHU_APP_ID=$APP_ID
FEISHU_APP_SECRET=$APP_SECRET
IPC_SECRET=$IPC_SECRET
WORKSPACE_ROOT=/var/lib/lark-bridge
EOF
chmod 600 "$ENV_FILE"
info ".env 已写入（chmod 600），IPC_SECRET 已自动生成"

# ── 步骤 8：构建 ─────────────────────────────────────
info "make build..."
make build
[[ -x bin/lark-feishu-front  && -x bin/lark-claude-back  && -x bin/lark-opencode-back ]] \
    || fail "构建产物缺失"
info "三进制已产出：bin/lark-{feishu-front,claude-back,opencode-back}"

# ── 步骤 9：衔接 deploy.sh，装 systemd 并启动 ────────
info "衔接 deploy.sh --init（systemd 部署 + 启动三服务）..."
./deploy/deploy.sh --init

echo
info "全部完成。验证：systemctl status lark-feishu-front lark-claude-back lark-opencode-back"
info "日志：journalctl -u lark-feishu-front -f"
