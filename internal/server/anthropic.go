// anthropic.go /v1/messages Anthropic Messages API 兼容层（v1.11）。
//
// 请求解析与响应/流式翻译在 internal/protocol；本文件只做 HTTP 编排：
// 读体（readChatBody）→ 协议转换 → 复用 doChat 轮转核心（选号/熔断/粘性/
// 告警/用量与 /v1/chat/completions 完全一致）→ Anthropic 形态输出。
//
// 错误形态：{"type":"error","error":{type,message}}；status → error type 映射
// 见 protocol.AnthropicErrType。多 key 白名单/RPM、model 前缀路由（cn:/qoder: 等）
// 全部照常生效。
package server

import (
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/protocol"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// writeAnthropicErr writeOpenAIError 同形的 Anthropic 版（readChatBody 注入用）。
func writeAnthropicErr(w http.ResponseWriter, status int, code, msg string) {
	protocol.WriteAnthropicError(w, status, protocol.AnthropicErrType(code), msg)
}

// anthropicMessages POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readChatBody(w, r, writeAnthropicErr)
	if !ok {
		return
	}
	req, chatBody, err := protocol.ParseAnthropicRequest(body)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// "@paid" 后缀（free_guard 单次强制付费放行阀）在入口剥：白名单/doChat/
	// 粘性/记账全按剥后名处理，sink 展示名同步为剥后名（响应 model 无后缀）。
	cleanModel, forcePaid := splitPaidSuffix(req.Model)
	if forcePaid {
		// 出站 body 同步改写：否则 doChat 里 bareModel==reqModel 跳过重写，
		// 上游收到带后缀的模型名 → 11102。
		chatBody = rewriteModel(chatBody, cleanModel)
	}
	// 多租户策略：白名单比对请求 model 原文（转换后的 chat body 同 model）。
	// 只查白名单：RPM 在 doChat 里统一计数（此处再查会双重计数，限额实际减半）。
	if !h.enforceModelAllowlistAnthropic(w, r, cleanModel) {
		return
	}
	h.doChat(w, r, chatBody, req.Stream, cleanModel, forcePaid, newAnthropicSink(cleanModel))
}

// enforceModelAllowlistAnthropic 只查模型白名单（错误形态 Anthropic）。
// RPM 不在此计数：本 handler 随后进 doChat，doChat 内的 enforceKeyPolicy
// 统一计一次（此前此处也计一次，导致 /v1/messages 的 RPM 双重计数、
// 限额实际减半——v1.12 修复）。429 错误形态因此为 OpenAI 外壳（doChat 内
// writeOpenAIError），属可接受的协议外壳折衷。
func (h *Handler) enforceModelAllowlistAnthropic(w http.ResponseWriter, r *http.Request, model string) bool {
	entry, ok := apikeys.FromContext(r.Context())
	if !ok {
		return true
	}
	if !entry.AllowsModel(model) {
		protocol.WriteAnthropicError(w, http.StatusForbidden, "permission_error",
			fmt.Sprintf("model %q is not allowed for api key %q", model, entry.Name))
		return false
	}
	return true
}

// newAnthropicSink Anthropic 协议输出：错误形态转换、流式翻译（经 stats 观测器
// 读取上游流，token/TTFB 记账与 OpenAI 路径同源）、非流式翻译。
// gateway_hint 是 l3api 自有诊断字段——Anthropic 形态下拼进 message 尾部
// （协议不允许未知顶层字段，并列补充语义保持）。
func newAnthropicSink(reqModel string) *chatSink {
	return &chatSink{
		WriteError: func(w http.ResponseWriter, status int, code, msg, hint string) {
			out := msg
			if hint != "" {
				out = msg + "\n(gateway_hint: " + hint + ")"
			}
			protocol.WriteAnthropicError(w, status, protocol.AnthropicErrType(code), out)
		},
		WriteStream: func(w http.ResponseWriter, rc io.ReadCloser, stats *chatStatsReader, _ func() upstream.HintContext, _ *upstream.MarkupRepair) error {
			// 从 stats（包装 rc 的观测 reader）读流：字节/usage 统计照常累积，
			// doChat 侧 recordAttempt(stats.Usage()) 不感知翻译。
			return protocol.WriteAnthropicStream(w, stats, reqModel)
		},
		WriteFinal: func(w http.ResponseWriter, resp map[string]any) {
			writeJSON(w, http.StatusOK, protocol.AnthropicFinalFromOpenAI(resp, reqModel))
		},
	}
}
