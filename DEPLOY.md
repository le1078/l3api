# l3api 云机部署说明（2026-09-24 打包）

## 包内容

| 路径 | 说明 |
|------|------|
| `Dockerfile` / `docker-compose.yml` | 镜像构建（内嵌 go vet + go test 质量门）与编排（日志轮转、512M 内存限额、127.0.0.1 回环端口） |
| `config.json` | **已生产化**：强随机 api_key、回环监听。含全部调度/池参数 |
| `auths/` | 账号凭证（qoder + codebuddy 两个真实账号 token） |
| `data/` | 池状态 / 用量 / 模型缓存 |
| `cloud-init.sh` | 云机一键初始化脚本 |
| `go.mod` / `go.sum` / `cmd/` / `internal/` | 完整源码（镜像构建用） |

## 云机操作（3 步）

```bash
# 1. 上传并解压（任选 scp/rsync/宝塔面板上传）
tar xzf l3api-cloud-*.tar.gz && cd l3api-cloud-*

# 2. 一键初始化（检查环境 → chown → 构建 → 启动 → 冒烟）
sh cloud-init.sh

# 3. 配 TLS 反代（SSE 参数照抄下面）
```

## TLS 反代（必做，服务本身只有明文 HTTP）

**Caddy（推荐，自动证书）** — `/etc/caddy/Caddyfile`：
```
l3api.example.com {
    reverse_proxy 127.0.0.1:7863
}
```

**Nginx** — 关键三条（缺第一条流式必挂）：
```nginx
location = /metrics { return 403; }   # /metrics 不鉴权，公网必须拦截（v1.11）
location / {
    proxy_pass http://127.0.0.1:7863;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;      # SSE 必须
    proxy_read_timeout 600s;  # 默认 60s 会掐断长流式
    proxy_send_timeout 600s;
}
```

## API 协议面（v1.11）

| 端点 | 协议 | 典型客户端 |
|------|------|-----------|
| `POST /v1/chat/completions` | OpenAI Chat | 绝大多数工具（原有） |
| `POST /v1/messages` | **Anthropic Messages** | Claude Code、Cline 等 |
| `POST /v1/responses` | **OpenAI Responses** | Codex、新版 SDK、n8n Responses 节点 |
| `GET /v1/models` | OpenAI | 全部 |

`/v1/messages` 按客户端配置：Base URL `https://<域名>`（或回环直连时
`http://127.0.0.1:7863`）、API Key 同网关密钥、模型名带前缀（如 `cn:glm-5.2`、
`qoder:gmodel`、`trae:glm-5.3`）。

## 多租户 API Keys（v1.11）

`config.json` 新增 `api_keys` 数组，每把 key 独立名称/白名单/限速：

```json
"api_keys": [
  {"name": "alice", "key": "sk-xxxx", "models": ["cn:glm-5.2", "glm-5.2"], "rpm": 60},
  {"name": "bob",   "key": "sk-yyyy", "disabled": true}
]
```

- 旧 `api_key` 继续有效（自动合成为 name=`default` 条目），零回归；
- `models` 白名单空 = 不限；`rpm` 0 = 不限（超限 429 带 Retry-After）；
- 面板「配置」页可视化增删改（即存即用），用量页按 key 归因拆分。

### 免费额度守护（v1.15，`free_guard`）

workbuddy 部分模型在促销窗口内免费（「限时免费」「夜间免费」等），但免费**有额度**，
额度用完后上游不报错、静默转扣积分。给 key 加 `free_guard: true` 后：

- 模型处于生效的免费促销窗口时，若某账号实测 `usage.credit > 0`（额度已尽），
  该账号被跳过（台账持久化，复位点 = 促销窗口结束/次日 0 点，过后自动重试）；
- 所有账号额度都尽 → 请求直接报错 `402 free_quota_exhausted`，**不花积分**；
- 额度尽后想继续花积分：模型名加 `@paid` 后缀（如 `hy3@paid`）本次强制付费；
- 守护只在免费促销窗口内生效；窗口外（无促销/促销过期）照常计费，零回归。

```json
"api_keys": [
  {"name": "freeonly", "key": "sk-zzzz", "free_guard": true}
]
```

检测是按账号的事后实测（上游不下发剩余额度），每账号每模型每窗口最多漏一单。
额度用尽时推送 `free_quota_exhausted` 告警事件（notify.channels 订阅）。

## 事件告警通知（v1.11）

`config.json` 新增 `notify` 段：账号被禁用/熔断/积分耗尽/签到失败/余额偏低/
池耗尽时推送到外部渠道（单机零依赖，Post 非阻塞不拖慢业务）：

```json
"notify": {
  "enabled": true,
  "cooldown": "30m",
  "balance_low_credits": 1000,
  "channels": [
    {"type": "telegram", "url": "https://api.telegram.org/bot<TOKEN>/sendMessage", "secret": "<chat_id>"},
    {"type": "bark",     "url": "https://api.day.app/<device_key>"},
    {"type": "wecom",    "url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=..."},
    {"type": "dingtalk", "url": "https://oapi.dingtalk.com/robot/send?access_token=...", "secret": "<加签密钥，可选>"},
    {"type": "generic",  "url": "https://example.com/hook"}
  ]
}
```

手动在面板禁用账号不告警（用户自己点的）；`cooldown` 内同 (事件,账号) 只发一条。

## Prometheus 监控（方案 A：同机部署）

`docker-compose.yml` 已内置 `prometheus` 服务（Grafana 可选，`--profile grafana`）。
l3api 新增 `GET /metrics`（**不鉴权**，Prometheus 免配 token）：

```bash
sh cloud-init.sh          # 构建 + 启动（会一并起 prometheus）
docker compose --profile grafana up -d   # 需要图表时再启动 Grafana
curl 127.0.0.1:9090/api/v1/targets | grep -o l3api   # 验证抓取 UP
```

指标面：`l3api_chat_requests_total{model,realm,status,mode}`、
`l3api_chat_attempts_total{realm,outcome}`、
`l3api_chat_upstream_errors_total{kind,status}`（v1.17 新增：429/5xx/超时/内容拦截分维）、
`l3api_chat_pick_failures_total{realm,reason}`（v1.17 新增：选号失败归因——
reason ∈ cooling/breaker/degrade/in_flight_full/free_exhaust/model_cooldown/credit_floor/realm_mismatch）、
`l3api_chat_request_duration_seconds`、`l3api_pool_accounts{brand,state}`、
`l3api_pool_in_flight`、`l3api_sticky_sessions`、`l3api_goroutines`。
数据保留 15 天（约 200~400MB），落在 `data/prometheus/`。

**安全边界（必读）**：`/metrics` 无鉴权，公网反代必须拦截——
若反代把 l3api 挂在**子路径**下（如 `/l3api/`），拦截规则要用**精确匹配**
（优先级高于前缀匹配，且子路径前缀形态同样要拦）：

```nginx
# Nginx：精确匹配优先级高于 ^~ /l3api/，放在它前面即可
location = /l3api/metrics { return 403; }
```
```
# Caddy 在站点块内加：
respond /l3api/metrics 403
```

2026-10-20 实测：子路径部署（`location ^~ /l3api/` 整块反代）时只写了
`location = /metrics`（裸路径口径）是**拦不住的**——`/l3api/metrics`
照样 200 全量暴露（模型名/成功率/池规模/账号维度）。

## 备份与恢复（v1.17）

`auths/`（7 个账号登录态，trae 的 accessToken 8h 有效且**无 refresh_token**，
丢一次 = 全部人工重新抓登录）、`data/state.json`（冷却/禁用/成本台账）、
`config.json`（api_key + 腾讯文档 token）**没有自动备份就等于没有备份**。
仓库内 `scripts/` 提供两个运维脚本（已装到云机 `/root/l3api-ops/` 并注册 cron）：

```bash
# 每日加密备份（gpg AES256 对称加密；口令文件 0600；保留最近 7 份）
30 3 * * * /root/l3api-ops/l3api-backup.sh >> /var/log/l3api-backup.log 2>&1

# 容器健康 + 磁盘/内存水位 + state.json 落盘失败巡检（异常退出码 1）
*/15 * * * * /root/l3api-ops/l3api-healthcheck.sh >> /var/log/l3api-healthcheck.log 2>&1
```

备份口径：`tar` 只打 `auths/ data/ config.json`（源码在 git 仓库，不进备份），
gpg 加密后落 `/root/l3api-backups/l3api-<ts>.tar.gpg`。
**口令文件 `/root/.l3api-backup-passphrase` 丢失 = 备份不可用**——首次运行时会
打印提醒，请立即存进密码管理器。

恢复：

```bash
gpg --passphrase-file /root/.l3api-backup-passphrase -d l3api-<ts>.tar.gpg | tar xv -C <运行目录>
```

健康巡检覆盖应用内 notify 到不了的盲区：容器存活 / healthcheck 状态 /
**重启计数变化**（上次巡检值在 `/tmp/l3api-restart-count`）/ 磁盘水位（≥85%）/
可用内存（<300MB）/ `state.json` 连续落盘失败（面板 overview 的
`state_persist_fails`，需 `L3API_API_KEY` 环境变量才查）。

## 关键提醒

- **api_key 已生成**：`sk-0ecb...6edf`（完整值在 `config.json` 内）。客户端/面板登录都用它。
- **单实例运行**：不要在多台机器/多个容器挂同一份 `data/`（无跨进程文件锁）。
- **config.json 是单文件 bind mount**：面板保存配置/管理 key 时无法对挂载点做
  rename 原子替换（内核 EBUSY），程序已自动回退为原地写（功能正常，仅失去崩溃
  瞬间的写原子性）。要完整原子性可改为挂载目录（`./config:/app/config` 并把
  `config_path` 指向 `/app/config/config.json`）。
- **TZ=Asia/Shanghai 已在 compose 里设好**，影响签到/冷却重置时刻，勿删。
  另：v1.17 起 main 内嵌 `time/tzdata`（+450KB），促销时段按上游
  `schedule.timezone` 解析时区不再依赖镜像里的 `/usr/share/zoneinfo`。
- 备份：见上一节（`scripts/l3api-backup.sh`，加密 + cron 已就位）。
- 面板仅回环可达；公网访问面板走反代后可加 IP 白名单/Basic Auth 再加一层。

## v1.17 新增配置项

| 键 | 缺省 | 说明 |
|----|------|------|
| `upstream.stream_total_timeout_seconds` | 1800 | 聊天 SSE 流总时长硬顶。与 `idle_timeout_seconds`（管静默多久）分工：本项管总共多久——上游 keepalive 拖流时 idle 永不触发，流会无限期占用账号在途名额（`max_in_flight=8`，几条这样的流就把池子打满）。显式 `0` = 禁用硬顶；负数归一为禁用。 |
| `upstream.header_timeout_seconds` | 60 | 首字节前（响应头）超时。**v1.17 改为独立兜底 60s**（原回落 `timeout_seconds=120`，比 transport 选的 60s 慢一倍，MaxRotate=3 最坏轮转从 3min 变 6min）。 |

环境变量：`WB2A_STREAM_TOTAL_TIMEOUT_SECONDS`（对应 `stream_total_timeout_seconds`）。

