// responses_stream.go Responses 流式适配器：OpenAI chat chunk SSE →
// Responses 事件序列（response.created → output_item.added → *.delta →
// *.done → response.completed；error 帧 → response.failed）。
//
// 事件形态（对齐 OpenAI 规范，field 全量补齐——Codex 等客户端按字段解析）：
//   - 文本：output_item.added(message) → content_part.added →
//     response.output_text.delta* → content_part.done → output_item.done
//   - 工具：output_item.added(function_call) → response.function_call_arguments.delta*
//     → response.function_call_arguments.done → output_item.done
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/l3api/internal/upstream"
)

// responsesStreamWriter 单条响应流状态机。
type responsesStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher

	seq           int  // sequence_number 单调递增
	created       bool // response.created 已发
	respID        string
	model         string
	outputIdx     int  // 当前 output item 下标
	inText        bool // 当前 message item 的 text part 是否已开
	textBuf       strings.Builder
	itemBuf       strings.Builder // 当前 item 累积文本（done 事件回放）
	toolCalls     []responsesStreamTool
	promptTok     int64
	completionTok int64
	finishReason  string
	// failed 写客户端失败（断连/写错误）：收尾后以错误返回（语义同上）。
	failed bool
}

type responsesStreamTool struct {
	callID string
	name   string
	args   strings.Builder
	itemID string
	idx    int // output index
}

// WriteResponsesStream 消费上游 SSE，写 Responses 事件流。
// 返回 *upstream.StreamFrameError / 普通 error（语义与 upstream.StreamHint 一致）。
func WriteResponsesStream(w http.ResponseWriter, r io.Reader, reqModel string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	sw := &responsesStreamWriter{w: w, flusher: w.(http.Flusher)}

	var errFrame string
	errFrame, rerr := sseFrames(r, func(payload string) bool {
		if isErrorFrame(payload) {
			// error 帧已由 sseFrames 记账（返回值），此处写失败事件后继续读。
			return sw.writeFailed(upstreamErrorFrame(payload))
		}
		chunk, ok := parseChatChunk(payload)
		if !ok {
			return true
		}
		return sw.writeChunk(chunk)
	})
	if rerr != nil {
		return rerr
	}
	if !sw.created {
		_ = sw.writeFailed("empty upstream stream")
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	sw.closeOpenItem()
	sw.closeToolItems()
	sw.writeCompleted()
	if errFrame != "" {
		return &upstream.StreamFrameError{Payload: errFrame}
	}
	if sw.failed {
		return fmt.Errorf("client write failed")
	}
	return nil
}

// nextSeq sequence_number 递增。
func (sw *responsesStreamWriter) nextSeq() int {
	sw.seq++
	return sw.seq
}

// writeEvent 写一帧 Responses SSE（data: {...}，type 字段必带）。
func (sw *responsesStreamWriter) writeEvent(eventType string, obj map[string]any) bool {
	obj["type"] = eventType
	obj["sequence_number"] = sw.nextSeq()
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

// responseObj 组装事件里的 response 对象（created/in_progress/completed 复用）。
func (sw *responsesStreamWriter) responseObj(status string) map[string]any {
	output := []any{}
	if sw.inText || sw.textBuf.Len() > 0 {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     sw.itemID(),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": sw.textBuf.String(), "annotations": []any{},
			}},
		})
	}
	for i := range sw.toolCalls {
		tc := &sw.toolCalls[i]
		output = append(output, map[string]any{
			"type": "function_call", "id": tc.itemID, "call_id": tc.callID,
			"name": tc.name, "arguments": tc.args.String(), "status": "completed",
		})
	}
	return map[string]any{
		"id":                 sw.respID,
		"object":             "response",
		"created_at":         timeNowUnixNano() / 1e9,
		"status":             status,
		"model":              sw.model,
		"output":             output,
		"usage":              nil,
		"error":              nil,
		"incomplete_details": nil,
		"tools":              []any{},
	}
}

// itemID 当前 message item 的 id。
func (sw *responsesStreamWriter) itemID() string {
	return "msg_" + strings.TrimPrefix(sw.respID, "resp_")
}

// ensureCreated 首帧前发 response.created + response.in_progress。
func (sw *responsesStreamWriter) ensureCreated() {
	if sw.created {
		return
	}
	sw.created = true
	sw.respID = "resp_" + newStreamID()
	_ = sw.writeEvent("response.created", map[string]any{"response": sw.responseObj("in_progress")})
	_ = sw.writeEvent("response.in_progress", map[string]any{"response": sw.responseObj("in_progress")})
}

// closeOpenItem 关闭当前打开的 output item（text part 的 done 序列或工具 done）。
func (sw *responsesStreamWriter) closeOpenItem() {
	if sw.inText {
		sw.inText = false
		text := sw.textBuf.String()
		id := sw.itemID()
		_ = sw.writeEvent("response.content_part.done", map[string]any{
			"item_id": id, "output_index": sw.outputIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		_ = sw.writeEvent("response.output_item.done", map[string]any{
			"output_index": sw.outputIdx,
			"item": map[string]any{
				"type": "message", "id": id, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			},
		})
		sw.outputIdx++
	}
}

// closeToolItems 流尾收尾全部 function_call item（arguments.done + item.done）。
// 工具项的事件延迟到流尾统一收：上游 delta 可能跨 chunk 交错（index 复现），
// 逐项即时收尾需要跟踪"当前打开的工具"，而规范的 done 事件只要求 item 数据
// 完整（arguments 全量回放），晚收不影响客户端解析。
func (sw *responsesStreamWriter) closeToolItems() {
	for i := range sw.toolCalls {
		t := &sw.toolCalls[i]
		if t.idx == -1 {
			continue
		}
		_ = sw.writeEvent("response.function_call_arguments.done", map[string]any{
			"item_id": t.itemID, "output_index": t.idx, "arguments": t.args.String(),
		})
		_ = sw.writeEvent("response.output_item.done", map[string]any{
			"output_index": t.idx,
			"item": map[string]any{
				"type": "function_call", "id": t.itemID, "call_id": t.callID,
				"name": t.name, "arguments": t.args.String(), "status": "completed",
			},
		})
	}
}

// writeCompleted 终态：response.completed（带 usage）。
// writeCompleted 终态：response.completed（带 usage）。finish_reason=length
// 时 response.status 按规范置 incomplete 并附 incomplete_details（事件名仍是
// response.completed——事件流无独立 incomplete 事件），客户端据此识别截断。
func (sw *responsesStreamWriter) writeCompleted() {
	status := "completed"
	incomplete := any(nil)
	if sw.finishReason == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	resp := sw.responseObj(status)
	resp["incomplete_details"] = incomplete
	resp["usage"] = map[string]any{
		"input_tokens":          sw.promptTok,
		"output_tokens":         sw.completionTok,
		"total_tokens":          sw.promptTok + sw.completionTok,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	_ = sw.writeEvent("response.completed", map[string]any{"response": resp})
}

// writeFailed 错误终态：response.failed。
func (sw *responsesStreamWriter) writeFailed(msg string) bool {
	if !sw.created {
		// 尚未 created：先补 created（事件序列要求 completed/failed 前有 created）。
		sw.ensureCreated()
	}
	resp := sw.responseObj("failed")
	resp["error"] = map[string]any{"code": "upstream_error", "message": msg}
	return sw.writeEvent("response.failed", map[string]any{"response": resp})
}

// writeChunk 消费一个 OpenAI chunk。
func (sw *responsesStreamWriter) writeChunk(c *chatChunk) bool {
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
	sw.ensureCreated()
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.Content != "" {
			if !sw.inText {
				// 开 message item：added + content_part.added。
				id := sw.itemID()
				_ = sw.writeEvent("response.output_item.added", map[string]any{
					"output_index": sw.outputIdx,
					"item": map[string]any{
						"type": "message", "id": id, "role": "assistant", "status": "in_progress",
						"content": []any{},
					},
				})
				_ = sw.writeEvent("response.content_part.added", map[string]any{
					"item_id": id, "output_index": sw.outputIdx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})
				sw.inText = true
			}
			sw.textBuf.WriteString(d.Content)
			if !sw.writeEvent("response.output_text.delta", map[string]any{
				"item_id": sw.itemID(), "output_index": sw.outputIdx, "content_index": 0,
				"delta": d.Content,
			}) {
				return false
			}
		}
		for _, tc := range d.ToolCalls {
			if tc.Index < 0 {
				continue // 畸形上游帧（负 index）：跳过，防索引越界 panic
			}
			for tc.Index >= len(sw.toolCalls) {
				sw.toolCalls = append(sw.toolCalls, responsesStreamTool{
					itemID: "fc_" + newStreamID(),
					idx:    -1,
				})
			}
			t := &sw.toolCalls[tc.Index]
			if tc.ID != "" && t.callID == "" {
				t.callID = tc.ID
			}
			if tc.Function.Name != "" && t.name == "" {
				t.name = tc.Function.Name
			}
			if t.idx == -1 {
				// 开 function_call item：先关文本 item（同 chunk 先 text 后 tool 的场景）。
				sw.closeOpenItem()
				t.idx = sw.outputIdx
				_ = sw.writeEvent("response.output_item.added", map[string]any{
					"output_index": t.idx,
					"item": map[string]any{
						"type": "function_call", "id": t.itemID, "call_id": t.callID,
						"name": t.name, "arguments": "", "status": "in_progress",
					},
				})
				sw.outputIdx++
			}
			if tc.Function.Arguments != "" {
				t.args.WriteString(tc.Function.Arguments)
				if !sw.writeEvent("response.function_call_arguments.delta", map[string]any{
					"item_id": t.itemID, "output_index": t.idx, "delta": tc.Function.Arguments,
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
