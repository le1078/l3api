// trae_chat.go Trae（字节 · CN）chat SSE 桥。
//
// 链路：OpenAI 请求体 → Trae llm_utils_chat 请求体 → cookie 现换 Cloud-IDE-JWT →
// POST {TraeChatHost}/api/agent/v3/llm_utils_chat → 事件流（metadata/output/token_usage/
// error/done）→ 还原成 l3api 期望的 OpenAI SSE 帧流（data: {...}\n\n），供 handler
// 复用 StreamHint/Aggregate 消费管线（与 QoderChatStream 同一契约）。
//
// 协议来源（2026-09-28 云机实测，探测脚本见 /root/probe_trae.py）：
//   - 官方 api.trae.cn 的 /api/agent/v3/llm_utils_chat 恒 404（TLB），国内版聊天
//     流量没有可用的官方直连 host（逆向社区同结论，见 JeffHu0912/trae2api
//     constants.go）。可用路径是中转 trae-api-cn.mchost.guru——JWT（8h 短效）
//     会经过该第三方服务器，这是已知且被用户接受的折衷；host 可由
//     TraeChatHost 覆盖（找到官方直连或自建中转时替换，勿改鉴权逻辑）。
//   - 实测通过组合：function=chat_v3 / solo_work_lite；config_name=model 需为
//     get_detail_param 下发的真实配置名（glm-5.3/kimi-k3/DeepSeek-V4-Pro 实测 200，
//     旧名 deepseek-V3 报 4001 param invalid）。
//   - 流内事件：metadata → timing_cost → output{response,reasoning_content}×N →
//     token_usage → done{finish_reason}；失败为 event:error{code,message}。
//     已知 code：1001 会话失效、1005/4008 额度耗尽、4011 软限流、4017 设备风控、
//     4001 参数错误、3003 all models failed（模型路由层故障，换号/换通道可解）、
//     4023 model is unknown。
//
// 凭据模型：cookie（Auth.RefreshToken，约 14 天长效）现换 8h 短效 JWT——JWT 在
// 进程内按 uid 缓存（traeJWTCache），过期前 10 分钟重换，不落盘（与签到路径同
// 原则：落盘的 AccessToken 仅供校验展示）。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// TraeChatHost chat 中转 host（见文件头注释：官方国内版无直连 chat host）。
// 包级变量而非 const：找到官方直连/自建中转时允许覆盖（cmd/server main 可注入）。
var TraeChatHost = "https://trae-api-cn.mchost.guru"

const (
	traeChatPath = "/api/agent/v3/llm_utils_chat"
	// traeChatFunction 实测可用通道（inline_chat 对任意模型恒 3003，勿用）。
	traeChatFunction = "chat_v3"
	traeAppID        = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeIdeVersion   = "2.0.0"
	traeIdeVerCode   = "20260811"
	// traeJWTLifetime Cloud-IDE-JWT 标称有效期 8h；提前 10min 重换。
	traeJWTLifetime    = 8 * time.Hour
	traeJWTRefreshSkew = 10 * time.Minute
)

// ────────────────────────────────────────────────────────────────────────────
// JWT 进程内缓存（cookie → JWT 每请求一次兑换约 +200ms，8h 有效期内应复用）
// ────────────────────────────────────────────────────────────────────────────

type traeJWTEntry struct {
	jwt     string
	expires time.Time
}

var traeJWTCache struct {
	mu sync.Mutex
	m  map[string]traeJWTEntry // key: uid
}

// traeJWTFor 取该账号当前可用的 JWT：缓存命中（余量 > skew）直接用，否则 cookie 现换。
// cookie 过期 → ErrSessionDead（TraeExchangeCookie 已分类），handler 侧禁用账号。
func (c *Client) traeJWTFor(ctx context.Context, a *auth.Auth) (string, error) {
	a.Lock()
	uid := a.UID
	cookie := a.RefreshToken
	deviceID := a.DeviceToken
	a.Unlock()
	if strings.TrimSpace(cookie) == "" {
		return "", &Error{Kind: ErrSessionDead, Status: 401,
			Msg: "trae cookie 缺失（auth.RefreshToken 为空），请重新导入账号"}
	}
	if deviceID == "" {
		deviceID = TraeDeviceIDFor(uid)
	}
	traeJWTCache.mu.Lock()
	if e, ok := traeJWTCache.m[uid]; ok && time.Now().Add(traeJWTRefreshSkew).Before(e.expires) {
		jwt := e.jwt
		traeJWTCache.mu.Unlock()
		return jwt, nil
	}
	traeJWTCache.mu.Unlock()
	sess, err := c.TraeExchangeCookie(ctx, cookie, deviceID)
	if err != nil {
		return "", err
	}
	traeJWTCache.mu.Lock()
	if traeJWTCache.m == nil {
		traeJWTCache.m = map[string]traeJWTEntry{}
	}
	traeJWTCache.m[uid] = traeJWTEntry{jwt: sess.JWT, expires: time.Now().Add(traeJWTLifetime)}
	traeJWTCache.mu.Unlock()
	return sess.JWT, nil
}

// ────────────────────────────────────────────────────────────────────────────
// 请求体构造（OpenAI → llm_utils_chat）
// ────────────────────────────────────────────────────────────────────────────

// traeChatRequest llm_utils_chat 请求体（实测必需字段；content 为结构化分片数组）。
type traeChatRequest struct {
	Messages   []traeChatMessage `json:"messages"`
	Function   string            `json:"function"`
	Stream     bool              `json:"stream"`
	ConfigName string            `json:"config_name,omitempty"`
	Model      string            `json:"model,omitempty"`
}

type traeChatMessage struct {
	Role    string            `json:"role"`
	Content []traeContentPart `json:"content"`
}

type traeContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// traeBuildChatBody 把 OpenAI chat 请求体转成 llm_utils_chat 请求体。
// model 为裸模型 key（已剥 trae: 前缀）：空 → 不传 config_name/model（上游默认模型，
// 实测默认通道可用）；非空 → config_name 与 model 同填（上游两者都消费）。
// system/developer 角色不拆出（llm_utils_chat 的 messages 直接接受 system 角色）。
func traeBuildChatBody(body []byte, model string) ([]byte, error) {
	var req openaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("trae: parse openai body: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("trae: no messages in request")
	}
	msgs := make([]traeChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		text := qoderContentText(m.Content) // content 字符串/分片数组两种形态兼容取文本
		role := strings.ToLower(m.Role)
		if role == "developer" {
			role = "system" // OpenAI 新名 → 上游通用名
		}
		msgs = append(msgs, traeChatMessage{
			Role:    role,
			Content: []traeContentPart{{Type: "text", Text: text}},
		})
	}
	out := traeChatRequest{
		Messages: msgs,
		Function: traeChatFunction,
		Stream:   true, // 上游永远流式；非流式客户端由 handler 侧 Aggregate 聚合
	}
	if model != "" {
		out.ConfigName = model
		out.Model = model
	}
	return json.Marshal(out)
}

// ────────────────────────────────────────────────────────────────────────────
// 主流：TraeChatStream
// ────────────────────────────────────────────────────────────────────────────

// TraeChatStream Trae chat SSE 桥：发 llm_utils_chat 请求，返回**翻译后**的
// OpenAI SSE 帧流（data: {...}\n\n）。契约与 QoderChatStream 完全一致：
//   - 返回的 *Error 与 codebuddy 路径同语义（Kind 分类 → handler 冷却/换号）；
//   - 传输层错误返回裸 fmt error（handler 走抖动换号）；
//   - 开流后预读至首个实质事件：error 帧在该窗口内到达 → 关流并返回 *Error
//     （handler 尚未 NoteSuccess，可正常轮换）；首个 output 到达 → 返回的流
//     先回放预读缓冲、再继续惰性翻译后续事件（流中晚期错误以 error 帧透传收尾）。
//
// conversationID 当前不参与上游协议（llm_utils_chat 无会话字段），保留入参只为
// 与 QoderChatStream 签名对齐（未来接 create_agent_task 通道时使用）。
func (c *Client) TraeChatStream(ctx context.Context, a *auth.Auth, body []byte, conversationID, model string) (io.ReadCloser, int, []byte, error) {
	_ = conversationID
	if ctx == nil {
		ctx = context.Background()
	}
	jwt, err := c.traeJWTFor(ctx, a)
	if err != nil {
		return nil, 0, nil, err
	}
	payload, err := traeBuildChatBody(body, model)
	if err != nil {
		return nil, 0, nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeChatHost+traeChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	a.Lock()
	uid := a.UID
	deviceID := a.DeviceToken
	a.Unlock()
	if deviceID == "" {
		deviceID = TraeDeviceIDFor(uid)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Cloud-IDE-JWT "+jwt)
	h.Set("X-Cloudide-Token", jwt)
	h.Set("x-app-id", traeAppID)
	h.Set("x-app-version", "default")
	h.Set("x-ide-version", traeIdeVersion)
	h.Set("x-ide-version-code", traeIdeVerCode)
	h.Set("x-ide-version-type", "stable")
	h.Set("x-device-id", deviceID)
	h.Set("x-machine-id", deviceID)
	h.Set("x-device-brand", "Windows")
	h.Set("x-device-cpu", "x64")
	h.Set("x-device-type", "desktop")
	h.Set("x-os-version", "10.0.26200")
	h.Set("x-uid", uid)
	h.Set("request-traffic-type", "prod")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", traeUA)

	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return nil, 0, nil, fmt.Errorf("trae: read err body: %w", rerr)
		}
		kind := traeClassifyHTTP(resp.StatusCode, string(raw))
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		return nil, traeErrStatus(kind, resp.StatusCode), raw, ue
	}

	// 预读：开流后先消费到首个实质事件（output/error/done），error → 直接 *Error
	// 返回（handler 可换号）；拿到 output → 缓冲回放。同步执行：取消语义由 ctx
	// （客户端断连）与 monitorBody 的 IdleTimeout（流空闲）兜底，queue/progress
	// 过程事件会刷新空闲计时，不会误掐排队中的请求。
	src := monitorBody(resp.Body, c.IdleTimeout, c.StreamTotalTimeout, cancel)
	tr := &traeSSETranslator{rc: src, cancel: cancel, model: model, id: "chatcmpl-trae-" + uid}
	if ue := tr.preread(); ue != nil {
		tr.Close()
		status := ue.Status
		if status < 400 {
			status = traeErrStatus(ue.Kind, 502)
		}
		return nil, status, []byte(ue.Msg), ue
	}
	return tr, http.StatusOK, nil, nil
}

// traeClassifyHTTP chat 端点 HTTP 层错误分类（与 codebuddy Classify 同语义面）。
func traeClassifyHTTP(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == 401 || status == 403:
		return ErrSessionDead
	case status == 429 || strings.Contains(lower, "rate limit"):
		return ErrSoftRate
	case status == 402 || strings.Contains(lower, "quota") || strings.Contains(body, "额度"):
		return ErrHardCredit
	case status == 404:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// traeClassifyStreamCode 流内 error 帧 code → ErrKind（2026-09-28 实测码表，
// 见文件头）。未知码归 ErrServer（可换号重试，不罚重）。
func traeClassifyStreamCode(code int) ErrKind {
	switch code {
	case 1001:
		return ErrSessionDead // auth 失效（cookie/JWT 过期）
	case 1005, 4008:
		return ErrHardCredit // 额度耗尽
	case 4011:
		return ErrSoftRate // 软限流（requests have exceeded the rate limit）
	case 4017:
		return ErrSoftRate // 设备/IP 级风控：换号无效，软冷却本号短避
	case 4001, 3003, 4023:
		// 4001 参数错误 / 3003 all models failed（模型路由层故障）/ 4023 模型未知：
		// 多为通道或模型名问题，换号可能可解（3003 实测与号无关但与通道/时机有关），
		// 归 ErrClient（轮转不熔断）。
		return ErrClient
	}
	return ErrServer
}

// traeErrStatus 把流内/信封错误映射回 HTTP 状态（handler 的错误分支以 status>=400
// 为判定前提——流内错误必须抬一个 >=400 的伪状态才能进分类路径）。
func traeErrStatus(kind ErrKind, fallback int) int {
	switch kind {
	case ErrSessionDead:
		return 401
	case ErrHardCredit:
		return 402
	case ErrSoftRate:
		return 429
	case ErrServer:
		return 502
	default:
		if fallback >= 400 {
			return fallback
		}
		return 500
	}
}

// ────────────────────────────────────────────────────────────────────────────
// SSE 事件 → OpenAI 帧翻译器
// ────────────────────────────────────────────────────────────────────────────

// traeSSETranslator 把 llm_utils_chat 事件流（event:metadata/output/token_usage/
// error/done）翻译成 OpenAI SSE 帧流。单消费者契约（同 qoderSSEUnwrapper），无锁。
type traeSSETranslator struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	br      *bufio.Reader
	pending []byte // 已翻译待消费字节（data: {...}\n\n 帧）
	err     error
	done    bool
	id      string // 全流统一的消息级 id（StreamHint 的 firstID 续传也依赖非空 id）
	model   string // 透出到 chunk.model（空则上游默认模型，回填 "trae"）

	// 流内晚期错误（预读窗口后到达）：粘滞到 lateErr，翻译为 OpenAI error 帧
	// 透传（sse.go 的 error-passthrough 路径原样写出），随后正常 [DONE] 收尾。
	// pending 放完后由 Read 当错误返回（2026-10-20）——此前 lateErr 只写不读，
	// Trae 流内 session 失效（code 1001）/额度耗尽（1005/4008）既不冷却也不
	// 禁用，账号反复被选中、每轮都白烧上游流量。
	lateErr *Error

	sentRole bool // 首帧 delta.role=assistant 只发一次
}

// preread 消费事件直到首个实质事件（output 正片/error/done）：
//   - output → 翻译进 pending，返回 nil（成功开流）；
//   - error → 分类成 *Error 返回（调用方关流换号）；
//   - done（无 output 的空完成）→ ErrServer（空响应，换号重试）；
//   - 流提前 EOF → ErrServer。
//
// metadata/timing_cost/progress_notice/queue_* 等过程事件跳过（不翻译、不计实质）。
func (t *traeSSETranslator) preread() *Error {
	for {
		ev, data, err := t.nextEvent()
		if err != nil {
			return &Error{Kind: ErrServer, Msg: "trae preread: " + err.Error()}
		}
		switch ev {
		case "output":
			t.emitOutput(data)
			return nil
		case "error":
			return t.classifyStreamError(data)
		case "done":
			return &Error{Kind: ErrServer, Msg: "trae: stream done without any output"}
		default:
			// 过程事件：继续等
		}
	}
}

// nextEvent 读下一帧 SSE（event: + data: 对），返回事件名与 data 载荷。
// 帧以空行结束；无 event: 行的帧按 data 内容推断（防御）。
func (t *traeSSETranslator) nextEvent() (string, string, error) {
	if t.br == nil {
		t.br = bufio.NewReaderSize(t.rc, 64*1024)
	}
	var ev string
	var data strings.Builder
	for {
		line, err := t.br.ReadString('\n')
		// 单行上界（2026-10-20）：trae 帧以 event:/data: 行传输，无换行超长行
		// 只可能是上游故障——超限断流（Read 侧走既有错误路径）。
		if lerr := CheckSSELineLen(line, 0); lerr != nil {
			return "", "", lerr
		}
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "event:") {
				ev = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			} else if strings.HasPrefix(trimmed, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			} else if trimmed == "" && (ev != "" || data.Len() > 0) {
				return ev, data.String(), nil
			}
		}
		if err != nil {
			if err == io.EOF && data.Len() > 0 {
				return ev, data.String(), nil // 尾帧无空行
			}
			return "", "", err
		}
	}
}

// Read 实现 io.Reader：先放 pending（含 preread 缓冲），再惰性翻译后续事件。
func (t *traeSSETranslator) Read(p []byte) (int, error) {
	if len(t.pending) > 0 {
		n := copy(p, t.pending)
		t.pending = t.pending[n:]
		return n, nil
	}
	if t.done {
		// 晚期错误（2026-10-20）：pending 已放完（error 帧 + [DONE] 都交给
		// 客户端了），此时才把粘滞的 lateErr 当 Read 错误返回——上游侧的
		// StreamHint/客户端写循环据此走失败路径，handler 的
		// errors.As(serr, &sfe) 之前的 err 分支即可按 *Error 分类罚号。
		// 放在这里而不是 emitErrorFrame 当场返回：客户端必须能读到 error 帧
		// 和 [DONE]（否则看到的是连接截断，不是上游真实错误）。
		if t.lateErr != nil {
			err := t.lateErr
			t.lateErr = nil // 一次性：后续 Read 恒 EOF，不重复报错
			return 0, err
		}
		return 0, io.EOF
	}
	if t.err != nil {
		return 0, t.err
	}
	for {
		ev, data, err := t.nextEvent()
		if err != nil {
			if err == io.EOF {
				// 上游未发 done 就断流：补 [DONE] 收尾（StreamHint 见到 [DONE] 停读）。
				t.done = true
				t.pending = append(t.pending, "data: [DONE]\n\n"...)
				break
			}
			t.err = err
			return 0, err
		}
		switch ev {
		case "output":
			t.emitOutput(data)
		case "error":
			// 流中晚期错误：翻译为 OpenAI error 帧透传（客户端可见真实 code/msg），
			// 再补 [DONE] 收尾。粘滞 lateErr 供 Close 后排查。
			t.lateErr = t.classifyStreamError(data)
			t.emitErrorFrame(t.lateErr)
			t.done = true
			t.pending = append(t.pending, "data: [DONE]\n\n"...)
		case "token_usage":
			t.emitUsage(data)
		case "done":
			t.emitDone(data)
			t.done = true
		default:
			continue // 过程事件不透传
		}
		if len(t.pending) > 0 {
			break
		}
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

// traeOutputEvent output 事件 data（response=正文增量，reasoning_content=思考增量）。
// 新形态兼容：部分版本 output 的 data 是 {"type":"text","content":...}。
type traeOutputEvent struct {
	Response         string `json:"response"`
	ReasoningContent string `json:"reasoning_content"`
	Content          string `json:"content"`
	Type             string `json:"type"`
}

// emitOutput 把一个 output 事件翻译成 OpenAI chunk 帧进 pending。
func (t *traeSSETranslator) emitOutput(data string) {
	var ev traeOutputEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return // 坏帧跳过（不阻断整流）
	}
	content := ev.Response
	if content == "" && ev.Type == "text" {
		content = ev.Content
	}
	if content == "" && ev.ReasoningContent == "" {
		return
	}
	delta := map[string]any{}
	if !t.sentRole {
		delta["role"] = "assistant"
		t.sentRole = true
	}
	if ev.ReasoningContent != "" {
		delta["reasoning_content"] = ev.ReasoningContent
	}
	if content != "" {
		delta["content"] = content
	}
	t.writeChunk(map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model":   t.modelOrDefault(),
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
}

// emitUsage 把 token_usage 事件翻译成带 usage 的 chunk（账本消费 usage 字段）。
func (t *traeSSETranslator) emitUsage(data string) {
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		ReasoningTokens  int64 `json:"reasoning_tokens"`
	}
	if err := json.Unmarshal([]byte(data), &u); err != nil {
		return
	}
	usage := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	t.writeChunk(map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": t.modelOrDefault(), "usage": usage,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}}},
	})
}

// emitDone 把 done 事件翻译成 finish_reason chunk + [DONE]。
func (t *traeSSETranslator) emitDone(data string) {
	var d struct {
		FinishReason string `json:"finish_reason"`
	}
	_ = json.Unmarshal([]byte(data), &d)
	fr := d.FinishReason
	if fr == "" {
		fr = "stop"
	}
	t.writeChunk(map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": t.modelOrDefault(),
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
	})
	t.pending = append(t.pending, "data: [DONE]\n\n"...)
}

// emitErrorFrame 把流内错误翻译成 OpenAI error 帧（sse.go error-passthrough 原样透传）。
func (t *traeSSETranslator) emitErrorFrame(ue *Error) {
	if ue == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": ue.Msg,
			"type":    "upstream_error",
			"code":    ue.Kind.String(),
		},
	})
	if err != nil {
		return
	}
	t.pending = append(t.pending, "data: "...)
	t.pending = append(t.pending, payload...)
	t.pending = append(t.pending, "\n\n"...)
}

// classifyStreamError 解析流内 error 帧并按实测码表分类。
func (t *traeSSETranslator) classifyStreamError(data string) *Error {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return &Error{Kind: ErrServer, Msg: "trae stream error (unparsed): " + truncate(data, 200)}
	}
	msg := e.Message
	if msg == "" {
		msg = e.Error
	}
	kind := traeClassifyStreamCode(e.Code)
	return &Error{Kind: kind, Status: traeErrStatus(kind, 500),
		Msg: fmt.Sprintf("trae stream error code=%d: %s", e.Code, msg)}
}

// writeChunk 序列化一帧 OpenAI chunk 进 pending。
func (t *traeSSETranslator) writeChunk(obj map[string]any) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return
	}
	t.pending = append(t.pending, "data: "...)
	t.pending = append(t.pending, raw...)
	t.pending = append(t.pending, "\n\n"...)
}

func (t *traeSSETranslator) modelOrDefault() string {
	if t.model != "" {
		return t.model
	}
	return "trae-default"
}

// Close 关闭底流并取消请求 ctx（释放连接），语义同 qoderSSEUnwrapper.Close。
func (t *traeSSETranslator) Close() error {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.rc != nil {
		return t.rc.Close()
	}
	return nil
}

// 防御：确保 translator 完整实现 io.ReadCloser。
var _ io.ReadCloser = (*traeSSETranslator)(nil)
