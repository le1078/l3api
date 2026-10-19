// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/provider"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	brand  string // 上游品牌 + 域的展示名（"CodeBuddy 国内版"），空 = 未选到号
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
// logModel 由调用方从 bodyFacts 单次解析产出（2026-10-20 合并：原先此处独自
// parseModelFromBody 解析 body，与调用方 peek 完全重复）。空串按 "-" 兜底。
func newChatStat(now time.Time, logModel string, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	if logModel == "" {
		logModel = "-"
	}
	return &chatStat{start: now, model: logModel, mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks, s.brand)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit bool
	credit    float64
	pend      []byte // 已读未返回的行缓存
	// lineBuf pend 的复用缓冲（2026-10-20：原先每行 []byte(line) 新分配一次，
	// 500 帧流 = 500 alloc）。SSE 行长稳定 → 第二次起零分配。
	lineBuf []byte
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
//
// 性能（2026-10-20）：预过滤 strings.Contains(payload, "usage") 置于解析之前。
// 上游 usage 只出现在末帧（带 usage 的帧占比 ~0.2%），过滤前每帧都要全量
// json.Unmarshal 一遍 5KB 的 delta 帧；过滤后约 100% 帧零 JSON 开销。
// TTFB 判定保持在过滤之前——首帧无论是否带 usage 都要计时。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	if !strings.Contains(payload, "usage") {
		return // 无 usage 字段的帧（绝大多数）：不付 JSON 解析开销
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     *int     `json:"prompt_tokens"`
			CompletionTokens *int     `json:"completion_tokens"`
			TotalTokens      *int     `json:"total_tokens"`
			Credit           *float64 `json:"credit"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
//
// 性能（2026-10-20）：pend 复用固定 buffer（原先每行 []byte(line) 新分配一次，
// 500 帧流 = 500 次 alloc）。lineBuf 容量按行长增长后保留——SSE 行长通常
// 稳定在几 KB，第二次起零分配。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	// 单行上界（2026-10-20）：与 upstream 读循环同口径——故障上游的无换行
	// 超长行会让 ReadString 无界扩容，统计 reader 也必须断流（错误经
	// WriteStream 返回给 handler 走 502）。
	if lerr := upstream.CheckSSELineLen(line, 0); lerr != nil {
		return 0, lerr
	}
	if line != "" {
		s.parseSSELine(line)
		// 复用 lineBuf：只在容量不足时增长（行长短稳定 → 第二次起零 alloc）。
		if cap(s.lineBuf) < len(line) {
			s.lineBuf = make([]byte, len(line))
		}
		s.pend = append(s.lineBuf[:0], line...)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
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

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
//
// 注意：chat 热路径已不再使用本函数（2026-10-20 起 model 由 bodyFacts 单次解析
// 产出，消除与调用方 peek 的重复解析）。保留供测试与非 chat 路径（如诊断）
// 单独查询模型名使用。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// brandLabel 返回账号归属的展示名（品牌 + 域），供对话日志行显示：
// codebuddy 分「国内版 / 国际版」（差在 realm：模型前缀 cn/global 走的其实是
// 两套完全不同的账号与配额），qoder / trae 各自一档。空串（未选到号）返回 "-"。
//
// 为什么不复用 provider.DisplayName()：那个面向「添加账号」的品牌选择项
//（"CodeBuddy (WorkBuddy)"、"Qoder (阿里)"），带公司归属；日志行每行都打，
// 要的是能对齐的短标签，且与前端 BRAND_LABEL 的口径一致（那里也是
// codebuddy / codebuddy-global / qoder / trae 四档）。
func brandLabel(brand, realm string) string {
	b := strings.ToLower(strings.TrimSpace(brand))
	if b == "" {
		b = string(provider.CodeBuddy) // 老凭证缺省，同 auth.Brand() 口径
	}
	switch provider.Brand(b) {
	case provider.Qoder:
		return "Qoder"
	case provider.Trae:
		return "Trae"
	case provider.CodeBuddy:
		if realm == "global" {
			return "CodeBuddy 国际版"
		}
		return "CodeBuddy 国内版"
	default:
		// 未知值原样透出（同 auth.Brand() 的脏数据可见策略）
		return brand
	}
}

// uidFull 展示完整 uid；空 uid 显示 "-"。
// 为什么不截前 8 位：日志是给人定位具体账号用的（对着面板「账号」页 Ctrl+F
// 找这个 uid），截断后多个同前缀号会撞车；行虽变长，但这是排障现场唯一能
// 直接复制去搜的东西。
func uidFull(uid string) string {
	if uid == "" {
		return "-"
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
//
// 模型名**不截断**（v1.16.13）：此前截到 11 个字符，`cn:deepseek-v4.1-flash`
// 只剩 `cn:deepseek`，面板里根本分不清调的是哪个模型。完整名交给面板按列宽
// 换行/省略；终端表格宽度由使用者自己调（日志是给人排障看的，宁可宽一点）。
//
// 品牌 + uid 前置到模型前面（v1.17.2）：一行日志要先能回答「哪个账号打的上游」，
// 再回答「用的什么模型」——前者才是「为什么这条慢 / 为什么这条收费」的第一归因
// 对象。之前只有 uid= 前 8 位挤在状态后面，既截断又排在了次要位置。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int, brand string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	if brand == "" {
		brand = "-"
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %s | %s | %d | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		brand,
		uidFull(uid),
		model,
		mode,
		status,
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
