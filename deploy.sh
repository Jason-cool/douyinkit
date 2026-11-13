#!/usr/bin/env bash
# 一键部署：拉取最新代码 + 重新构建并启动容器。
#
# 用法（在服务器项目根目录，与 docker-compose.yml 同级执行）：
#   bash deploy.sh                          # 交互式输入 git 账号/密码
#   GIT_USER=xx GIT_PASS=xx bash deploy.sh  # 环境变量传入（无人值守）
#   FORCE=1 bash deploy.sh                  # 拉取冲突时，丢弃服务器上的代码改动、强制对齐远程
#
# 安全说明：
#   - 密码仅本次拉取鉴权使用（GIT_ASKPASS 临时脚本），不写入 .git/config、仓库或命令历史；
#   - 不要把密码硬编码进本脚本；用完建议 unset GIT_PASS / 清终端。

set -uo pipefail
cd "$(dirname "$0")" || exit 1

BRANCH="${BRANCH:-main}"

# ---- 1) 凭据 ----
if [ -z "${GIT_USER:-}" ]; then read -r -p "Git 用户名: " GIT_USER; fi
if [ -z "${GIT_PASS:-}" ]; then read -r -s -p "Git 密码: " GIT_PASS; echo; fi
[ -n "$GIT_USER" ] || { echo "! 用户名为空"; exit 1; }

# ---- 2) 拉取代码（凭据不落盘）----
ASK="$(mktemp /tmp/git-askpass.XXXXXX)"
cat > "$ASK" <<EOF
#!/bin/sh
case "\$1" in
  Username*) echo "$GIT_USER" ;;
  Password*) echo "$GIT_PASS" ;;
esac
EOF
chmod +x "$ASK"
trap 'rm -f "$ASK"' EXIT

if [ "${FORCE:-0}" = "1" ]; then
  GIT_ASKPASS="$ASK" git fetch origin "$BRANCH" || exit 1
  git reset --hard "origin/$BRANCH" || exit 1
  echo "已强制对齐 origin/$BRANCH：$(git log --oneline -1)"
else
  GIT_ASKPASS="$ASK" git pull --ff-only origin "$BRANCH" || {
    echo "! 拉取失败（服务器上代码可能有本地改动）；确认无需保留后改用 FORCE=1 重试"
    exit 1
  }
  echo "已拉取最新：$(git log --oneline -1)"
fi
unset GIT_PASS

# ---- 3) 重新构建并启动容器 ----
if docker compose version >/dev/null 2>&1; then DC="docker compose"; else DC="docker-compose"; fi
$DC up -d --build || exit 1
$DC ps

echo "---- 最近 20 行日志 ----"
$DC logs --tail=20
echo "部署完成。"
