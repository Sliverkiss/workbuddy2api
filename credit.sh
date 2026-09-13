#!/usr/bin/env bash
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制: 不存在时才编译（源码改动后手动 go build -o credit ./cmd/credit）
set -euo pipefail
cd "$(dirname "$0")"

# credit 工具:不存在才编译
CREDIT_BIN="./credit"
if [[ ! -x "$CREDIT_BIN" ]]; then
    go build -o "$CREDIT_BIN" ./cmd/credit
fi

if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
