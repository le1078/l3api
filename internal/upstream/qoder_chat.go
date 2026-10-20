// qoder_chat.go Qoder chat SSE 桥（M4 P2）。
//
// 链路：OpenAI 请求体 → Qoder chat 请求体 → Encode=1 编码 → Bearer COSY 签名 →
// POST gateway.qoder.com.cn/.../agent_chat_generation → 解包 SSE 信封（body 字段
// 是字符串化 JSON）→ 还原成 l3api 期望的 OpenAI SSE 帧流（data: {...}\n\n）。
//
// 协议依据 docs/qoder-protocol.md §2.2 / §3 / §4；签名实现见 qoder_bearer.go。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/session"
)

// qoderChatURL chat 端点全路径（Encode=1 编码通道）。
const qoderChatURL = qoderChatBase + qoderChatPath +
	"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// qoderUUID 生成带连字符的标准 UUID v4（与 Qoder 客户端抓包形态一致）。
// 注意：签名服务对 requestId 形态敏感——必须用带连字符的 36 字符形态，
// 用 session.NewMessageID（32 hex 无连字符）会得到 code 101 Signature invalid。
// session.NewMessageID 仍用于会话级 id 派生（request_set_id/session_id），
// 那些字段上游不校验格式，只校验 requestId。
func qoderUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 熵源故障的极端兜底：仍保证合法 UUID 形态（不可能发生，防御）。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qoderSessionID 会话级 id：进程内按 key 稳定复用（与 codebuddy 的会话头族同源，
// 用 session.RequestIDForKey 生成；Qoder 没有粘性会话概念，但 request_set_id /
// session_id 同轮复用可减少后台记账碎片）。
var qoderSessionID = session.RequestIDForKey

// qoderChatPayload agent_chat_generation 请求体（编码前明文 JSON 的子集）。
// 抓包实测字段全集见 docs/qoder-protocol.md §3；这里只带上游消费必需项 +
// 稳定会话字段，缺省值沿用客户端真实形态（stream:true / chat_task:FREE_INPUT 等）。
type qoderChatPayload struct {
	RequestID    string         `json:"request_id"`
	RequestSetID string         `json:"request_set_id"`
	ChatRecordID string         `json:"chat_record_id"`
	SessionID    string         `json:"session_id"`
	Stream       bool           `json:"stream"`
	ChatTask     string         `json:"chat_task"`
	Source       string         `json:"source"`
	Version      string         `json:"version"`
	AgentID      string         `json:"agent_id"`
	TaskID       string         `json:"task_id"`
	SessionType  string         `json:"session_type"`
	ModelConfig  qoderModelCfg  `json:"model_config"`
	System       string         `json:"system"`
	Messages     []qoderMessage `json:"messages"`
	Tools        []any          `json:"tools"`
	Parameters   map[string]any `json:"parameters"`
	Business     map[string]any `json:"business"`
}

type qoderModelCfg struct {
	Key    string `json:"key"`
	Format string `json:"format"`
	Source string `json:"source"`
}

// qoderMessage chat 消息元素。抓包实测：content 是字符串、contents 是
// 结构化分片 [{type:"text",text:...}]。system 角色也用同一形状（content 直接放
// 提示词全文），上游不校验角色值——透传即可。
//
// ToolCalls/ToolCallID 是工具往返的必需字段（2026-10-21 补）：Qoder 网关按
// OpenAI 规范**严格校验 tool_call ↔ tool 结果配对**——assistant 的 tool_calls
// 缺 id 或 tool 结果缺 tool_call_id，上游直接反序列化失败并回
// provider_error（实测原文："messages[2]: missing field `tool_call_id`"）。
// 此前本桥只透传 role/content，把 tool_calls 整个丢了，于是「调用工具 → 回传
// 结果」的第二轮必然失败，客户端表现为模型答非所问或空流。
type qoderMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	Contents   []qoderContentPart `json:"contents"`
	ToolCalls  []qoderToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

// qoderToolCall OpenAI 形态的工具调用（Qoder 网关按 OpenAI 形态消费：
// {"id","type","function":{"name","arguments"}}）。
type qoderToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function qoderToolCallFunc `json:"function"`
}

type qoderToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type qoderContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// openaiChatRequest l3api 收到的 OpenAI chat 请求体（仅消费本桥需要的字段）。
// messages[].content 可能是字符串或多模态分片数组——两种情况都兼容取文本。
//
// ToolCalls/ToolCallID 随消息解析（2026-10-21 补）：工具往返的配对信息必须
// 原样带到上游，否则第二轮请求被网关拒绝。
type openaiChatRequest struct {
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []qoderToolCall `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
	Tools []any `json:"tools"`
}

// qoderBuildChatBody 把 OpenAI 请求体转成 Qoder chat 请求体（明文 JSON）。
// conversationID 用于会话级 id 复用（空则生成一次性随机）。
// model 是客户端请求的裸模型 key（qfmodel/kmodel_latest/...）；空则回落 qfmodel
// （Qwen3.8-Flash，与 Qoder 客户端缺省模型一致）。
func qoderBuildChatBody(body []byte, conversationID, model string) ([]byte, error) {
	var req openaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("qoder: parse openai body: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("qoder: no messages in request")
	}
	if model == "" {
		model = "qfmodel" // 缺省 Flash（Qoder 客户端默认；上游对空 key 报参数错误）
	}

	sessKey := conversationID
	if sessKey == "" {
		sessKey = "qoder-once-" + qoderUUID()
	}
	sessionID := qoderSessionID(sessKey)
	if sessionID == "" {
		sessionID = qoderUUID()
	}

	// system 提示词：取 role=system/developer 的 content 文本（多模态分片取 text）。
	// Qoder 顶层 system 是字符串（非数组），抓包实测。
	system := ""
	var msgs []qoderMessage
	for _, m := range req.Messages {
		text := qoderContentText(m.Content)
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			if system == "" {
				system = text
			}
			continue // system 不进 messages
		default:
			msgs = append(msgs, qoderMessage{
				Role:       m.Role,
				Content:    text,
				Contents:   []qoderContentPart{{Type: "text", Text: text}},
				ToolCalls:  m.ToolCalls,
				ToolCallID: m.ToolCallID,
			})
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("qoder: no non-system messages")
	}

	p := qoderChatPayload{
		RequestID:    qoderUUID(),
		RequestSetID: sessionID,
		ChatRecordID: qoderUUID(),
		SessionID:    sessionID,
		Stream:       true,
		ChatTask:     "FREE_INPUT",
		Source:       "1",
		Version:      "3",
		AgentID:      "agent_common",
		TaskID:       "common",
		SessionType:  "app",
		ModelConfig: qoderModelCfg{
			Key:    model,
			Format: "openai",
			Source: "system",
		},
		System:     system,
		Messages:   msgs,
		Tools:      req.Tools,
		Parameters: map[string]any{},
		Business:   map[string]any{},
	}
	return json.Marshal(p)
}

// qoderContentText 把 OpenAI message.content（字符串或多模态分片数组）转成纯文本。
// 多模态分片只取 type:"text" 的 text；图片等其余分片丢弃（Qoder 网关不接视觉输入）。
func qoderContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 快路径：纯字符串。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 慢路径：分片数组（兼容 OpenAI vision 形态）。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// QoderChatStream Qoder chat SSE 桥：发送 Encode=1 编码的 chat 请求，
// 返回**解包后**的 OpenAI SSE 帧流（data: {...}\n\n），供 handler 复用
// StreamHint/newChatStatsReaderSince 的既有消费管线。
//
// 返回的 *Error 与 codebuddy 路径同语义：Kind 分类 + RetryAfter。
// 传输层错误返回非 *Error（err 为裸 fmt error），由 handler 走抖动换号分支。
//
// conversationID 用于会话级 id（request_set_id/session_id）的进程内稳定复用：
// 同一会话的多轮请求共享，减少后台记账碎片。空则生成一次性随机。
// model 是客户端请求的裸模型 key（qfmodel/kmodel_latest/...，剥掉 qoder: 前缀），
// 空则回落 qfmodel（Flash，与 Qoder 客户端缺省一致）。
func (c *Client) QoderChatStream(ctx context.Context, a *auth.Auth, body []byte, conversationID, model string) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// jobToken 滚动刷新（AccessToken 即当前 jobToken；NeedsRefresh 判过期窗口）。
	// 单飞（2026-10-20）：同账号并发请求（max_in_flight=8）同时进过期窗口时
	// 只有 leader 真打 refresh 端点——否则 8 次滚动刷新互相覆盖 auth 字段，
	// 且风控视角是同 uid 短时多次刷 token。四条 qoder 路径共用同一单飞组
	//（qoderRefreshSingleflight）：chat/modelsync/checkin/RefreshIfNeeded。
	if a.NeedsRefresh(qoderRefreshSkew) {
		if a.RefreshToken == "" {
			return nil, 0, nil, &Error{Kind: ErrSessionDead, Status: 401,
				Msg: "qoder refresh_token missing"}
		}
		_ = c.qoderRefreshSingleflight(a)
		// 单飞完成仍临期 = 刷新失败（保持旧 token）：按 session dead 换号
		// （与旧实现的错误分类一致：调用方走 ErrSessionDead 阈值化禁用）。
		if a.NeedsRefresh(qoderRefreshSkew) {
			return nil, 0, nil, &Error{Kind: ErrSessionDead, Status: 401,
				Msg: "qoder token refresh failed"}
		}
	}
	if a.UID == "" || a.AccessToken == "" {
		return nil, 0, nil, &Error{Kind: ErrSessionDead, Status: 401,
			Msg: "qoder auth incomplete (uid/jobToken missing)"}
	}

	// 1) 构造请求体并编码（model 缺省回落在 buildChatBody 内统一处理）。
	plain, err := qoderBuildChatBody(body, conversationID, model)
	if err != nil {
		return nil, 0, nil, err
	}
	encoded := QoderEncode(plain)
	// model 的生效值：buildChatBody 已做缺省回落，这里用同一口径取（头与体一致）。
	modelKey := model
	if modelKey == "" {
		modelKey = "qfmodel"
	}

	// 2) 构造 Bearer COSY（同一 cosyDate 进签名与 Cosy-Date 头）。
	identity := qoderCosyIdentity{UID: a.UID, SecurityOauthToken: a.AccessToken}
	bearer, cosyKey, cosyDate, err := QoderBuildBearer(identity, qoderChatURL, encoded, qoderUUID())
	if err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: build bearer: %w", err)
	}

	// 3) 发送请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qoderChatURL,
		bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, 0, nil, err
	}
	machine := a.UID // 机器指纹与账号绑定（无桌面 Turing SDK 部署的稳定替代）
	qoderSetChatHeaders(req, bearer, cosyKey, cosyDate, machine, modelKey, a.UID)

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
			return nil, 0, nil, fmt.Errorf("qoder: read err body: %w", rerr)
		}
		kind := qoderClassifyHTTP(resp.StatusCode, string(raw))
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		return nil, resp.StatusCode, raw, ue
	}

	// 4) 成功流：包一层信封解包器，把 data:{"body":"..."} 还原成 data:{...}。
	rc := &qoderSSEUnwrapper{
		rc:     monitorBody(resp.Body, c.IdleTimeout, c.StreamTotalTimeout, cancel),
		cancel: cancel,
	}
	return rc, resp.StatusCode, nil, nil
}

// qoderRefreshSkew jobToken 刷新窗口：30 分钟（与 handler.cfg.RefreshSkew 同量级，
// jobToken 24h 有效期，早刷无害，晚刷可能在请求中途过期——保守取大值）。
const qoderRefreshSkew = 30 * time.Minute

// qoderQuotaResp GET /api/v2/quota/usage 响应（明文，openapi 域）。
type qoderQuotaResp struct {
	UserQuota  qoderQuotaBucket `json:"userQuota"`
	AddOnQuota qoderQuotaBucket `json:"addOnQuota"`
	// IsQuotaExceeded 整体超额标记（配额为 0 时 true）。
	IsQuotaExceeded bool `json:"isQuotaExceeded"`
}

type qoderQuotaBucket struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`
}

// QoderQuota 查询 Qoder 余额（credits）。返回 (remain, total)。
// 数值是浮点 credits（抓包实测 300.0），转成 int64 时四舍五入到整数——
// l3api 的 credits 账本全是 int64（codebuddy 积分也是整数），小数无意义。
//
// remain = userQuota.remaining + addOnQuota.remaining（主额度 + 加油包）；
// total 同理。两个桶任一缺失（字段为零值）不影响另一个。
func (c *Client) QoderQuota(ctx context.Context, a *auth.Auth) (remain, total int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderOpenapiBase+"/api/v2/quota/usage", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, 0, fmt.Errorf("qoder quota %d: %s", resp.StatusCode, string(b))
	}
	var out qoderQuotaResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, fmt.Errorf("qoder quota parse: %w", err)
	}
	remain = int64(out.UserQuota.Remaining + out.AddOnQuota.Remaining + 0.5)
	total = int64(out.UserQuota.Total + out.AddOnQuota.Total + 0.5)
	return remain, total, nil
}

// qoderSetChatHeaders 按真实抓包 v1.1.57 形态写 Qoder chat 请求头族。
// machine 是稳定指纹（同一账号复用同一值，避免被当作多设备漂移）。
// modelKey 进 X-Model-Key 头（与 body.model_config.key 一致）。
// uid 进 Cosy-User 头——**缺失会直接导致 code 101 Signature invalid**：
// 网关用 Cosy-User 与解密后的 identity.uid 做一致性校验，
// 头缺了服务端就不知道该拿哪份 uid 去对齐签名（Python 参考实现与
// 客户端抓包都带这个头，见 scripts/qoder_live.py 的 headers）。
func qoderSetChatHeaders(req *http.Request, bearer, cosyKey, cosyDate, machine, modelKey, uid string) {
	h := req.Header
	h.Set("Accept", "text/event-stream")
	h.Set("Authorization", bearer)
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/json")
	h.Set("Cosy-Business-Product", "app")
	h.Set("Cosy-Business-Type", "agent")
	h.Set("Cosy-ClientType", "10")
	h.Set("Cosy-Data-Policy", "agree")
	h.Set("Cosy-Date", cosyDate)
	h.Set("Cosy-Key", cosyKey)
	h.Set("Cosy-MachineId", machine)
	h.Set("Cosy-MachineToken", machine)
	h.Set("Cosy-MachineType", "5")
	h.Set("Cosy-Scene", "app")
	h.Set("Cosy-User", uid)
	h.Set("Cosy-Version", qoderCosyVersion)
	h.Set("Login-Version", "v2")
	h.Set("X-Model-Key", modelKey)
	h.Set("X-Model-Source", "system")
	h.Set("User-Agent", "undici")
}

// qoderClassifyHTTP 非 200 状态的分类（chat 端点 HTTP 层错误形态）。
// 与 codebuddy 的 Classify 同语义面：Kind 决定 handler 罚不罚号。
// 注意：Bearer COSY 签名错误（code 101 "Signature invalid"）属于实现 bug，
// 不应罚号——归 ErrClient（换号重试无意义，但保留轮转以兼容真正的号间差异）。
func qoderClassifyHTTP(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == 401 || status == 403:
		return ErrSessionDead
	case strings.Contains(lower, "signature invalid"):
		// 101 签名错误：我们的构造与上游预期不符，是 bug 而非账号问题。
		return ErrClient
	case status == 429 || strings.Contains(lower, "rate limit") || strings.Contains(body, "限流"):
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

// ────────────────────────────────────────────────────────────────────────────
// SSE 信封解包
// ────────────────────────────────────────────────────────────────────────────

// qoderSSEUnwrapper 把 Qoder SSE 信封（data:{"body":"<JSON 字符串>"}）解包成
// l3api 期望的 OpenAI SSE 帧（data: {...}\n\n），并检测流内认证失败
// （statusCodeValue 401/403 或 body.code 105）——这类错误不进 handler 的
// StreamHint 消费管线，必须以 *Error 形态提前暴露给池子换号。
//
// 帧边界：上游按 \n\n 分帧；data: 前缀后接 JSON 对象，其 body 字段是字符串化
// 的内层 JSON（OpenAI 兼容 choices[].delta...）。[DONE] 帧原样透传。
type qoderSSEUnwrapper struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	br      *bufio.Reader
	pending []byte // 已解包待消费的字节（含 "data: {...}\n\n"）
	err     error  // 粘滞错误（读到底即返回）
	done    bool
	// authErr 流内认证失败的粘滞标记：读到时立即转成 *Error 返回，
	// 避免把错误帧当正常数据透传给 handler（handler 不会二次分类流内帧）。
	authErr *Error
}

// Read 实现 io.Reader：把解包后的帧流喂给上层（Aggregate/StreamHint）。
// 单消费者契约（monitorBody → Aggregate / StreamHint 串行读），无锁。
// 行为约定：读到流内认证错误时，先把已缓冲的 pending 发完，再返回 *Error。
// 这样 handler 至少能看到部分已产出的内容（与 codebuddy 路径的流内错误帧一致）。
func (u *qoderSSEUnwrapper) Read(p []byte) (int, error) {
	if len(u.pending) > 0 {
		n := copy(p, u.pending)
		u.pending = u.pending[n:]
		return n, nil
	}
	if u.authErr != nil {
		return 0, u.authErr
	}
	if u.done {
		return 0, io.EOF
	}
	if u.err != nil {
		return 0, u.err
	}
	if u.br == nil {
		u.br = bufio.NewReaderSize(u.rc, 64*1024)
	}
	if err := u.fill(); err != nil {
		var ue *Error
		switch {
		case errors.As(err, &ue):
			u.authErr = ue
			if len(u.pending) == 0 {
				return 0, ue
			}
		case errors.Is(err, io.EOF):
			u.done = true
			if len(u.pending) == 0 {
				return 0, io.EOF
			}
		default:
			u.err = err
			if len(u.pending) == 0 {
				return 0, err
			}
		}
	}
	if len(u.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}

// fill 从上游读到下一帧完整 SSE 块（以空行分隔），解包后写进 pending。
// 返回 nil 表示成功填了一帧；io.EOF 表示流正常结束（上游关闭连接且无残留数据）；
// 其余为错误或流内认证失败。
//
// 帧边界按 SSE 规范：块以空行（\n 或 \r\n）结束。空块（连续空行）跳过继续读，
// 不计为帧也不结束——SSE 规范允许空行作为 keepalive 注释位，io.ReadAll 依赖
// 这一区分才能读到流真正结束。
func (u *qoderSSEUnwrapper) fill() error {
	var frame strings.Builder
	for {
		line, err := u.br.ReadString('\n')
		// 单行上界（2026-10-20）：qoder 信封帧含 base64 body，但无换行超长行
		// 只可能是上游故障——超限断流（调用方走既有解析失败路径）。
		if lerr := CheckSSELineLen(line, 0); lerr != nil {
			return lerr
		}
		if err != nil {
			if err == io.EOF && frame.Len() > 0 {
				// 尾帧无换行：处理已累积的帧（不清 frame，下方照常 emit）。
				break
			}
			return err
		}
		frame.WriteString(line)
		if line == "\n" || line == "\r\n" {
			break // 空行 = 一帧结束
		}
	}
	text := frame.String()
	if strings.TrimSpace(text) == "" {
		// 空块（只有空行）：不是结束信号，继续读下一帧。
		// 递归深度受上游连续空行数约束——正常 SSE 不会连续发大量空行，
		// 即便有，深度也远小于栈上限（每帧一次调用）。
		return u.fill()
	}
	return u.emit(text)
}

// emit 解析一帧 SSE 文本（可能含多行 data:），把解包结果写进 pending。
// 帧内认证失败返回 *Error（粘滞到 authErr）。
func (u *qoderSSEUnwrapper) emit(frame string) error {
	var out strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			// 非信封形态（防御）：原样透传。
			out.WriteString("data: " + payload + "\n\n")
			continue
		}
		// 流内认证失败检测（statusCodeValue 401/403，或 body.code 105）。
		if scv, ok := env["statusCodeValue"].(float64); ok && (int(scv) == 401 || int(scv) == 403) {
			msg := qoderEnvMessage(env)
			return &Error{Kind: ErrSessionDead, Status: int(scv),
				Msg: fmt.Sprintf("qoder in-stream auth error %d: %s", int(scv), msg)}
		}
		bodyStr, _ := env["body"].(string)
		var inner map[string]any
		if bodyStr != "" && json.Unmarshal([]byte(bodyStr), &inner) == nil {
			if code, ok := inner["code"]; ok {
				cs := fmt.Sprintf("%v", code)
				if cs == "105" || cs == "105.0" {
					return &Error{Kind: ErrSessionDead, Status: 403,
						Msg: "qoder login expired (code 105)"}
				}
			}
			// 流内业务错误（provider_error 等，2026-10-21 补）：Qoder 网关把
			// 上游模型侧的错误（如请求体反序列化失败）包在 HTTP 200 的 SSE 信封里
			// 回传——statusCodeValue=400 + body.code="provider_error"。
			//
			// 此前这类帧被当成正常数据透传：客户端只收到一串空帧 + [DONE]，
			// 既看不到错误原因，也无从判断该换号重试——表现为「模型跑几秒就断开」
			// 且无任何报错。必须在此转成 *Error，让 handler 走错误分类路径。
			//
			// 同时把错误写成 OpenAI error 帧透出（error-passthrough 语义，与
			// codebuddy 路径一致）：客户端必须看到真实原因（上游原文），
			// 而不是「连接被截断」。该帧走 pending → StreamHint 的 error 分支，
			// 会以 *StreamFrameError 回到 handler 供分类罚号。
			if ue := qoderStreamError(inner, env); ue != nil {
				out.WriteString("data: " + qoderErrorFrame(ue) + "\n\n")
				u.pending = append(u.pending, out.String()...)
				// 错误已透出给客户端；粘滞返回 *Error 让 handler 分类。
				return ue
			}
			// 内层是 OpenAI 兼容帧：原样写回（id/created/choices/usage 等）。
			out.WriteString("data: " + bodyStr + "\n\n")
			continue
		}
		// 无 body 字段或非 JSON：原样透传（防御）。
		out.WriteString("data: " + payload + "\n\n")
	}
	u.pending = append(u.pending, out.String()...)
	return nil
}

// qoderErrorFrame 把流内错误渲染成 OpenAI error 帧（JSON 字符串）。
// 与 upstream 通用 error 帧形态一致（{"error":{"message","type","code"}}），
// 供 sse.go 的 error-passthrough 路径原样写出。
func qoderErrorFrame(ue *Error) string {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": ue.Msg,
			"type":    "upstream_error",
			"code":    ue.Kind.String(),
		},
	})
	if err != nil {
		return `{"error":{"message":"qoder upstream error","type":"upstream_error"}}`
	}
	return string(payload)
}

// qoderStreamError 判定 SSE 信封内层是否为上游业务错误帧，是则返回 *Error。
//
// 判定口径（2026-10-21 实测抓包）：
//
//	信封: {"headers":{...},"body":"<内层 JSON 字符串>","statusCodeValue":400,"statusCode":"BAD_REQUEST"}
//	内层: {"code":"provider_error","message":"Error in upstream response","type":"provider_error",
//	       "details":"{\"error\":{\"message\":\"Failed to deserialize the JSON body ...\"}}"}
//
// 关键：**HTTP 状态码是 200**，错误只在 SSE 信封里——所以 HTTP 层分类完全
// 看不到它，只能在解包处识别。识别后必须转成 *Error，否则会被当成正常帧
// 透传给客户端（客户端表现为「空流 + 无原因」）。
//
// 分类口径：内层 details 里携带真实原因（上游模型的报错原文），按 Classify
// 归类。归类为请求级错误（ErrBadParams）时不罚账号——同一个畸形 body 换任何
// 号都会失败，罚号是白扔健康号配额。
func qoderStreamError(inner map[string]any, env map[string]any) *Error {
	// 只有 provider_error / error 形态才算业务错误；正常的 OpenAI 兼容帧
	// （含 "code":"0" 之类的成功码）不走这里。
	code := fmt.Sprintf("%v", inner["code"])
	if code != "provider_error" && code != "error" {
		return nil
	}
	// 信封层状态码（400/500…）作为 HTTP 语义；缺失时按 400 兜底（上游错误帧
	// 绝大多数是 4xx 请求级错误）。
	status := 400
	if scv, ok := env["statusCodeValue"].(float64); ok && int(scv) >= 400 {
		status = int(scv)
	}
	// 真实原因在 details（JSON 字符串）里；退而求其次用 message。
	detail := qoderErrorDetail(inner)
	msg := detail
	if msg == "" {
		msg, _ = inner["message"].(string)
	}
	if msg == "" {
		msg = "qoder upstream stream error"
	}
	// 按真实原因分类：请求体畸形（ErrBadParams）不罚号，其余按上游语义归类。
	kind := Classify(status, detail)
	if kind == ErrNone {
		kind = ErrClient
	}
	// 上游模型侧的反序列化失败（"Failed to deserialize the JSON body into the
	// target type: messages[2]: missing field `tool_call_id`"）是**请求体**问题，
	// 不是账号问题——同一个畸形 body 换任何号都会失败。归 ErrBadParams：
	// 不冷却/不熔断/不计错，但仍轮转（不同账号模型权限可能不同）。
	//
	// 通用 Classify 不认识这个 marker（它是 Qoder 模型侧的报错，不在 codebuddy
	// 的 marker 家族里），故在此本地判定。
	if isQoderDeserializeError(detail) {
		kind = ErrBadParams
	}
	return &Error{Kind: kind, Status: status,
		Msg: fmt.Sprintf("qoder in-stream error %d: %s", status, msg)}
}

// isQoderDeserializeError 上游模型侧的反序列化失败 marker（请求体畸形）。
func isQoderDeserializeError(detail string) bool {
	return strings.Contains(detail, "Failed to deserialize the JSON body") ||
		strings.Contains(detail, "missing field")
}

// qoderErrorDetail 从 provider_error 内层提取真实报错原文。
// details 是 JSON 字符串（内含 {"error":{"message":...}}），解析失败返回空串。
func qoderErrorDetail(inner map[string]any) string {
	raw, _ := inner["details"].(string)
	if raw == "" {
		return ""
	}
	var d struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &d) != nil {
		return ""
	}
	return d.Error.Message
}

// qoderEnvMessage 从信封里提取错误文案（body.message 或顶层 message）。
func qoderEnvMessage(env map[string]any) string {
	if body, _ := env["body"].(string); body != "" {
		var inner map[string]any
		if json.Unmarshal([]byte(body), &inner) == nil {
			if m, _ := inner["message"].(string); m != "" {
				return m
			}
		}
	}
	if m, _ := env["message"].(string); m != "" {
		return m
	}
	return ""
}

// Close 关闭底流并取消请求 ctx（释放连接）。
// monitorBody 的 Close 已经会 cancel；这里再 cancel 一次是幂等的（context.CancelFunc
// 可重入），双保险保证读侧提前退出时连接也能释放。
func (u *qoderSSEUnwrapper) Close() error {
	if u.cancel != nil {
		u.cancel()
		u.cancel = nil
	}
	if u.rc != nil {
		return u.rc.Close()
	}
	return nil
}
