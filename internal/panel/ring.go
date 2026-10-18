// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
	// Row 是对话表格行的**结构化解析结果**（仅 Ch==ChChat 且解析成功时非 nil）。
	// 为什么在写入口解析（v1.16.13）：对话行里那串 `| #123 | 06:58:44 | model |`
	// 本身带一个时刻，而 LogEntry.TS 又有一个时刻——前端两者都显示，用户看到
	// 「每行有两个时间」不知道谁是谁。解析后前端只渲染 Row 的结构化字段，
	// 行内时刻不再重复展示；解析失败（老格式/手写日志）退回纯文本渲染。
	Row *ChatRow `json:"row,omitempty"`
}

// ChatRow 一条对话表格日志的解析结果（对应 server.logging.logChatRow 的输出格式）。
//
// 格式：| #138 | 06:58:44 | cn:deepseek-v4.1-flash | stream | 200 | uid=55a0be20 |
// TTFB=7989ms | tok=948 | 76.2tok/s | total=12.4s |
type ChatRow struct {
	Seq     int     `json:"seq"`
	At      string  `json:"at"`                // 行内本地时刻（HH:MM:SS），前端不单独展示（用 TS）
	Model   string  `json:"model"`             // 完整模型名（不截断）
	Mode    string  `json:"mode"`              // stream | sync
	Status  int     `json:"status"`            // HTTP 状态码
	UID     string  `json:"uid"`               // 前 8 位
	TTFBMs  int     `json:"ttfb_ms,omitempty"` // 0 = 未知
	Tokens  int     `json:"tokens"`            // -1 = 上游没给 usage
	TokPerS float64 `json:"tok_s"`             // 0 = 未知
	TotalMs int     `json:"total_ms"`
}

// chatRowRe 解析 chat 表格行。字段容忍缺失（老格式/异常行）——解析不出来
// 就整体退回纯文本，不让日志查看功能因格式演进失效。
var chatRowRe = regexp.MustCompile(
	`^\| #(\d+) \| ([0-9:]+) \| (.+?) \| (\w+) \| (\d+) \| uid=(\S*) \| TTFB=(\S+) \| tok=(\S+) \| (\S+)tok/s \| total=([0-9.]+)s \|$`)

// parseChatRow 解析一行 chat 表格日志；不匹配返回 nil。
func parseChatRow(line string) *ChatRow {
	m := chatRowRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	row := &ChatRow{
		Seq:    atoiDefault(m[1], 0),
		At:     m[2],
		Model:  m[3],
		Mode:   m[4],
		Status: atoiDefault(m[5], 0),
		UID:    m[6],
		Tokens: atoiDefault(m[8], -1),
	}
	// TTFB=7989ms / TTFB=- ；tok=- 表示无 usage
	if v := strings.TrimSuffix(m[7], "ms"); v != "-" {
		row.TTFBMs = atoiDefault(v, 0)
	}
	if v := strings.TrimSuffix(m[9], ""); v != "-" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			row.TokPerS = f
		}
	}
	if f, err := strconv.ParseFloat(m[10], 64); err == nil {
		row.TotalMs = int(f * 1000)
	}
	return row
}

// atoiDefault 解析失败或空串时返回 def。
func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 开学季",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		ch := classifyLine(text)
		e := LogEntry{TS: now, Ch: ch, Text: text}
		if ch == ChChat {
			e.Row = parseChatRow(text)
		}
		r.entries = append(r.entries, e)
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
