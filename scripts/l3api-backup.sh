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

# 口令文件：**必须已存在**（2026-10-20 修正）。
#
# 原逻辑「不存在就生成新口令」是个陷阱：口令文件一旦丢失，下次跑备份会静默
# 生成一个新口令，旧备份全部变成不可解密——而运维还以为备份是连续的。
# 现在改为：缺失即报错退出（退出码 2），让人工介入决定「重新初始化备份链」
# 还是「恢复口令文件」。
#
# 初始化新备份链（首次部署 / 口令确认丢失）需显式执行：
#   L3API_BACKUP_INIT=1 l3api-backup.sh
# 它会生成新口令并把**旧备份整体移到 <OUT_DIR>/orphaned-<ts>/**（不删——
# 万一旧口令后来找回来了，那些备份还有救）。
if [[ ! -f "$PASS_FILE" ]]; then
  if [[ "${L3API_BACKUP_INIT:-0}" != "1" ]]; then
    echo "l3api-backup: 口令文件缺失：$PASS_FILE" >&2
    echo "  没有它旧备份全部不可解密。若是首次部署，用 L3API_BACKUP_INIT=1 显式初始化" >&2
    echo "  （旧备份会被移到 orphaned-<ts>/ 而非删除）；若口令文件是被误删/迁移，" >&2
    echo "  请先从备份副本恢复它再跑备份。" >&2
    exit 2
  fi
  umask 077
  openssl rand -hex 32 > "$PASS_FILE"
  chmod 600 "$PASS_FILE"
  # 旧备份隔离：新口令下它们不可读，移走而非删除（旧口令找回时仍可解密）。
  if ls "$OUT_DIR"/l3api-*.tar.gpg >/dev/null 2>&1; then
    orphan="$OUT_DIR/orphaned-$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$orphan"
    mv "$OUT_DIR"/l3api-*.tar.gpg "$orphan"/ 2>/dev/null || true
    echo "l3api-backup: 已生成新口令 $PASS_FILE —— 请立即存进密码管理器（丢失则备份不可用）" >&2
    echo "l3api-backup: 旧备份已移至 $orphan/（新口令下不可读，未删除）" >&2
  else
    echo "l3api-backup: 已生成新口令 $PASS_FILE —— 请立即存进密码管理器（丢失则备份不可用）" >&2
  fi
fi
[[ -s "$PASS_FILE" ]] || { echo "l3api-backup: 口令文件为空：$PASS_FILE" >&2; exit 2; }

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
