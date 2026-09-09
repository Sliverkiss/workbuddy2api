#!/bin/bash
# sync-auths.sh — 从 CPA 容器拷出 CN workbuddy auth 到 ./auths
set -e
cd /root/workbuddy2api
mkdir -p auths /tmp/wb-sync
rm -rf /tmp/wb-sync
mkdir -p /tmp/wb-sync
docker cp cpa-manager-plus-cli-proxy-api-1:/root/.cli-proxy-api/. /tmp/wb-sync/ 2>/dev/null
kept=0; skipped=0
for f in /tmp/wb-sync/workbuddy*.json; do
  [ -e "$f" ] || continue
  # domain 为空或 codebuddy.cn → CN；否则跳过（非 CN）
  dom=$(grep -o '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$f" | head -1 | sed 's/.*: *"//;s/"$//')
  case "$dom" in
    ""|*codebuddy.cn*)
      cp "$f" auths/ && kept=$((kept+1));;
    *)
      skipped=$((skipped+1));;
  esac
done
chmod 600 auths/*.json 2>/dev/null || true
echo "kept_cn=$kept skipped_non_cn=$skipped total_in_auths=$(ls auths/ | wc -l)"
