#!/bin/bash
# l3api-backup.sh — auths/ + data/ + config.json 加密备份（2026-10-20）
#
# 背景：auths/ 是 7 个账号的登录态（trae 的 accessToken 8h 有效且无 refresh_token，
# 丢一次 = 全部人工重新抓登录），data/state.json 是冷却/禁用/成本台账，
# config.json 含 api_key 与腾讯文档 token。此前这些**零备份**。
#
# 加密口径：gpg 对称加密（AES256），口令存于 /root/.l3api-backup-passphrase
# （0600，仅 root 可读）。明文 tar 绝不落盘外发——含登录态，外传等于把 7 个
# 账号送上公网。口令文件本身丢失 = 备份不可用，故它也要有副本（见 DEPLOY.md
# 「备份与恢复」节的说明：把它打印出来/存进密码管理器）。
#
# 保留策略：最近 7 份（每日一轮即一周窗口）。文件名带日期，覆盖同一天的重跑。
set -euo pipefail

RUN_DIR="${L3API_RUN_DIR:-/root/l3api-cloud-v1.14-2026-10-12}"
OUT_DIR="${L3API_BACKUP_DIR:-/root/l3api-backups}"
PASS_FILE="${L3API_BACKUP_PASS_FILE:-/root/.l3api-backup-passphrase}"
KEEP="${L3API_BACKUP_KEEP:-7}"

ts="$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT_DIR"

# 口令文件：不存在则生成一个（32 字节随机 hex）。仅首次运行打印到 stderr 提醒保存。
if [[ ! -f "$PASS_FILE" ]]; then
  umask 077
  openssl rand -hex 32 > "$PASS_FILE"
  chmod 600 "$PASS_FILE"
  echo "l3api-backup: 已生成新口令 $PASS_FILE —— 请立即保存到密码管理器，丢失则备份不可用" >&2
fi

# 只打包「丢了就不可恢复」的东西：auths/ data/ config.json。
# 源码在 git 仓库（GitHub），不需要进备份。
tmp_tar="$(mktemp /tmp/l3api-backup.XXXXXX.tar)"
trap 'rm -f "$tmp_tar"' EXIT
tar -cf "$tmp_tar" -C "$RUN_DIR" auths data config.json 2>/dev/null || {
  # config.json 或 data/ 可能不存在（全新部署）：缺项不算失败，打包存在的部分。
  tar -cf "$tmp_tar" -C "$RUN_DIR" $(cd "$RUN_DIR" && ls -d auths data config.json 2>/dev/null)
}

out="$OUT_DIR/l3api-$ts.tar.gpg"
gpg --batch --yes --passphrase-file "$PASS_FILE" \
    --symmetric --cipher-algo AES256 -o "$out" "$tmp_tar"

# 原子性：gpg 写完才 rename 到位（上面 -o 直接写目标名，这里校验非空）。
[[ -s "$out" ]] || { echo "l3api-backup: 备份文件为空：$out" >&2; exit 1; }

# 清理：只留最近 KEEP 份 .gpg（按 mtime 排序，最旧的先删）。
ls -1t "$OUT_DIR"/l3api-*.tar.gpg 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r f; do
  rm -f "$f"
done

size="$(du -h "$out" | cut -f1)"
echo "l3api-backup: ok $out ($size), keep=$KEEP"
