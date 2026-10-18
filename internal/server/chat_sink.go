// chat_sink.go 协议输出适配：/v1/chat/completions（OpenAI 原生）、/v1/messages
// （Anthropic 兼容层）、/v1/responses（OpenAI Responses API）共享同一轮转核心
// （doChat：选号/熔断/粘性/告警/用量），差异只在三类输出点的协议形态：
//
//	WriteError  错误响应（status + code + message + gateway_hint）
//	WriteStream 流式：消费上游 OpenAI SSE 帧 → 写目标协议事件
//	WriteFinal  非流式：聚合的 OpenAI 响应 → 目标协议对象
//
// OpenAI sink 是恒等实现（与抽取前行为逐字节一致）；Anthropic/Responses sink
// 见 internal/protocol 包。写法用函数字段而非接口：doChat 只有一个调用方组合，
// 函数字段让协议包不必反向依赖 server（避免循环），也便于测试内联替换。
package server

import (
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/l3api/internal/upstream"
)

// chatSink doChat 的三个协议输出点。零值字段不可用——由各协议构造函数填充。
type chatSink struct {
	WriteError func(w http.ResponseWriter, status int, code, msg, hint string)
	// WriteStream 消费上游 SSE（rc），stats 是共享的字节/usage 观测器（doChat 在
	// 其上取 TTFB/tokens 记账）。hintFn 是 OpenAI 特有的 gateway_hint 惰性求值，
	// 非 OpenAI sink 忽略。返回 *upstream.StreamFrameError（流内 error 帧）或
	// io/写失败错误，语义与 upstream.StreamHint 相同。
	// repair：DSML 原生工具调用标记修复器（nil = 关闭）。Anthropic/Responses
	// sink 的翻译器直接消费 OpenAI 帧，标记修复在它们的翻译层另行处理
	// （当前未接：标记会以普通正文透出，与修复前行为一致）。
	WriteStream func(w http.ResponseWriter, rc io.ReadCloser, stats *chatStatsReader, hintFn func() upstream.HintContext, repair *upstream.MarkupRepair) error
	// WriteFinal 写非流式响应（resp 为聚合的 OpenAI chat.completion 对象）。
	WriteFinal func(w http.ResponseWriter, resp map[string]any)
}

// newOpenAISink OpenAI 原生输出（恒等）：错误走 writeOpenAIErrorHint，流式
// 走 upstream.StreamHint 透传，非流式原样 JSON。
func newOpenAISink() *chatSink {
	return &chatSink{
		WriteError: writeOpenAIErrorHint,
		WriteStream: func(w http.ResponseWriter, rc io.ReadCloser, stats *chatStatsReader, hintFn func() upstream.HintContext, repair *upstream.MarkupRepair) error {
			return upstream.StreamHint(w, stats, upstream.FrameHintFunc(hintFn), upstream.WithMarkupRepair(repair))
		},
		WriteFinal: func(w http.ResponseWriter, resp map[string]any) {
			writeJSON(w, http.StatusOK, resp)
		},
	}
}

// readChatBody 请求体读取与 413 上限检查（三个协议端点共用；超限/读失败的
// 错误形态由调用方传入的 writeErr 决定）。ok=false 时调用方直接 return。
//
// limit<=0 = 不限（2026-10-18 起）：此时走 io.ReadAll 全量读，不做 413 判定。
// 多图/长上下文会话每轮以 base64 重发历史图片，8MB 上限把它们前置成 413；
// "多大算过大"交给上游判定（上游自己的 413/400 会正常透出并走既有错误分类）。
func (h *Handler) readChatBody(w http.ResponseWriter, r *http.Request, writeErr func(w http.ResponseWriter, status int, code, msg string)) ([]byte, bool) {
	limit := h.maxBodyBytes.Load()
	if limit <= 0 {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
			return nil, false
		}
		return body, true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return nil, false
	}
	if int64(len(body)) > limit {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过上限（%s，实测 %s）：多图/长上下文会话易触发（历史图片每轮以 base64 重发）；请压缩图片或调大 server.max_body_mb（留空或 0 = 不限，面板修改即时生效）后重试",
				humanBytes(limit), humanBytes(int64(len(body)))))
		return nil, false
	}
	return body, true
}

// humanBytes 字节数的人类可读形式（1024 进制，1024B→"1.0 KB"）。
// 413 文案专用：上限按字节存但配置按 MB，小上限（测试里的 512B、或将来按 KB 配）
// 用 "%d MB" 打印会显示成「超过 0 MB 上限」，用户完全无法自查。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	val := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for val >= unit && i < len(units)-1 {
		val /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", val, units[i])
}
