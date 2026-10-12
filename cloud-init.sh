#!/bin/sh
# l3api 云机一键初始化：权限修正 → 构建镜像 → 启动 → 冒烟。
# 用法：把整个部署目录上传到云机后，进入目录执行 sh cloud-init.sh
set -e
cd "$(dirname "$0")"

echo "==> [1/5] 环境检查"
if ! command -v docker >/dev/null 2>&1; then
  echo "错误：未检测到 docker。请先安装 Docker（https://docs.docker.com/engine/install/）。" >&2
  exit 1
fi
if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  echo "错误：未检测到 docker compose 插件。请安装 docker-compose-plugin。" >&2
  exit 1
fi
echo "    docker: $(docker --version)"
echo "    compose: $COMPOSE"

echo "==> [2/5] 校验必要文件"
for f in Dockerfile docker-compose.yml config.json go.mod; do
  [ -f "$f" ] || { echo "错误：缺少 $f（部署包不完整）" >&2; exit 1; }
done
if grep -q '"api_key": ""' config.json; then
  echo "错误：config.json 的 api_key 为空（公网裸奔）。请填入强随机值：" >&2
  echo "  openssl rand -hex 24" >&2
  exit 1
fi
echo "    config.json: api_key 已设置"

echo "==> [3/5] 修正数据目录属主（容器以 uid 10001 运行）"
mkdir -p auths data
if [ "$(id -u)" = "0" ]; then
  chown -R 10001:10001 auths data
  chown 10001:10001 config.json
else
  sudo chown -R 10001:10001 auths data 2>/dev/null || \
    echo "    提示：无 sudo 权限时可用 PUID=$(id -u) PGID=$(id -g) $COMPOSE up -d 以当前用户运行"
fi

echo "==> [4/5] 构建并启动（构建内含 go vet + go test 质量门，首次约 3-8 分钟）"
$COMPOSE up -d --build

echo "==> [5/5] 冒烟"
sleep 6
KEY=$(sed -n 's/.*"api_key": *"\([^"]*\)".*/\1/p' config.json | head -1)
ok() { echo "    ✔ $1"; }
fail() { echo "    ✘ $1" >&2; FAIL=1; }

curl -sf --max-time 5 http://127.0.0.1:7863/livez >/dev/null && ok "/livez 200" || fail "/livez 不通"
curl -s --max-time 5 -o /dev/null -w "%{http_code}" http://127.0.0.1:7863/healthz | grep -qE "200|503" \
  && ok "/healthz 可达（503=暂无健康账号，属正常）" || fail "/healthz 不通"
curl -sf --max-time 5 -H "Authorization: Bearer $KEY" http://127.0.0.1:7863/panel/api/overview >/dev/null \
  && ok "面板 API（带鉴权）200" || fail "面板 API 鉴权失败（检查 api_key）"

echo
if [ -n "$FAIL" ]; then
  echo "冒烟未全部通过，日志：$COMPOSE logs --tail 50" >&2
  exit 1
fi
echo "全部通过 ✅"
echo "  面板: http://127.0.0.1:7863/panel/  （仅回环可达；公网请走 TLS 反代）"
echo "  日志: $COMPOSE logs -f"
echo "  下一步：配置 Nginx/Caddy TLS 反代（SSE 参数样例见部署包内 README 或项目 README「云部署要点」）"
