// bodyfacts.go 请求体事实单次解析（2026-10-20 优化）。
//
// 背景：doChat 入口原先对同一份 body 做 7 次只读 unmarshal——
//   peek(stream/model) → ToolNameAllowlist → parseModelFromBody(日志 model)
//   → session.ExtractKey(粘性键) → session.TurnKey(轮级键) → hasImagePart
//   → session.ResolveConversationID(chatMeta)
// 其中 parseModelFromBody 与 peek 完全重复；ResolveConversationID 是 ExtractKey
// 前四步的逐字复制。700KB body 实测 ≈ 2.2ms CPU + 400KB 垃圾/请求（纯浪费）。
//
// 本文件把 7 次合并为 1 次 typed 解析（typed struct 比 map[string]any 快 ~30%、
// alloc 少 ~68%），产出 ChatFacts 结构供 doChat 一次取用。
//
// 语义保持（宽松解析）：任一步解析失败不阻塞请求——对应字段退化为零值，
// 与旧实现"Unmarshal 失败即返回空"完全一致。
package server

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/linguo2625469/l3api/internal/session"
)

// ChatFacts 一次解析得到的请求体事实（doChat 入口 7 次 unmarshal 的合并产物）。
type ChatFacts struct {
	// Model / Stream 请求声明的模型名与流式开关（peek 原语义）。
	Model  string
	Stream bool
	// DeclaredTools DSML 修复层工具名名单（ToolNameAllowlist 原语义）；
	// 空 = 无名单（调用方据此启用弱判定或关闭修复）。
	DeclaredTools map[string]bool
	// LogModel 表格日志用模型名（parseModelFromBody 原语义，缺省 "-"）。
	LogModel string
	// SessionKey 会话粘性键（session.ExtractKey 原语义：显式 id 优先，
	// 缺失时由 system+首条 user 文本派生）。
	SessionKey string
	// ConversationID 会话头族 conversationId（ResolveConversationID 原语义：
	// 只认 conversationId，绝不回落 user_id）。
	ConversationID string
	// TurnKey 轮级聚合键（session.TurnKey 原语义：仅 SessionKey 为空时调用方使用）。
	TurnKey string
	// HasImage 请求体是否携带 image_url part（hasImagePart 原语义）。
	HasImage bool
}

// factsBody bodyFacts 解析用的请求体形态。
//
// Content 用 json.RawMessage 而非具体 slice：畸形 content（string/数字/null）
// 会让 []struct 形态整条消息解析失败并**中止整个 Unmarshal**——实测
// {"content":"hi"} 会让 hasImagePart 直接漏判（旧实现的行为）。RawMessage 对
// 任何形态都成功解码，各字段单独判定，畸形消息不污染其他字段的解析。
type factsBody struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Metadata *struct {
		ConversationID  string `json:"conversation_id"`
		ConversationIDC string `json:"conversationId"`
		UserID          string `json:"user_id"`
	} `json:"metadata"`
	ConversationID  string `json:"conversation_id"`
	ConversationIDC string `json:"conversationId"`
	Tools           []struct {
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Functions []struct {
		Name string `json:"name"`
	} `json:"functions"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		System  json.RawMessage `json:"system"`
		ToolCalls []struct {
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
		FunctionCall *struct {
			Name string `json:"name"`
		} `json:"function_call"`
	} `json:"messages"`
}

// bodyFacts 单次解析请求体，产出 doChat 入口所需的全部事实。
//
// 解析失败（畸形 JSON）时返回零值结构——调用方按旧语义继续（模型名空 →
// 后续 resolveModel/上游报参数错，与旧实现一致）。
func bodyFacts(body []byte) ChatFacts {
	var f ChatFacts
	if len(body) == 0 {
		f.LogModel = "-"
		return f
	}
	var b factsBody
	if err := json.Unmarshal(body, &b); err != nil {
		// 畸形 body：日志 model 旧语义为 "-"，其余留空。
		f.LogModel = "-"
		return f
	}
	f.Model, f.Stream = b.Model, b.Stream
	f.LogModel = b.Model
	if f.LogModel == "" {
		f.LogModel = "-"
	}
	f.DeclaredTools = factsToolNames(&b)
	f.HasImage = factsHasImage(&b)
	f.ConversationID = factsConversationID(&b)
	f.SessionKey = factsSessionKey(&b)
	f.TurnKey = factsTurnKey(&b)
	return f
}

// factsToolNames ToolNameAllowlist 的合并实现：两个来源（本请求声明的工具 +
// 会话历史里出现过的工具）。空名单返回 nil（调用方据此启用弱判定）。
func factsToolNames(b *factsBody) map[string]bool {
	names := map[string]bool{}
	for _, t := range b.Tools {
		if t.Function != nil && t.Function.Name != "" {
			names[t.Function.Name] = true
			continue
		}
		if t.Name != "" {
			names[t.Name] = true
		}
	}
	for _, fn := range b.Functions {
		if fn.Name != "" {
			names[fn.Name] = true
		}
	}
	for _, m := range b.Messages {
		for _, tc := range m.ToolCalls {
			if tc.Function != nil && tc.Function.Name != "" {
				names[tc.Function.Name] = true
			}
		}
		if m.FunctionCall != nil && m.FunctionCall.Name != "" {
			names[m.FunctionCall.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// factsHasImage hasImagePart 的合并实现：任一消息的 content parts 里有
// type=="image_url" 即真。string content（RawMessage 首字节为 '"'）直接跳过。
func factsHasImage(b *factsBody) bool {
	for _, m := range b.Messages {
		raw := bytes.TrimSpace(m.Content)
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// factsConversationID ResolveConversationID 的合并实现：metadata 优先、
// snake 优先于 camel，**绝不回落 user_id**（X-Conversation-ID 语义是对话 ID，
// user_id 回落会污染后台按对话聚合的判据）。
func factsConversationID(b *factsBody) string {
	if b.Metadata != nil {
		if b.Metadata.ConversationID != "" {
			return b.Metadata.ConversationID
		}
		if b.Metadata.ConversationIDC != "" {
			return b.Metadata.ConversationIDC
		}
	}
	if b.ConversationID != "" {
		return b.ConversationID
	}
	return b.ConversationIDC
}

// factsSessionKey ExtractKey 的合并实现：显式会话 id 优先（metadata → 顶层，
// snake 优先于 camel），缺失时由 system 文本 + 首条 user 文本派生稳定键
// （同一会话多轮恒定 → 粘住同一账号命中上游 prompt 缓存）。
//
// 派生口径与 session.deriveKey 完全一致：SHA-256(system+"\x00"+firstUser) 前
// 16 字节，加 "d-" 前缀与显式 id 命名空间隔离。
func factsSessionKey(b *factsBody) string {
	if v := factsConversationID(b); v != "" {
		return v
	}
	// 派生键：system/developer 首条文本 + 首条 user 文本。
	systemText, firstUserText := "", ""
	for _, m := range b.Messages {
		text := factsMessageText(m.Content)
		switch m.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				firstUserText = text
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史（与 deriveKey 同口径）
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话（不粘性，安全降级）
	}
	return session.DeriveSessionKey(systemText, firstUserText)
}

// factsTurnKey TurnKey 的合并实现：**最后一条** role=="user" 消息的
// 「序号 + 文本」。取最后一条而非第一条：同轮内该消息位置恒定（tool call
// 多轮/换号重试共享同键），用户发下一条消息即换键——对齐官方
// X-Conversation-Request-ID 的「对话轮」语义。序号一并入键，两次不同轮里
// 内容相同的提问（"继续"）不会被并成一轮。
//
// 最后一条 user 消息无文本（纯图片）→ ""：不继续往前找（整轮内该消息位置
// 恒定，往前找会让键随 step 漂移）。
func factsTurnKey(b *factsBody) string {
	for i := len(b.Messages) - 1; i >= 0; i-- {
		m := b.Messages[i]
		if m.Role != "user" {
			continue
		}
		text := factsMessageText(m.Content)
		if text == "" {
			return ""
		}
		return "u" + itoa(i) + ":" + text
	}
	return ""
}

// factsMessageText 取消息 content 文本：字符串形态原样；数组形态（多模态
// parts）拼接各 part 的 text 字段；其他（数字/null/对象）为空。
// 与 session.contentText/messageText 同口径（经 json.RawMessage 分支判定）。
func factsMessageText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	case '[':
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return ""
		}
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

// itoa 小整数快速字符串化（轮级键的序号，值域 = 消息数，通常 <1000）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// rewriteModelWithFacts 把出站 body 的 model 字段替换为 bare（保留其余字段）。
// 仅当 bare != 原 model 时调用；body 不可解析/已是 bare 时原样返回。
// 与 logging.go rewriteModel 同语义——本函数供 prompt 改写已合并的场景使用
// （prompt 改写失败回落原 body 时仍需剥前缀）。
func rewriteModelWithFacts(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
