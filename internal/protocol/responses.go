// responses.go OpenAI Responses API（/v1/responses）兼容层。
//
// 覆盖面（Codex / 新版 OpenAI SDK / n8n Responses 节点的日常路径）：
//   - input：string 快捷形态 | items 数组（message / function_call /
//     function_call_output）；instructions → system；tools（扁平 function 形态）、
//     tool_choice、max_output_tokens、temperature/top_p；流式与非流式。
//   - output：message（output_text）/ function_call 两类 item；usage 三元组。
//   - 不支持（照常工作但忽略）：reasoning 相关请求参数（网关不模拟 reasoning
//     item，模型自带的 reasoning_content 上游观测保留）、store/previous_response_id
//     （网关无状态；多轮由客户端回传完整 input——与 function_call_output 回传路径
//     一致）、web_search/file_search 工具。
//
// 相比直接用 OpenAI chat 兼容层的原因：Codex 等客户端只讲 Responses 事件协议
// （response.output_text.delta 等），chat chunk 无法被它们解析。
package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ResponsesRequest /v1/responses 请求宽松解析。
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"` // string | items
	Instructions       string          `json:"instructions"`
	Tools              []responsesTool `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Stream             bool            `json:"stream"`
	MaxOutputTokens    int64           `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	Reasoning          json.RawMessage `json:"reasoning"`
	Metadata           json.RawMessage `json:"metadata"`
}

// responsesTool Responses 的 function 工具（扁平：name/parameters 在顶层）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responsesItem input/output item 的宽松形态。
type responsesItem struct {
	Type    string          `json:"type"` // message | function_call | function_call_output | item_reference
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | parts
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	ID        string `json:"id"`
	// function_call_output
	Output json.RawMessage `json:"output"`
}

// responsesContentPart content part（input_text/output_text/input_image）。
type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	// input_image
	ImageDetail string `json:"detail"`
}

// ParseResponsesRequest 解析并转换成 OpenAI chat body。
func ParseResponsesRequest(body []byte) (*ResponsesRequest, []byte, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid json: %w", err)
	}
	if req.Model == "" {
		return nil, nil, fmt.Errorf("model: is required")
	}
	if req.PreviousResponseID != "" {
		return nil, nil, fmt.Errorf("previous_response_id: 网关无状态存储，不支持该参数——请回传完整 input（含 function_call_output）")
	}

	chat := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		chat["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	msgs := make([]any, 0, 8)
	if req.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	if err := convertResponsesInput(req.Input, &msgs); err != nil {
		return nil, nil, err
	}
	chat["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				return nil, nil, fmt.Errorf("tools[%s].type: 仅支持 function 工具（%q 不支持）", t.Name, t.Type)
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				var schema any
				if json.Unmarshal(t.Parameters, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		chat["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		var tc any
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			switch v := tc.(type) {
			case string:
				if v == "auto" || v == "none" || v == "required" {
					chat["tool_choice"] = v
				}
			case map[string]any:
				if name, ok := v["name"].(string); ok {
					chat["tool_choice"] = map[string]any{
						"type":     "function",
						"function": map[string]any{"name": name},
					}
				}
			}
		}
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, err
	}
	return &req, out, nil
}

// convertResponsesInput input（string | items）→ OpenAI messages（追加进 msgs）。
func convertResponsesInput(raw json.RawMessage, msgs *[]any) error {
	if len(raw) == 0 {
		return fmt.Errorf("input: is required")
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		*msgs = append(*msgs, map[string]any{"role": "user", "content": plain})
		return nil
	}
	var items []responsesItem
	if json.Unmarshal(raw, &items) != nil {
		return fmt.Errorf("input: 不是 string 或 item 数组")
	}
	for i, item := range items {
		switch item.Type {
		case "", "message":
			role := item.Role
			if role == "" {
				role = "user"
			}
			content, err := responsesContentToOpenAI(item.Content, i)
			if err != nil {
				return err
			}
			*msgs = append(*msgs, map[string]any{"role": role, "content": content})
		case "function_call":
			// 助手的历史工具调用：转 assistant tool_calls（保持调用顺序）。
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			*msgs = append(*msgs, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":       item.CallID,
					"type":     "function",
					"function": map[string]any{"name": item.Name, "arguments": args},
				}},
			})
		case "function_call_output":
			// 工具结果回传：output（string | parts）→ tool 消息。
			out := item.Output
			var s string
			if json.Unmarshal(out, &s) == nil {
				*msgs = append(*msgs, map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": s})
			} else {
				b, _ := json.Marshal(out)
				*msgs = append(*msgs, map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": string(b)})
			}
		case "reasoning":
			// 回传的 reasoning item：网关不模拟，跳过（上游会生成新的推理内容）。
			continue
		case "item_reference":
			return fmt.Errorf("input[%d].type=item_reference: 网关无状态存储，不支持引用历史 item", i)
		default:
			return fmt.Errorf("input[%d].type=%q: 不支持的 item 类型", i, item.Type)
		}
	}
	return nil
}

// responsesContentToOpenAI content（string | parts）→ OpenAI content。
func responsesContentToOpenAI(raw json.RawMessage, idx int) (any, error) {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain, nil
	}
	var parts []responsesContentPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil, fmt.Errorf("input[%d].content: 不是 string 或 part 数组", idx)
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "", "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image", "image_url":
			url := p.ImageURL
			if url != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		default:
			out = append(out, map[string]any{"type": "text", "text": "[unsupported part: " + p.Type + "]"})
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 非流式响应转换
// ---------------------------------------------------------------------------

// ResponsesFinalFromOpenAI 聚合 OpenAI chat.completion → Responses 对象。
// finish_reason=length（输出被 max_output_tokens 截断）→ status "incomplete" +
// incomplete_details.reason="max_output_tokens"（Responses 规范语义；一律报
// completed 会让客户端把截断回答当成完整回答）。
func ResponsesFinalFromOpenAI(resp map[string]any, reqModel string) map[string]any {
	choice, _ := firstChoice(resp)
	status, incomplete := "completed", any(nil)
	if fr, _ := choice["finish_reason"].(string); fr == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	message, _ := choice["message"].(map[string]any)
	id, _ := resp["id"].(string)
	model, _ := resp["model"].(string)
	if model == "" {
		model = reqModel
	}

	output := []any{}
	fcIdx := 0
	if message != nil {
		if txt, _ := message["content"].(string); txt != "" {
			output = append(output, map[string]any{
				"type":    "message",
				"id":      "msg_" + strings.TrimPrefix(id, "chatcmpl-"),
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": txt, "annotations": []any{}}},
			})
		}
		if tcs, ok := message["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				call, _ := tc.(map[string]any)
				if call == nil {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				name := ""
				args := "{}"
				if fn != nil {
					name, _ = fn["name"].(string)
					if a, _ := fn["arguments"].(string); a != "" {
						args = a
					}
				}
				cid, _ := call["id"].(string)
				if cid == "" {
					cid = fmt.Sprintf("call_%d", fcIdx)
				}
				fcIdx++
				output = append(output, map[string]any{
					"type":      "function_call",
					"id":        "fc_" + strings.TrimPrefix(cid, "call_"),
					"call_id":   cid,
					"name":      name,
					"arguments": args,
					"status":    "completed",
				})
			}
		}
	}
	return map[string]any{
		"id":                  "resp_" + strings.TrimPrefix(id, "chatcmpl-"),
		"object":              "response",
		"created_at":          int64OrZero(resp["created"]),
		"status":              status,
		"incomplete_details":  incomplete,
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage": map[string]any{
			"input_tokens":          usageField(resp, "prompt_tokens"),
			"output_tokens":         usageField(resp, "completion_tokens"),
			"total_tokens":          usageField(resp, "total_tokens"),
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}
}

// int64OrZero created（float64）→ int64。
func int64OrZero(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// WriteResponsesError Responses 错误形态（与 chat completions 同构的 error 包裹）。
func WriteResponsesError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
			"param":   nil,
		},
	})
}
