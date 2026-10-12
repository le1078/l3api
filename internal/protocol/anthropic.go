// anthropic.go Anthropic Messages API（/v1/messages）兼容层。
//
// 覆盖面（Claude Code / Cline / Cherry Studio 等客户端的日常路径）：
//   - system（string 或 content block 数组）、messages（text/image/tool_use/
//     tool_result blocks）、tools + tool_choice、stop_sequences、max_tokens、
//     temperature/top_p；流式与非流式；reasoning_content → thinking block。
//   - 不支持（显式 400 invalid_request_error）：metadata.user_id 透传无意义、
//     content block 的 document/citations 类型、tool_choice 的 disable_parallel_tool_use。
//
// 流式事件序列（Anthropic 官方规范）：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	→ message_delta(stop_reason) → message_stop；error 帧 → event: error。
package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// AnthropicRequest /v1/messages 请求的宽松解析。
type AnthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int64           `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	StopSequences []string        `json:"stop_sequences"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicBlock content block 的宽松形态（type 分流）。
type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// image
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string | []block
	IsError   bool            `json:"is_error"`
}

// ParseAnthropicRequest 解析并转换成 OpenAI chat body。
// 校验失败返回 error（调用方映射为 400 invalid_request_error）。
func ParseAnthropicRequest(body []byte) (*AnthropicRequest, []byte, error) {
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid json: %w", err)
	}
	if req.Model == "" {
		return nil, nil, fmt.Errorf("model: is required")
	}
	if req.MaxTokens <= 0 {
		return nil, nil, fmt.Errorf("max_tokens: is required（Anthropic 协议必填，请带上单次生成的 token 上限）")
	}

	chat := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}
	if len(req.StopSequences) > 0 {
		chat["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	// system：string 或 block 数组（text block 取 text）。
	if sys := systemToText(req.System); sys != "" {
		chat["system"] = sys
	}
	// tools → OpenAI function 形态。
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if json.Unmarshal(t.InputSchema, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		chat["tools"] = tools
	}
	// tool_choice 映射：auto→auto，any→required，tool→{type:function}。
	if len(req.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			switch tc.Type {
			case "auto":
				chat["tool_choice"] = "auto"
			case "any":
				chat["tool_choice"] = "required"
			case "tool":
				chat["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tc.Name},
				}
			}
		}
	}
	msgs, err := convertAnthropicMessages(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	chat["messages"] = msgs
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, err
	}
	return &req, out, nil
}

// systemToText system 字段归一为文本（string 直取；block 数组拼接 text 块）。
func systemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// convertAnthropicMessages messages 数组 → OpenAI messages。
func convertAnthropicMessages(in []anthropicMsg) ([]any, error) {
	msgs := make([]any, 0, len(in))
	for i, m := range in {
		// content 为纯 string 的快捷形态。
		var plain string
		if json.Unmarshal(m.Content, &plain) == nil {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": plain})
			continue
		}
		var blocks []anthropicBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil, fmt.Errorf("messages[%d].content: 不是 string 或 block 数组", i)
		}
		switch m.Role {
		case "user":
			msgs = append(msgs, convertAnthropicUserBlocks(blocks)...)
		case "assistant":
			msgs = append(msgs, convertAnthropicAssistantBlocks(blocks))
		default:
			return nil, fmt.Errorf("messages[%d].role: %q 不支持（仅 user/assistant）", i, m.Role)
		}
	}
	return msgs, nil
}

// convertAnthropicUserBlocks user 消息：text/image 直转；tool_result 转成
// 独立的 OpenAI tool 消息（一条 tool_result = 一条 tool message，顺序保持）。
func convertAnthropicUserBlocks(blocks []anthropicBlock) []any {
	out := make([]any, 0, len(blocks))
	var parts []any // user content parts（text/image）
	flushParts := func() {
		if len(parts) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": parts})
		parts = nil
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": blk.Text})
		case "image":
			url := blk.Source.URL
			if blk.Source.Type == "base64" && blk.Source.Data != "" {
				url = "data:" + blk.Source.MediaType + ";base64," + blk.Source.Data
			}
			if url != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_result":
			// 工具结果：OpenAI 形态是独立 {role:"tool", tool_call_id, content}。
			content := toolResultToText(blk)
			flushParts() // tool 结果必须先于后续文本块投递（保序）
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": blk.ToolUseID,
				"content":      content,
			})
		default:
			// 未知 block：降级为文本占位（不中断会话；上游会按文本处理）。
			parts = append(parts, map[string]any{"type": "text", "text": "[unsupported block: " + blk.Type + "]"})
		}
	}
	flushParts()
	return out
}

// toolResultToText tool_result 的 content（string | blocks）归一为文本。
func toolResultToText(blk anthropicBlock) string {
	var s string
	if json.Unmarshal(blk.Content, &s) == nil {
		if blk.IsError {
			return "tool error: " + s
		}
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(blk.Content, &blocks) == nil {
		var b strings.Builder
		for _, sub := range blocks {
			if sub.Type == "text" && sub.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(sub.Text)
			}
		}
		if blk.IsError {
			return "tool error: " + b.String()
		}
		return b.String()
	}
	return ""
}

// convertAnthropicAssistantBlocks assistant 消息：text → content，
// tool_use → tool_calls（一次转换全带，content 与 tool_calls 并存合法）。
func convertAnthropicAssistantBlocks(blocks []anthropicBlock) map[string]any {
	var textParts []string
	var toolCalls []any
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				textParts = append(textParts, blk.Text)
			}
		case "tool_use":
			args := "{}"
			if len(blk.Input) > 0 {
				var v any
				if json.Unmarshal(blk.Input, &v) == nil {
					if b, err := json.Marshal(v); err == nil {
						args = string(b)
					}
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":       blk.ID,
				"type":     "function",
				"function": map[string]any{"name": blk.Name, "arguments": args},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// ---------------------------------------------------------------------------
// 非流式响应转换
// ---------------------------------------------------------------------------

// AnthropicFinalFromOpenAI 聚合的 OpenAI chat.completion → Anthropic message 对象。
func AnthropicFinalFromOpenAI(resp map[string]any, reqModel string) map[string]any {
	choice, _ := firstChoice(resp)
	message, _ := choice["message"].(map[string]any)
	id, _ := resp["id"].(string)
	model, _ := resp["model"].(string)
	if model == "" {
		model = reqModel
	}

	content := []any{}
	if message != nil {
		if txt, _ := message["content"].(string); txt != "" {
			content = append(content, map[string]any{"type": "text", "text": txt})
		}
		if rc, _ := message["reasoning_content"].(string); rc != "" {
			// thinking 块排在 text 前（生成顺序语义）。
			content = append([]any{map[string]any{"type": "thinking", "thinking": rc}}, content...)
		}
		if tcs, ok := message["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				call, _ := tc.(map[string]any)
				if call == nil {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				input := json.RawMessage("{}")
				if fn != nil {
					if raw, _ := fn["arguments"].(string); raw != "" {
						input = json.RawMessage(raw)
					}
				}
				name := ""
				if fn != nil {
					name, _ = fn["name"].(string)
				}
				cid, _ := call["id"].(string)
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    cid,
					"name":  name,
					"input": input,
				})
			}
		}
	}
	return map[string]any{
		"id":            "msg_" + strings.TrimPrefix(id, "chatcmpl-"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(choice),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usageField(resp, "prompt_tokens"),
			"output_tokens": usageField(resp, "completion_tokens"),
		},
	}
}

// firstChoice 取 choices[0]（缺省空对象）。
func firstChoice(resp map[string]any) (map[string]any, bool) {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return map[string]any{}, false
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		c = map[string]any{}
	}
	return c, true
}

// anthropicStopReason finish_reason → Anthropic stop_reason。
func anthropicStopReason(choice map[string]any) string {
	fr, _ := choice["finish_reason"].(string)
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// usageField 从 OpenAI usage 取 int64 字段（缺省 0）。
func usageField(resp map[string]any, key string) int64 {
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return 0
	}
	v, _ := u[key].(float64)
	return int64(v)
}

// WriteAnthropicError Anthropic 错误形态：
// {"type":"error","error":{"type":"<类型>","message":"<原文>"}}。
// errType：invalid_request_error / authentication_error / permission_error /
// rate_limit_error / api_error / overloaded_error。
func WriteAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// AnthropicErrType 网关错误 code → Anthropic error type（状态码兜底）。
func AnthropicErrType(code string) string {
	switch code {
	case "invalid_api_key":
		return "authentication_error"
	case "model_not_allowed":
		return "permission_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "invalid_request", "request_body_too_large", "prompt_too_long", "content_blocked":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}
