// anthropic_stream.go Anthropic 流式适配器：OpenAI chat chunk SSE →
// Anthropic message 事件序列（message_start / content_block_* / message_delta /
// message_stop / error）。
//
// 块管理：
//   - text delta → text 块（首 delta 开块）；
//   - reasoning_content delta → thinking 块（Anthropic extended thinking 形态）；
//   - tool_calls delta（按 index 合并 id/name 首片 + arguments 片段）→ tool_use 块，
//     arguments 片段走 input_json_delta。
//
// usage：上游末帧 usage.prompt/completion_tokens 汇入 message_start 的
// input_tokens 与 message_delta 的 output_tokens。
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/l3api/internal/upstream"
)

// anthropicStreamWriter 单条消息流的状态机（每请求一个实例）。
type anthropicStreamWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	reqModel string

	msgID         string
	model         string
	started       bool
	blockIndex    int      // 下一个块的 index
	openBlock     int      // 当前打开块 index；-1 = 无
	openType      string   // "text" / "thinking" / "tool_use"
	openToolIdx   int      // 当前打开的 tool_use 块对应的 tool_calls index
	finishReason  string   // 上游最后出现的 finish_reason（stop/length/tool_calls）
	toolName      []string // per-index 工具名（首片带，后续片省）
	toolID        []string // per-index 工具 id
	promptTok     int64
	completionTok int64
	// failed 写客户端失败（断连/写错误）：收尾后以错误返回，让调用方与
	// OpenAI 路径同语义（不记成功，喂连败兜底）。
	failed bool
}

// WriteAnthropicStream 消费上游 SSE，写 Anthropic 事件流。
// 返回 *upstream.StreamFrameError（流内 error 帧）或写失败错误，
// 语义与 upstream.StreamHint 一致（调用方据此撤销乐观记账）。
func WriteAnthropicStream(w http.ResponseWriter, r io.Reader, reqModel string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	sw := &anthropicStreamWriter{
		w:         w,
		flusher:   w.(http.Flusher),
		reqModel:  reqModel,
		openBlock: -1,
	}
	var errFrame string
	errFrame, rerr := sseFrames(r, func(payload string) bool {
		if isErrorFrame(payload) {
			// error 帧已由 sseFrames 记账（返回值），此处写错误事件后继续读。
			return sw.writeErrorEvent(upstreamErrorFrame(payload))
		}
		chunk, ok := parseChatChunk(payload)
		if !ok {
			return true // 非法帧忽略（与 Stream 的静默降级一致）
		}
		return sw.writeChunk(chunk)
	})
	if rerr != nil {
		return rerr
	}
	// 空流兜底（无任何块开启且未 started）：补一条错误形态，客户端能感知失败
	// 而不是拿到空消息。
	if !sw.started {
		_ = sw.writeErrorEvent("empty upstream stream")
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	sw.finishMessage()
	if errFrame != "" {
		return &upstream.StreamFrameError{Payload: errFrame}
	}
	if sw.failed {
		return fmt.Errorf("client write failed")
	}
	return nil
}

// writeEvent 写一帧 Anthropic SSE（event: <type> + data: payload）。
func (sw *anthropicStreamWriter) writeEvent(eventType string, obj map[string]any) bool {
	obj["type"] = eventType
	raw, err := json.Marshal(obj)
	if err != nil {
		return false
	}
	if _, werr := io.WriteString(sw.w, "event: "+eventType+"\ndata: "+string(raw)+"\n\n"); werr != nil {
		sw.failed = true
		return false
	}
	sw.flusher.Flush()
	return true
}

// writeErrorEvent event: error（message_start 未发也可独立出现，规范允许）。
func (sw *anthropicStreamWriter) writeErrorEvent(msg string) bool {
	return sw.writeEvent("error", map[string]any{
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

// ensureMessageStart 首个数据帧前发 message_start。
func (sw *anthropicStreamWriter) ensureMessageStart() {
	if sw.started {
		return
	}
	sw.started = true
	id := sw.msgID
	if id == "" {
		id = "msg_" + newStreamID()
	}
	sw.writeEvent("message_start", map[string]any{
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         sw.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": sw.promptTok, "output_tokens": 0},
		},
	})
}

// openBlockEvent 开新内容块（关旧块）。
func (sw *anthropicStreamWriter) openBlockEvent(blockType string, obj map[string]any) {
	sw.closeBlockEvent()
	sw.openBlock = sw.blockIndex
	sw.openType = blockType
	if obj == nil {
		obj = map[string]any{}
	}
	obj["type"] = blockType
	obj["index"] = sw.blockIndex
	sw.writeEvent("content_block_start", map[string]any{"index": sw.blockIndex, "content_block": obj})
}

// closeBlockEvent 关当前块。
func (sw *anthropicStreamWriter) closeBlockEvent() {
	if sw.openBlock < 0 {
		return
	}
	sw.writeEvent("content_block_stop", map[string]any{"index": sw.openBlock})
	sw.openBlock = -1
	sw.openType = ""
	sw.blockIndex++
}

// finishMessage 收尾：关块 + message_delta + message_stop。
func (sw *anthropicStreamWriter) finishMessage() {
	if sw.openBlock < 0 && sw.blockIndex == 0 {
		// 没有任何内容块：开一个空 text 块保持规范形态（官方空回复也带 text 块）。
		sw.openBlockEvent("text", map[string]any{"text": ""})
	}
	sw.closeBlockEvent()
	sw.writeEvent("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": anthropicStopReasonValue(sw.finishReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": sw.completionTok},
	})
	sw.writeEvent("message_stop", map[string]any{})
}

// anthropicStopReasonValue finish_reason → Anthropic stop_reason。
func anthropicStopReasonValue(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func (sw *anthropicStreamWriter) writeChunk(c *chatChunk) bool {
	if c.ID != "" && sw.msgID == "" {
		sw.msgID = "msg_" + strings.TrimPrefix(c.ID, "chatcmpl-")
	}
	if c.Model != "" && sw.model == "" {
		sw.model = c.Model
	}
	if c.Usage != nil {
		if c.Usage.PromptTokens > 0 {
			sw.promptTok = c.Usage.PromptTokens
		}
		if c.Usage.CompletionTokens > 0 {
			sw.completionTok = c.Usage.CompletionTokens
		}
	}
	sw.ensureMessageStart()
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.ReasoningContent != "" {
			if sw.openType != "thinking" {
				sw.openBlockEvent("thinking", map[string]any{"thinking": ""})
			}
			if !sw.writeEvent("content_block_delta", map[string]any{
				"index": sw.openBlock,
				"delta": map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent},
			}) {
				return false
			}
		}
		if d.Content != "" {
			if sw.openType != "text" {
				sw.openBlockEvent("text", map[string]any{"text": ""})
			}
			if !sw.writeEvent("content_block_delta", map[string]any{
				"index": sw.openBlock,
				"delta": map[string]any{"type": "text_delta", "text": d.Content},
			}) {
				return false
			}
		}
		for _, tc := range d.ToolCalls {
			if tc.Index < 0 {
				continue // 畸形上游帧（负 index）：跳过，防索引越界 panic
			}
			for tc.Index >= len(sw.toolID) {
				sw.toolID = append(sw.toolID, "")
				sw.toolName = append(sw.toolName, "")
			}
			// 新工具 index：开新块。
			if sw.openType != "tool_use" || sw.openToolIdx != tc.Index {
				if sw.toolID[tc.Index] == "" && tc.ID != "" {
					sw.toolID[tc.Index] = tc.ID
				}
				if tc.Function.Name != "" {
					sw.toolName[tc.Index] = tc.Function.Name
				}
				sw.openBlockEvent("tool_use", map[string]any{
					"id":    sw.toolID[tc.Index],
					"name":  sw.toolName[tc.Index],
					"input": json.RawMessage("{}"),
				})
				sw.openToolIdx = tc.Index
			}
			if tc.Function.Arguments != "" {
				if !sw.writeEvent("content_block_delta", map[string]any{
					"index": sw.openBlock,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}) {
					return false
				}
			}
		}
		if ch.FinishReason != "" {
			sw.finishReason = ch.FinishReason
		}
	}
	return true
}
