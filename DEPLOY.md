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
`l3api_chat_attempts_total{realm,outcome}`、`l3api_chat_request_duration_seconds`、
`l3api_pool_accounts{brand,state}`、`l3api_pool_in_flight`、`l3api_sticky_sessions`。
数据保留 15 天（约 200~400MB），落在 `data/prometheus/`。

**安全边界（必读）**：`/metrics` 无鉴权，公网反代必须拦截——

```nginx
# Nginx 在 location / 之前加：
location = /metrics { return 403; }
```
```
# Caddy 在站点块内加：
respond /metrics 403
```

## 关键提醒

- **api_key 已生成**：`sk-0ecb...6edf`（完整值在 `config.json` 内）。客户端/面板登录都用它。
- **单实例运行**：不要在多台机器/多个容器挂同一份 `data/`（无跨进程文件锁）。
- **config.json 是单文件 bind mount**：面板保存配置/管理 key 时无法对挂载点做
  rename 原子替换（内核 EBUSY），程序已自动回退为原地写（功能正常，仅失去崩溃
  瞬间的写原子性）。要完整原子性可改为挂载目录（`./config:/app/config` 并把
  `config_path` 指向 `/app/config/config.json`）。
- **TZ=Asia/Shanghai 已在 compose 里设好**，影响签到/冷却重置时刻，勿删。
- 备份：定期打包 `auths/` + `data/` 异地存放（含明文 token，注意加密）。
- 面板仅回环可达；公网访问面板走反代后可加 IP 白名单/Basic Auth 再加一层。
