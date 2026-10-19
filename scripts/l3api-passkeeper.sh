#!/bin/bash
# l3api-passkeeper.sh — 备份口令的多副本守护（2026-10-20）
#
# 背景：备份口令文件 /root/.l3api-backup-passphrase 是**单点**——它丢了，
# 所有加密备份（auths/ 登录态）立刻变成不可解密，而 trae token 8h 无 refresh，
# 丢了等于 7 个账号全人工重登。本脚本把它复制到几个「不同死法」的位置：
#
#   主副本  /root/.l3api-backup-passphrase        （备份脚本直接用，0600）
#   守护副1 /root/.l3api-secrets/passphrase.bak   （独立目录，0600）
#   守护副2 /etc/l3api-backup-passphrase          （etc 下，root 0600）
#   指纹    /var/log/l3api-passkeeper.log         （每次巡检记 md5，变更可发现）
#
# 语义（关键）：
#   - 主副本存在 → 把内容同步到两个副副本（覆盖）。副副本只作恢复源，
#     绝不反向「生成」口令——避免副副本被污染后覆盖主副本。
#   - 主副本缺失 → 若恰有**一个**副副本存在则从它恢复主副本（打日志）；
#     多个副副本内容不一致 → 不动，只报警（人工裁定，不猜）。
#   - 全缺失 → 报警。此时备份脚本会拒绝运行（退出码 2），不会静默开新链。
#
# cron 每日 04:00 跑（备份 03:30 之后）：既做日常同步，也做「主副本被误删」的
# 自动恢复。
set -uo pipefail

PASS="${L3API_BACKUP_PASS_FILE:-/root/.l3api-backup-passphrase}"
BAK1="/root/.l3api-secrets/passphrase.bak"
BAK2="/etc/l3api-backup-passphrase"
LOG="/var/log/l3api-passkeeper.log"

log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

# collect 收集存在的副本内容（文件:内容 形式，用于比对）。
existing=()
for f in "$PASS" "$BAK1" "$BAK2"; do
  [[ -s "$f" ]] && existing+=("$f")
done

if [[ ${#existing[@]} -eq 0 ]]; then
  log "ALERT: 三个副本全部缺失——备份口令已丢，加密备份不可解密。需人工决定是否重开备份链（L3API_BACKUP_INIT=1）"
  exit 1
fi

# 主副本存在 → 同步到副副本（覆盖式）。
if [[ -s "$PASS" ]]; then
  install -m 0600 -D "$PASS" "$BAK1" 2>/dev/null || { install -m 0600 "$PASS" "$BAK1"; }
  install -m 0600 -D "$PASS" "$BAK2" 2>/dev/null || { install -m 0600 "$PASS" "$BAK2"; }
  log "ok: 主副本同步到 2 个副副本 (md5=$(md5sum "$PASS" | cut -d' ' -f1))"
  exit 0
fi

# 主副本缺失 → 从副副本恢复（仅当副副本之间内容一致）。
h1="$(md5sum "$BAK1" 2>/dev/null | cut -d' ' -f1)"
h2="$(md5sum "$BAK2" 2>/dev/null | cut -d' ' -f1)"
if [[ -n "$h1" && -n "$h2" && "$h1" != "$h2" ]]; then
  log "ALERT: 主副本缺失且两个副副本内容不一致（$h1 vs $h2）——不猜，人工裁定后写入 $PASS"
  exit 1
fi
src="$BAK1"; [[ -s "$src" ]] || src="$BAK2"
install -m 0600 -D "$src" "$PASS" 2>/dev/null || install -m 0600 "$src" "$PASS"
log "RECOVER: 主副本缺失，已从 $src 恢复 (md5=$(md5sum "$PASS" | cut -d' ' -f1))"
exit 0
