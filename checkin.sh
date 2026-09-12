#!/usr/bin/env bash
# checkin.sh — 手动签到并把结果排成人类可读的表格
#
# 用法:
#   ./checkin.sh              # 表格输出（默认）
#   ./checkin.sh -json        # 原始 JSON（脚本/管道用）
#   ./checkin.sh -v           # 表格 + 问题明细
#
# 依赖: curl、jq（jq 缺失时自动回退为原始 JSON）
#
# 环境变量:
#   WB2A_CONFIG  配置文件路径（默认 ./config.json，用于读取端口与 api_key）
#   WB2A_URL     直接指定服务地址（如 http://1.2.3.4:7863），设置后忽略配置文件
set -euo pipefail
cd "$(dirname "$0")"

MODE=table
VERBOSE=0
case "${1:-}" in
    "") ;;
    -json|--json) MODE=json ;;
    -v|--verbose) VERBOSE=1 ;;
    *) echo "未知参数: $1（可用 -json / -v）" >&2; exit 2 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
    echo "需要 curl" >&2
    exit 1
fi
HAS_JQ=1
command -v jq >/dev/null 2>&1 || HAS_JQ=0

# ─── 解析服务地址与密钥：优先 WB2A_URL，其次 config.json ──────────────────────
CFG=${WB2A_CONFIG:-config.json}
PORT=7863
KEY=""
if [[ -f "$CFG" ]] && [[ "$HAS_JQ" == "1" ]]; then
    LISTEN=$(jq -r '.listen // ":7863"' "$CFG")
    PORT=${LISTEN##*:}
    KEY=$(jq -r '.api_key // ""' "$CFG")
fi
PORT=${PORT:-7863}
BASE=${WB2A_URL:-http://localhost:$PORT}

AUTH=()
[[ -n "$KEY" ]] && AUTH=(-H "Authorization: Bearer $KEY")

# ─── 调用签到接口（-s 静默，-w 单独取状态码，便于区分 HTTP 错误与签到结果）───
# curl 连接失败时本身返回非 0，须用 || true 兜住（否则 set -e 会让脚本静默退出，
# 打不出下面那句人话提示）；失败时 RESP 为空，CODE 归一为 000。
RESP=$(curl -s -w $'\n%{http_code}' -X POST "$BASE/checkin" ${AUTH+"${AUTH[@]}"} || true)
if [[ -z "$RESP" ]]; then
    RESP=$'\n000'
fi
CODE=${RESP##*$'\n'}
BODY=${RESP%$'\n'*}

if [[ "$CODE" != "200" ]]; then
    MSG=$BODY
    if [[ "$HAS_JQ" == "1" ]]; then
        MSG=$(printf '%s' "$BODY" | jq -r '.error.message // .' 2>/dev/null || printf '%s' "$BODY")
    fi
    case "$CODE" in
        401) echo "鉴权失败（401）：api_key 与 config.json 不一致" >&2 ;;
        409) echo "已有签到正在执行（409）：稍后重试即可" >&2 ;;
        405) echo "服务版本过旧（405）：镜像不含 /checkin，请 docker compose up -d --build" >&2 ;;
        503) echo "签到接口未启用（503）" >&2 ;;
        000) echo "连不上 $BASE：服务未启动或端口不对" >&2 ;;
        *)   echo "请求失败（HTTP $CODE）" >&2 ;;
    esac
    printf '%s\n' "$MSG" >&2
    exit 1
fi

if [[ "$MODE" == "json" ]] || [[ "$HAS_JQ" == "0" ]]; then
    printf '%s\n' "$BODY"
    exit 0
fi

# ─── 表格渲染 ───────────────────────────────────────────────────────────────
# 中文/emoji 按 2 格宽计算（column -t 按字节补齐会错位，故自己算宽度）。
printf '%s' "$BODY" | jq -r '
def w: explode | map(
      if (. >= 4352   and . <= 44415)   # CJK 符号与标点
      or (. >= 11904  and . <= 55215)   # 汉字、假名、韩文
      or (. >= 63744  and . <= 64255)   # CJK 兼容
      or (. >= 65072  and . <= 65103)   # CJK 兼容形式
      or (. >= 65280  and . <= 65519)   # 全角形式
      or (. >= 127744 and . <= 129791)  # emoji
      then 2 else 1 end) | add // 0;
def pad($n): . as $s | $s + (" " * ([($n - ($s | w)), 0] | max));
# 状态用中文，一眼能看懂；未识别的状态原样输出，避免静默吞掉新状态。
def zh: if . == "ok" then "成功" elif . == "already" then "已签到"
        elif . == "skipped" then "跳过" elif . == "fail" then "失败"
        else . end;
["状态", "账号", "积分", "UID"] as $h
| [.accounts[] | {
      s: (.status | zh),
      n: ((.nickname // "") | if . == "" then "-" else . end),
      c: ((.credits // 0) | tostring),
      u: (.uid[0:8])
  }] as $r
| (([$r[].s | w] + [$h[0] | w] | max) + 2) as $w1
| (([$r[].n | w] + [$h[1] | w] | max) + 2) as $w2
| (([$r[].c | w] + [$h[2] | w] | max) + 2) as $w3
| ($h | [(.[0] | pad($w1)), (.[1] | pad($w2)), (.[2] | pad($w3)), .[3]] | join("")),
  ($r[] | [(.s | pad($w1)), (.n | pad($w2)), (.c | pad($w3)), .u] | join("")),
  "",
  ("合计 \(.total)  成功 \(.ok)  已签到 \(.already)  失败 \(.failed)  跳过 \(.skipped)")
'

# ─── -v：只补充真正有问题的账号明细 ─────────────────────────────────────────
# "已签到"是幂等成功，即便上游返回 400 报文也不算问题，一律不展示。
if [[ "$VERBOSE" == "1" ]]; then
    DETAILS=$(printf '%s' "$BODY" | jq -r '
      .accounts[]
      | select(.status != "already" and .status != "ok" and (.detail // "") != "")
      | "  ! \(.nickname // "-") (\(.uid[0:8])) [\(.status)]: \(.detail)"')
    if [[ -n "$DETAILS" ]]; then
        printf '\n问题明细:\n%s\n' "$DETAILS"
    fi
fi
