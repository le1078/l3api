# l3api

AI 模型 API 聚合网关：把多个品牌的免费/促销额度账号聚合成一个池子，对外提供统一的 OpenAI / Anthropic / Responses 三种协议端点，并自动完成各品牌的**每日签到、积分任务与保活**。

> 本项目是在 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 基础上的魔改二创，感谢原作者的开源工作。本分支的主打方向是 **CodeBuddy / Qoder / Trae 三品牌的签到自动化 + 统一 API 调用**。

## 特点

**三品牌签到与薅羊毛自动化**
- CodeBuddy（国内版 + 国际版双账号体系）、Qoder、Trae 三品牌账号池
- 定时自动签到、Token 保活、猫猫旅行、活跃上报、成长任务队列一键闭环
- 模型目录每日自动同步：探测官方目录 → diff → 新模型实测门 → 自动生效 + 变化告警
- 免费额度守护：促销模型（限时/夜间免费）额度用尽后上游静默转扣费，网关检测到实扣即跳过该账号，全部用尽返回 402 而不是悄悄花你的积分（`模型名@paid` 可强制付费）

**统一 API 网关**
- 三协议端点：`POST /v1/chat/completions`（OpenAI）、`POST /v1/messages`（Anthropic）、`POST /v1/responses`（Responses）——Codex / Claude Code / 各类客户端直接接入
- 模型名前缀路由：裸名/`cn:` → 国内版，`global:` → 国际版，`qoder:` / `trae:` → 对应品牌
- 多账号池轮询：冷却/熔断/连败降权/积分保底/会话粘性，429 与模型级错误自动退避换号
- 多 API Key：每 key 独立白名单 + RPM 限流 + 用量归因

**自带管理面板**（纯静态、零依赖构建）
- 品牌卡片墙、账号详情、健康检验、Token 用量时序（按总量/模型/品牌/Key 四种视角）
- 配置页全量热生效、API Keys 管理、运行日志查看

**运维向**
- `/metrics` Prometheus 端点（零依赖手写文本格式）
- 事件告警：Telegram / Bark / 企业微信 / 钉钉 / 通用 webhook / SMTP
- 状态本地 JSON + Upstash Redis 双写，重启可恢复

## 快速开始

```bash
# 需要 Docker；Go 1.22+ 仅本机开发需要
sh cloud-init.sh   # 检查环境 → 生成含强随机 api_key 的 config.json → 构建 → 启动 → 冒烟
```

服务监听 `:7863`（compose 默认只绑 `127.0.0.1`，公网请走 TLS 反代，样例见 [DEPLOY.md](DEPLOY.md)）。注意：`config.json` 必须先于 `docker compose up` 存在（cloud-init.sh 已处理），否则 Docker 会把挂载点建成目录导致启动失败。

- 管理面板：`http://127.0.0.1:7863/panel/`（密钥 = config.json 的 `api_key`）
- 健康探针：`/livez`（进程）/ `/healthz`（池内有可用账号才 200，给 LB 用）

客户端接入示例（OpenAI 兼容）：

```bash
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer <你的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model": "cn:glm-5.2", "messages": [{"role":"user","content":"hi"}]}'
```

## 配置

全部配置在 `config.json`，面板「配置」页可视化编辑并热生效（监听地址等少数项需重启）。关键项：

| 字段 | 说明 |
|------|------|
| `api_key` / `api_keys[]` | 网关鉴权密钥；多 key 支持白名单/限流/免费守护 |
| `schedule.*` | 签到/保活/旅行/体检/模型同步的时点 |
| `pool.*` | 在途上限、熔断阈值、冷却退避、积分保底 |
| `notify.*` | 告警渠道与冷却 |

**注意**：`config.json`、`auths/`、`data/` 含明文密钥与账号凭证，已加入 `.gitignore`，请勿提交或公开分发。

## 部署与运维

详见 [DEPLOY.md](DEPLOY.md)：TLS 反代（Caddy/Nginx，SSE 参数）、Prometheus/Grafana（compose profile）、单实例约束（多实例共享 data/ 会互相覆盖）、账号登录脚本（`login.sh` / `signin.sh` / `credit.sh`）。

## 免责声明

本项目仅供学习交流。各品牌积分/额度策略以其官方条款为准，使用本项目产生的一切后果由使用者自行承担。
