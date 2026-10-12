// Package protocol 出站协议兼容层（v1.11）：把 Anthropic Messages 与 OpenAI
// Responses 两种协议的请求/响应/流式事件与 l3api 内部的 OpenAI chat 形态互转。
//
// 分工边界：本包只做**纯转换**（JSON in → 结构/字节 out），不碰 HTTP 路由、
// 选号、熔断——那些全部在 server.doChat 轮转核心里，两个兼容层通过 server 的
// chatSink 注入到同一核心，行为与原生 /v1/chat/completions 完全一致。
//
// 流式输入约定：上游（l3api 出站恒 stream=true）的 SSE 是 OpenAI chat chunk 帧
// （data: {...choices[].delta...}），error 帧（data: {"error":...}）与 [DONE] 同
// upstream.StreamHint 的语义；本包的流适配器逐帧消费并产出目标协议事件。
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"
)

// timeNowUnixNano / randUint16 独立封装便于测试注入确定性。
func timeNowUnixNano() int64 { return time.Now().UnixNano() }
func randUint16() uint32     { return rand.Uint32() & 0xffff }

// sseFrame 迭代上游 SSE 的 data 帧（与 upstream.Stream 同分帧规则：\n\n 分帧、
// "data: " 前缀、[DONE] 终止、注释行忽略）。f 返回 false 时提前终止（写失败）。
// 返回首个 error 帧 payload（顶层带 error 键；由本函数自行检测——回调不要自己
// 记账：闭包在 := 右侧引用同名变量会绑定到另一个变量，赋值对外不可见）与读取错误。
func sseFrames(r io.Reader, f func(payload string) bool) (errFrame string, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, rerr := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			return errFrame, nil
		case strings.HasPrefix(trimmed, "data: "):
			payload := strings.TrimPrefix(trimmed, "data: ")
			if errFrame == "" && isErrorFrame(payload) {
				errFrame = payload
			}
			if !f(payload) {
				return errFrame, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return errFrame, nil
			}
			return errFrame, rerr
		}
	}
}

// chatChunk 上游 OpenAI 流帧的宽松解析（只取翻译需要的字段）。
type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// isErrorFrame 报告 payload 是否为上游 error 帧（顶层带 error 键）。
func isErrorFrame(payload string) bool {
	var probe map[string]any
	if json.Unmarshal([]byte(payload), &probe) != nil {
		return false
	}
	_, has := probe["error"]
	return has
}

// parseChatChunk 解析一帧；error 帧（带 error 键）返回 false。
func parseChatChunk(payload string) (*chatChunk, bool) {
	var c chatChunk
	if json.Unmarshal([]byte(payload), &c) != nil {
		return nil, false
	}
	return &c, true
}

// upstreamErrorFrame 解析上游 error 帧的 message（ Anthropic/Responses 错误事件
// 都要携带上游原文，error-passthrough 语义与 OpenAI 透传一致）。
func upstreamErrorFrame(payload string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	if m, ok := e["message"].(string); ok && m != "" {
		return m
	}
	return payload
}

// fmtInt 快速 int 转 string（包内多处拼接用）。
func fmtInt(n int) string { return fmt.Sprintf("%d", n) }

// newStreamID 生成消息级 id 后缀（协议层把 chatcmpl-*/resp_* 前缀替换时用）。
// 纳秒 + 随机熵足够避免进程内碰撞。
func newStreamID() string {
	return fmt.Sprintf("%d%04x", timeNowUnixNano(), randUint16())
}
