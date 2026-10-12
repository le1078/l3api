// healthprobe.go 账号健康探测核心：对每个账号发一次最小真实 chat 调用，验证
// 账号 → 品牌上游链路是否健康（凭证有效、端点可达、模型可用）。
//
// 由面板「健康检验」（手动）与 scheduler 定时体检（自动）共用：
//   - 直连 upstream.Client，**绕过 pool 的一切记账**（检验不应冷却/熔断账号，
//     也不污染成功率统计与用量账本）；
//   - 每品牌一个固定的最小探测模型（便宜/快速优先）；
//   - 单账号 45s 超时（调用方控制并发）；
//   - 结果只读返回，不写任何 pool 状态。
package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// ProbeModelFor 按品牌/域选最小探测模型。空串 = 该账号无法探测（附原因）。
func ProbeModelFor(a *auth.Auth) (model, reason string) {
	switch a.Brand() {
	case "qoder":
		return "auto", "" // Qoder 客户端缺省模型（网关侧 key，最便宜的 Flash 路由）
	case "trae":
		return "glm-5.3-flashx", "" // Trae 目录里最轻的非推理模型
	default: // codebuddy（cn/global 同模型名，上游各自路由）
		return "glm-5.2", ""
	}
}

// ProbeResult 单账号探测结果（前端按 uid 对齐展示；scheduler 落盘供「上次定时体检」读取）。
type ProbeResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Brand    string `json:"brand"`
	Model    string `json:"model,omitempty"`
	OK       bool   `json:"ok"`
	Skipped  bool   `json:"skipped,omitempty"`
	Latency  int64  `json:"latency_ms,omitempty"`
	Reply    string `json:"reply,omitempty"` // 成功时回答摘要（前 80 字符）
	Err      string `json:"error,omitempty"`
}

// ProbeOne 对单账号发一次最小 chat（非流式聚合读全量）。绕过 pool 记账（见文件头）。
func ProbeOne(ctx context.Context, c *Client, a *auth.Auth) ProbeResult {
	res := ProbeResult{UID: a.UID, Nickname: a.Nickname, Brand: a.Brand()}
	model, reason := ProbeModelFor(a)
	if model == "" {
		res.Skipped = true
		res.Err = reason
		return res
	}
	res.Model = model
	reqBody, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "ping，只回答 OK"}},
		"stream":   false,
	})
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	start := time.Now()
	rc, status, respBody, err := c.ChatStreamContext(ctx, a, reqBody, "",
		ChatMeta{Model: model})
	if err != nil {
		res.Latency = time.Since(start).Milliseconds()
		if ue, ok := err.(*Error); ok {
			res.Err = ue.Kind.String() + ": " + truncateProbe(ue.Msg, 120)
		} else {
			res.Err = "transport: " + truncateProbe(err.Error(), 120)
		}
		return res
	}
	if status >= 400 {
		res.Latency = time.Since(start).Milliseconds()
		res.Err = "http " + http.StatusText(status) + ": " + truncateProbe(string(respBody), 120)
		return res
	}
	resp, aerr := Aggregate(rc)
	rc.Close()
	res.Latency = time.Since(start).Milliseconds()
	if aerr != nil {
		res.Err = "parse: " + truncateProbe(aerr.Error(), 120)
		return res
	}
	res.OK = true
	if ch, ok := resp["choices"].([]any); ok && len(ch) > 0 {
		if c0, ok := ch[0].(map[string]any); ok {
			if m, ok := c0["message"].(map[string]any); ok {
				if txt, ok := m["content"].(string); ok {
					res.Reply = truncateProbe(txt, 80)
				}
			}
		}
	}
	return res
}

// truncateProbe 截断到 n 个字符（按 rune，超长追加省略号）。
func truncateProbe(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
