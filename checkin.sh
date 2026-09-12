#!/usr/bin/env bash
# checkin.sh — 签到结果表格化辅助工具（薄包装）
#
# 不调 HTTP 签到接口：cmd/signin 已自带表格输出（uid/nick/status/remain/detail
# + total=N ok=N already=N fail=N 汇总行）与 token 预刷新（NeedsRefresh(2h)），
# 比包一层 HTTP 更完善。本脚本：
#   1. 读 config.json 的端口与 api_key，调 GET /status 判网关活不死活
#      （活着只是提示"定时签到已在网关侧运行"）；
#   2. 核心动作 = 构建并执行 ./signin_bin auths，透传其已表格化的输出；
#   3. -v 模式从表格里过滤出 fail 明细（FAIL/LOAD_ERR/AUTH_INVALID，已签到不计）。
#
# 用法:
#   ./checkin.sh            # 网关状态头 + signin_bin 表格
#   ./checkin.sh -v         # 表格 + 末尾 fail 明细汇总
#   ./checkin.sh auths_dir  # 指定 auths 目录（透传给 signin_bin）
#
# 依赖: go（首次构建 signin_bin）、curl、jq（jq 缺失时 /status 头部降级为原始 JSON）
#
# 环境变量:
#   WB2A_CONFIG  配置文件路径（默认 ./config.json，读取端口与 api_key）
#   WB2A_URL     直接指定服务地址（如 http://1.2.3.4:7863），设置后忽略配置文件
set -euo pipefail
cd "$(dirname "$0")"

VERBOSE=0
AUTHS_DIR="auths"
# 参数解析：-v/-h 与可选的 auths 目录。照搬 PR #48 的严格解析风格。
while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--verbose) VERBOSE=1; shift ;;
        -h|--help)
            sed -n '2,20p' "$0"
            exit 0 ;;
        -*) echo "未知参数: $1（可用 -v / -h）" >&2; exit 2 ;;
        *) AUTHS_DIR="$1"; shift ;;
    esac
done

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

# ─── 网关状态头：GET /status 判活不死活（活着 = 定时签到已在网关侧运行）───────
# -s 静默，-w 单独取状态码；连接失败用 || true 兜住，CODE 归一为 000。
GATEWAY_OK=0
GATEWAY_INFO=""
RESP=$(curl -s -w $'\n%{http_code}' ${AUTH+"${AUTH[@]}"} "$BASE/status" || true)
if [[ -n "$RESP" ]]; then
    CODE=${RESP##*$'\n'}
    BODY=${RESP%$'\n'*}
    if [[ "$CODE" == "200" ]]; then
        GATEWAY_OK=1
        if [[ "$HAS_JQ" == "1" ]]; then
            GATEWAY_INFO=$(printf '%s' "$BODY" | jq -r \
                '"网关在线：total=\(.total) healthy=\(.healthy) cooling=\(.cooling) disabled=\(.disabled)"' 2>/dev/null || true)
        fi
        [[ -z "$GATEWAY_INFO" ]] && GATEWAY_INFO="网关在线（/status 200）"
    else
        case "$CODE" in
            401) GATEWAY_INFO="网关鉴权失败（401）：api_key 与 config.json 不一致" ;;
            000) GATEWAY_INFO="连不上 $BASE：服务未启动或端口不对" ;;
            *)   GATEWAY_INFO="网关 /status 异常（HTTP $CODE）" ;;
        esac
    fi
else
    GATEWAY_INFO="连不上 $BASE：curl 失败"
fi

echo "── 网关状态 ─────────────────────────────"
echo "$GATEWAY_INFO"
if [[ "$GATEWAY_OK" == "1" ]]; then
    echo "提示：定时签到已在网关侧运行（09/21 点）。下面跑离线签到工具即时对账。"
fi
echo

# ─── 核心：构建并执行 signin_bin（复用 signin.sh 的构建逻辑）──────────────────
BIN=./signin_bin
if [[ ! -x "$BIN" ]]; then
    echo "build signin_bin ..."
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 signin_bin（或先 ./signin.sh 预构建）" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/signin
fi

# signin_bin 已是表格输出 + 汇总行，原样透传。-v 时先落盘再抓 fail 明细追加末尾。
if [[ "$VERBOSE" == "1" ]]; then
    OUT=$("$BIN" "$AUTHS_DIR") || true
    printf '%s\n' "$OUT"
    echo
    echo "── 问题明细（仅 fail/错误，已签到不计）──"
    # signin_bin 表格行：uid | nick | status | remain | detail
    # 状态列左对齐填充（%-12s），故状态值后跟若干空格再 |。用 [[:space:]]* 容差。
    # 只挑 FAIL/LOAD_ERR/AUTH_INVALID 行（ALREADY 是幂等成功，不计）。
    printf '%s\n' "$OUT" | grep -E '\| (FAIL|LOAD_ERR|AUTH_INVALID)[[:space:]]*\|' || echo "  （无 fail）"
else
    exec "$BIN" "$AUTHS_DIR"
fi
