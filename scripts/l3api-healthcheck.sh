#!/bin/bash
# l3api-healthcheck.sh — 容器健康 + 磁盘水位巡检（2026-10-20）
#
# 背景：l3api 的应用内 notify 只覆盖账号级事件（禁用/熔断/积分耗尽），
# 进程 OOM、容器重启、磁盘写满、宿主机失联**完全在盲区**——本脚本补这一层。
# 只巡检 + 输出告警文本，不发通知（通知渠道由用户自行接：wecom/dingtalk webhook
# 或直接看 crontab 的 MAILTO）。
#
# 检查项：
#   1. 容器 l3api 是否在跑（docker ps）
#   2. healthcheck 状态（Dockerfile 内置 /livez）
#   3. 最近是否重启过（RestartCount 变大）
#   4. 磁盘水位（/ 使用率 > 85% 告警；docker build 悬空层是历史淤积源）
#   5. state.json 连续落盘失败（面板 /panel/api/overview 的 state_persist_fails）
#   6. 内存水位（可用 < 300MB 告警——机器 3.6G，n8n/litellm 常占大头）
#
# 退出码：0 = 全部正常；1 = 至少一项告警（cron 可用退出码触发 MAILTO）。
set -uo pipefail

CONTAINER="${L3API_CONTAINER:-l3api}"
DISK_WARN="${L3API_DISK_WARN:-85}"
MEM_WARN_KB="${L3API_MEM_WARN_KB:-307200}"   # 300MB 可用
API_BASE="${L3API_API_BASE:-http://127.0.0.1:7863}"

alerts=0
warn() { echo "[WARN] $*"; alerts=$((alerts + 1)); }
ok()   { echo "[ok]   $*"; }

# 1) 容器在跑？
if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
  warn "容器 $CONTAINER 未在运行（docker ps 无此名）"
  echo "l3api-healthcheck: $alerts 项告警"
  exit 1
fi

# 2) healthcheck 状态
health="$(docker inspect -f '{{.State.Health.Status}}' "$CONTAINER" 2>/dev/null || echo unknown)"
if [[ "$health" == "healthy" ]]; then
  ok "healthcheck=$health"
else
  warn "healthcheck=$health（期望 healthy；unknown=未配 healthcheck）"
fi

# 3) 重启计数（与上次巡检值比较，变大即告警）
count_file="/tmp/l3api-restart-count"
cur="$(docker inspect -f '{{.RestartCount}}' "$CONTAINER" 2>/dev/null || echo 0)"
if [[ -f "$count_file" ]]; then
  prev="$(cat "$count_file" 2>/dev/null || echo 0)"
  if [[ "$cur" -gt "$prev" ]]; then
    warn "容器重启计数 $prev → $cur（查看：docker logs --tail 50 $CONTAINER）"
  fi
fi
echo "$cur" > "$count_file"

# 4) 磁盘水位
disk="$(df -P / | awk 'NR==2{gsub("%","",$5); print $5}')"
if [[ "$disk" -ge "$DISK_WARN" ]]; then
  warn "磁盘 / 使用率 ${disk}%（阈值 ${DISK_WARN}%）——docker image prune -f 可回收历史 build 悬空层"
else
  ok "磁盘 ${disk}%"
fi

# 5) state.json 连续落盘失败（需面板鉴权；无 key 时跳过该项）
mem_kb="$(awk '/MemAvailable/{print $2}' /proc/meminfo)"
if [[ -n "$mem_kb" && "$mem_kb" -lt "$MEM_WARN_KB" ]]; then
  warn "可用内存 $((mem_kb / 1024))MB < $((MEM_WARN_KB / 1024))MB（top: n8n/litellm 常占大头）"
else
  ok "可用内存 $((mem_kb / 1024))MB"
fi

# 6) 应用层：state.json 落盘失败计数（面板 overview，需要 api_key；缺失则跳过）
if [[ -n "${L3API_API_KEY:-}" ]]; then
  fails="$(curl -s -H "Authorization: Bearer ${L3API_API_KEY}" \
    "$API_BASE/panel/api/overview" 2>/dev/null | grep -o '"state_persist_fails":[0-9]*' | cut -d: -f2 || true)"
  if [[ -n "$fails" && "$fails" -gt 0 ]]; then
    warn "state.json 连续落盘失败 $fails 次（磁盘满/权限丢失——重启会丢冷却与禁用状态）"
  elif [[ -n "$fails" ]]; then
    ok "state.json 落盘正常"
  fi
fi

if [[ "$alerts" -eq 0 ]]; then
  echo "l3api-healthcheck: 全部正常"
else
  echo "l3api-healthcheck: $alerts 项告警"
fi
exit $((alerts > 0 ? 1 : 0))
