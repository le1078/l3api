// responses.go /v1/responses OpenAI Responses API 兼容层（v1.11）。
//
// 与 anthropic.go 同构：协议翻译在 internal/protocol，HTTP 编排 + doChat 复用在
// 本文件。错误形态沿用 OpenAI error 包裹（Responses 客户端按 chat 同构解析）；
// gateway_hint 是自有字段，Responses 客户端按未知键忽略，直接并列保留。
package server

import (
	"io"
	"net/http"

	"github.com/linguo2625469/l3api/internal/protocol"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// writeResponsesErr writeOpenAIError 同形的 Responses 版（readChatBody 注入用）。
func writeResponsesErr(w http.ResponseWriter, status int, code, msg string) {
	protocol.WriteResponsesError(w, status, code, msg)
}

// responsesAPI POST /v1/responses。
func (h *Handler) responsesAPI(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readChatBody(w, r, writeResponsesErr)
	if !ok {
		return
	}
	req, chatBody, err := protocol.ParseResponsesRequest(body)
	if err != nil {
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", err.Error())
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
	// 只查白名单：RPM 在 doChat 里统一计数（此前此处双重计数，限额减半）。
	if !h.enforceModelAllowlist(w, r, cleanModel) {
		return
	}
	h.doChat(w, r, chatBody, req.Stream, cleanModel, forcePaid, newResponsesSink(cleanModel))
}

// newResponsesSink Responses 协议输出（流式翻译经 stats 观测器，记账同源）。
func newResponsesSink(reqModel string) *chatSink {
	return &chatSink{
		WriteError: writeOpenAIErrorHint, // 错误形态与 chat completions 同构
		WriteStream: func(w http.ResponseWriter, rc io.ReadCloser, stats *chatStatsReader, _ func() upstream.HintContext, _ *upstream.MarkupRepair) error {
			return protocol.WriteResponsesStream(w, stats, reqModel)
		},
		WriteFinal: func(w http.ResponseWriter, resp map[string]any) {
			writeJSON(w, http.StatusOK, protocol.ResponsesFinalFromOpenAI(resp, reqModel))
		},
	}
}
