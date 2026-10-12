// Package notify 账号池事件的外发告警（v1.11）。
//
// 背景：保号场景最大的盲区是「静默缩水」——账号被禁用/熔断/积分耗尽时进程
// 照常跑，池子在无人知晓的情况下变小。本包把关键状态演进外推到用户常用的
// 推送渠道（Telegram/Bark/企业微信/钉钉/通用 webhook），单机零依赖实现。
//
// 设计约束：
//   - 非阻塞：Post 永不卡业务路径——队列满即丢弃并计数（告警可丢，业务不可等）；
//   - 去重：同 (event, subject) 在 cooldown 窗口内只发一次（防上游错误风暴刷屏）；
//   - 热更：面板保存配置后 Store 整体替换快照，无需重启；
//   - nil 安全：*Notifier 为 nil 或未启用时全部入口静默返回，调用方免判空
//     （与 usage.Recorder 同风格）。
package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 事件类型（config notify.events 订阅名单用；空名单 = 全部订阅）。
const (
	EventAccountDisabled = "account_disabled" // 账号被自动禁用（session dead / 11140 封禁）
	EventBreakerOpen     = "account_breaker"  // 连续失败触发熔断（指数退避冷却）
	EventCreditExhausted = "credit_exhausted" // 积分耗尽硬冷却到次日 04:00
	EventCheckinFailed   = "checkin_failed"   // 签到任务失败（按轮汇总，一单一告警）
	EventBalanceLow      = "balance_low"      // 账号余额低于阈值（阈值 notify.balance_low_credits）
	EventPoolExhausted   = "pool_exhausted"   // 轮转后无可用账号（请求 503）
	EventModelSync       = "model_sync"       // 品牌模型目录自动同步（新增/下架/变更/实测未过汇总）
	EventSessionDead     = "session_dead"     // 账号 session 失效观测（余额/签到 401；未禁用也告警，仅跳变时发一次）
	EventProbeFailed     = "probe_failed"     // 定时体检失败（按轮汇总，一单一告警）
	// EventFreeQuotaExhausted 免费额度尽（free_guard）：账号在生效的免费促销窗口内
	// 实测扣费（usage.credit>0），判定该 (账号, 模型) 免费额度已尽。仅首次跳变发一次
	// （pool.MarkFreeExhausted 幂等：台账未过期不重复告警），复位点过后重新可发。
	EventFreeQuotaExhausted = "free_quota_exhausted"
)

// allEvents 事件全集（订阅校验用）。
var allEvents = []string{
	EventAccountDisabled, EventBreakerOpen, EventCreditExhausted,
	EventCheckinFailed, EventBalanceLow, EventPoolExhausted, EventModelSync,
	EventSessionDead, EventProbeFailed, EventFreeQuotaExhausted,
}

// Channel 单个推送渠道声明（config notify.channels 数组元素）。
type Channel struct {
	// Type 渠道类型："telegram" / "bark" / "wecom" / "dingtalk" / "generic" / "smtp"。
	Type string `json:"type"`
	// URL 渠道端点（smtp 类型不用，走下方 Smtp* 字段）：
	//   - telegram: https://api.telegram.org/bot<TOKEN>/sendMessage
	//   - bark:     https://api.day.app/<device_key>
	//   - wecom:    https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...
	//   - dingtalk: https://oapi.dingtalk.com/robot/send?access_token=...
	//   - generic:  任意接收 {"event","subject","title","body"} JSON POST 的端点
	URL string `json:"url,omitempty"`
	// Secret 附加参数：telegram 的 chat_id；dingtalk 加签密钥（可选）。
	Secret string `json:"secret,omitempty"`

	// ── SMTP 邮箱渠道字段（type=="smtp" 时使用，任意服务商通用）──
	// SmtpHost SMTP 服务器地址（如 smtp.qq.com / smtp.163.com / smtp.gmail.com）。
	SmtpHost string `json:"smtp_host,omitempty"`
	// SmtpPort 端口，0 = 465（SSL 直连）。465→SSL，其余默认 STARTTLS。
	SmtpPort int `json:"smtp_port,omitempty"`
	// SmtpUsername 发信账号（一般是邮箱地址）。
	SmtpUsername string `json:"smtp_username,omitempty"`
	// SmtpPassword 授权码 / 客户端专用密码（QQ/163/Gmail 都不是登录密码）。
	SmtpPassword string `json:"smtp_password,omitempty"`
	// SmtpFrom 发件人地址，缺省 = SmtpUsername。
	SmtpFrom string `json:"smtp_from,omitempty"`
	// SmtpTo 收件人列表（至少一个）。
	SmtpTo []string `json:"smtp_to,omitempty"`
	// SmtpTLS 加密方式："ssl" | "starttls" | "none"；空 = 按端口推断。
	SmtpTLS string `json:"smtp_tls,omitempty"`
}

// Config 通知配置（config.json notify 段）。
type Config struct {
	Enabled bool `json:"enabled"`
	// Events 订阅的事件名单（空 = 全部）；未知事件名报错（fail fast）。
	Events []string `json:"events,omitempty"`
	// Channels 推送渠道（enabled=true 时至少一个）。
	Channels []Channel `json:"channels,omitempty"`
	// Cooldown 同 (event, subject) 的最小告警间隔，默认 "30m"。
	Cooldown string `json:"cooldown,omitempty"`
	// BalanceLowCredits 余额告警阈值（credits 原始单位，0 = 关闭该事件）。
	// 值放在 notify 段（而非 schedule）：它是告警语义，不参与调度。
	BalanceLowCredits int64 `json:"balance_low_credits,omitempty"`
}

// snapshot Notifier 运行期的不可变配置视图（Store 热替换单位）。
type snapshot struct {
	cfg        Config
	cooldown   time.Duration
	subscribed map[string]bool
	channels   []Channel
}

// Poster 告警投递接口（pool/scheduler/handler 依赖此接口而非具体类型）。
type Poster interface {
	// Post 投递一条告警。event 为事件类型常量；subject 为去重主体（uid 或
	// 任务名，空 = 全局）；title/body 为人类可读文案。实现必须非阻塞。
	Post(event, subject, title, body string)
}

// Notifier 非阻塞告警投递器。
type Notifier struct {
	cfg atomic.Pointer[snapshot]
	hc  *http.Client

	queue chan notification
	stop  chan struct{}
	done  chan struct{}

	// dedupMu/dedup 同 (event, subject) 上次发送时刻（Post 侧判重，worker 外）。
	dedupMu sync.Mutex
	dedup   map[string]time.Time

	dropped atomic.Int64 // 队列满丢弃计数（观测）
}

// notification 一条待投递消息。
type notification struct {
	event   string
	subject string
	title   string
	body    string
}

// queueSize 告警队列容量：告警风暴时丢最旧的入不了队，业务路径零阻塞。
const queueSize = 64

// New 构建并启动投递 worker。cfg 无效（enabled 但无渠道）时仍返回可用实例，
// 状态为"启用但无渠道"（Post 进队列后丢弃，调用方不必特判）。
func New(cfg Config) *Notifier {
	n := &Notifier{
		hc:    &http.Client{Timeout: 10 * time.Second},
		queue: make(chan notification, queueSize),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		dedup: map[string]time.Time{},
	}
	n.Store(cfg)
	go n.run()
	return n
}

// Store 热替换配置（面板保存路径）。去重时间线保留。
func (n *Notifier) Store(cfg Config) error {
	s := &snapshot{cfg: cfg, cooldown: 30 * time.Minute}
	if cfg.Cooldown != "" {
		d, err := time.ParseDuration(cfg.Cooldown)
		if err != nil {
			return fmt.Errorf("notify.cooldown: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("notify.cooldown: 不能为负数")
		}
		s.cooldown = d
	}
	s.subscribed = map[string]bool{}
	if len(cfg.Events) == 0 {
		for _, e := range allEvents {
			s.subscribed[e] = true
		}
	} else {
		for _, e := range cfg.Events {
			if !isKnownEvent(e) {
				return fmt.Errorf("notify.events: %q 不是已知事件（可用: %s）", e, strings.Join(allEventsSorted(), ", "))
			}
			s.subscribed[e] = true
		}
	}
	// 渠道清理：空 url 丢弃（防御脏配置）；类型未知丢弃（宁可少发不发错）。
	for _, c := range cfg.Channels {
		if c.Type == "smtp" {
			// smtp 渠道无 URL，校验自己的必备字段
			if strings.TrimSpace(c.SmtpHost) == "" {
				log.Printf("notify: smtp 渠道缺 host，已忽略")
				continue
			}
			// 收件人逐项清理：空项剔除，剔完为空则丢弃渠道
			to := c.SmtpTo[:0]
			for _, t := range c.SmtpTo {
				if t = strings.TrimSpace(t); t != "" {
					to = append(to, t)
				}
			}
			if len(to) == 0 {
				log.Printf("notify: smtp 渠道缺收件人，已忽略")
				continue
			}
			c.SmtpTo = to
			if strings.TrimSpace(c.SmtpFrom) == "" && strings.TrimSpace(c.SmtpUsername) == "" {
				log.Printf("notify: smtp 渠道 from/username 均为空，已忽略")
				continue
			}
			if c.SmtpPort < 0 || c.SmtpPort > 65535 {
				log.Printf("notify: smtp 渠道端口 %d 非法，已忽略", c.SmtpPort)
				continue
			}
			switch strings.ToLower(strings.TrimSpace(c.SmtpTLS)) {
			case "", "ssl", "starttls", "none":
			default:
				log.Printf("notify: smtp 渠道 smtp_tls %q 非法（可用 ssl/starttls/none），已忽略", c.SmtpTLS)
				continue
			}
			s.channels = append(s.channels, c)
			continue
		}
		if strings.TrimSpace(c.URL) == "" {
			continue
		}
		switch c.Type {
		case "telegram", "bark", "wecom", "dingtalk", "generic":
			s.channels = append(s.channels, c)
		default:
			log.Printf("notify: 未知渠道类型 %q 已忽略", c.Type)
		}
	}
	if cfg.Enabled && len(s.channels) == 0 {
		log.Printf("notify: enabled=true 但没有有效渠道，告警将被丢弃")
	}
	n.cfg.Store(s)
	return nil
}

// Post 非阻塞投递：判开关/订阅/去重后入队。任何情况下不阻塞调用方。
func (n *Notifier) Post(event, subject, title, body string) {
	if n == nil {
		return
	}
	s := n.cfg.Load()
	if s == nil || !s.cfg.Enabled || len(s.channels) == 0 {
		return
	}
	if !s.subscribed[event] {
		return
	}
	// 去重：同 (event, subject) cooldown 窗口内只发一条（时间戳更新失败 = 并发
	// 已发，同样放弃）。
	key := event + "\x00" + subject
	now := time.Now()
	n.dedupMu.Lock()
	if last, ok := n.dedup[key]; ok && now.Sub(last) < s.cooldown {
		n.dedupMu.Unlock()
		return
	}
	n.dedup[key] = now
	// 顺手清掉已过窗口的旧键：map 只增不减的话，free_quota_exhausted 这类
	// (uid|model) 复合 subject 长期运行会缓慢累积。告警频率低，O(n) 可接受。
	for k, t := range n.dedup {
		if now.Sub(t) > s.cooldown {
			delete(n.dedup, k)
		}
	}
	n.dedupMu.Unlock()

	select {
	case n.queue <- notification{event: event, subject: subject, title: title, body: body}:
	default:
		if n.dropped.Add(1) == 1 || n.dropped.Load()%100 == 0 {
			log.Printf("notify: 队列满，告警丢弃累计 %d 条", n.dropped.Load())
		}
	}
}

// Stop 停止 worker 并排空在途消息（尽力而为，队列剩余消息不再投递）。
func (n *Notifier) Stop() {
	if n == nil {
		return
	}
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
	<-n.done
}

func (n *Notifier) run() {
	defer close(n.done)
	for {
		select {
		case <-n.stop:
			return
		case msg := <-n.queue:
			n.deliver(msg)
		}
	}
}

// deliver 逐渠道投递（渠道间互相独立，单渠道失败不影响其他渠道）。
func (n *Notifier) deliver(msg notification) {
	s := n.cfg.Load()
	if s == nil {
		return
	}
	for _, c := range s.channels {
		if err := n.send(c, msg); err != nil {
			log.Printf("notify: 渠道 %s 投递失败: %v", c.Type, err)
		}
	}
}

// send 单渠道投递（失败重试一次）。
func (n *Notifier) send(c Channel, msg notification) error {
	var do func() error
	if c.Type == "smtp" {
		do = func() error { return sendMail(c, msg) }
	} else {
		payload, ct, err := buildPayload(c, msg)
		if err != nil {
			return err
		}
		do = func() error {
			req, err := http.NewRequest(http.MethodPost, c.URL, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", ct)
			resp, err := n.hc.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			return nil
		}
	}
	if err := do(); err != nil {
		time.Sleep(2 * time.Second)
		if err2 := do(); err2 != nil {
			return fmt.Errorf("重试后仍失败: %v (首次: %v)", err2, err)
		}
	}
	return nil
}

// sendMail SMTP 邮箱渠道投递（type=="smtp"）。标准库 net/smtp 实现，对任意
// 服务商通用：465 SSL 直连 / 587 等 STARTTLS 自动升级 / none 明文（不推荐）。
func sendMail(c Channel, msg notification) error {
	host := strings.TrimSpace(c.SmtpHost)
	port := c.SmtpPort
	if port == 0 {
		port = 465
	}
	mode := strings.ToLower(strings.TrimSpace(c.SmtpTLS))
	if mode == "" {
		if port == 465 {
			mode = "ssl"
		} else {
			mode = "starttls"
		}
	}
	// 未识别的 mode 宁可报错也不静默降级——降级意味着授权码明文上线路。
	if mode != "ssl" && mode != "starttls" && mode != "none" {
		return fmt.Errorf("未知 smtp_tls 模式 %q（可用 ssl/starttls/none）", c.SmtpTLS)
	}
	// 地址头注入防御：From/To 来自配置，剔除控制字符防 \r\n 注头
	sanAddr := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, s)
	}
	from := sanAddr(strings.TrimSpace(c.SmtpFrom))
	if from == "" {
		from = sanAddr(strings.TrimSpace(c.SmtpUsername))
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	d := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if mode == "ssl" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("SMTP 握手: %w", err)
	}
	defer client.Close()
	if mode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("服务器 %s 不支持 STARTTLS（可改用 465/ssl 端口）", addr)
		}
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if c.SmtpUsername != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("服务器 %s 未宣告 AUTH 扩展，无法认证（可尝试换 465/ssl 端口）", addr)
		}
		if err := client.Auth(smtp.PlainAuth("", c.SmtpUsername, c.SmtpPassword, host)); err != nil {
			return fmt.Errorf("SMTP 认证失败（确认填的是授权码而非登录密码）: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	tos := make([]string, 0, len(c.SmtpTo))
	for _, to := range c.SmtpTo {
		tos = append(tos, sanAddr(to))
		if err := client.Rcpt(tos[len(tos)-1]); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := io.WriteString(w, buildMail(from, tos, msg)); err != nil {
		return fmt.Errorf("写入邮件体: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("结束邮件体: %w", err)
	}
	// DATA 已被服务器接收，邮件实际已发出——QUIT 报错只记日志，上抛会让
	// send() 的重试把这封信再发一遍。
	if err := client.Quit(); err != nil {
		log.Printf("notify: smtp QUIT 异常（邮件已发出）: %v", err)
	}
	return nil
}

// buildMail 组装 RFC822 邮件（UTF-8 主题按 RFC2047 编码，正文纯文本）。
func buildMail(from string, to []string, msg notification) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", "[l3api] "+msg.title) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(text(msg))
	return b.String()
}

// text 组装纯文本消息体（title 单独一行 + body，超长裁剪）。
func text(msg notification) string {
	var b strings.Builder
	b.WriteString(msg.title)
	if msg.body != "" {
		b.WriteString("\n")
		b.WriteString(msg.body)
	}
	s := b.String()
	const max = 1800 // 渠道普遍 2k 上限，留 header 余量
	if len(s) > max {
		s = s[:max] + "\n…(截断)"
	}
	return s
}

// buildPayload 按渠道类型组装请求体与 Content-Type。
func buildPayload(c Channel, msg notification) ([]byte, string, error) {
	switch c.Type {
	case "telegram":
		p, _ := json.Marshal(map[string]any{
			"chat_id": c.Secret,
			"text":    text(msg),
		})
		return p, "application/json", nil
	case "bark":
		p, _ := json.Marshal(map[string]any{
			"title": msg.title,
			"body":  msg.body,
			"group": "l3api",
		})
		return p, "application/json", nil
	case "wecom":
		p, _ := json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]any{"content": text(msg)},
		})
		return p, "application/json", nil
	case "dingtalk":
		if c.Secret != "" {
			// 加签：timestamp + "\n" + secret 的 HMAC-SHA256 → base64，拼进 url。
			ts := time.Now().UnixMilli()
			mac := hmac.New(sha256.New, []byte(c.Secret))
			fmt.Fprintf(mac, "%d\n%s", ts, c.Secret)
			sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			sep := "?"
			if strings.Contains(c.URL, "?") {
				sep = "&"
			}
			c.URL = fmt.Sprintf("%s%stimestamp=%d&sign=%s", c.URL, sep, ts, sign)
		}
		p, _ := json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]any{"content": text(msg)},
		})
		return p, "application/json", nil
	case "generic":
		p, _ := json.Marshal(map[string]any{
			"event":   msg.event,
			"subject": msg.subject,
			"title":   msg.title,
			"body":    msg.body,
		})
		return p, "application/json", nil
	default:
		return nil, "", fmt.Errorf("未知渠道类型 %q", c.Type)
	}
}

// Validate 校验 Config 的可解析性（config normalize 与面板保存共用同一口径）。
// 只做静态检查（事件名/时长格式），渠道有效性在 Store 时清理。
func Validate(cfg Config) error {
	if cfg.Cooldown != "" {
		if d, err := time.ParseDuration(cfg.Cooldown); err != nil {
			return fmt.Errorf("notify.cooldown: %w", err)
		} else if d < 0 {
			return fmt.Errorf("notify.cooldown: 不能为负数")
		}
	}
	for _, e := range cfg.Events {
		if !isKnownEvent(e) {
			return fmt.Errorf("notify.events: %q 不是已知事件（可用: %s）", e, strings.Join(allEventsSorted(), ", "))
		}
	}
	if cfg.Enabled && len(cfg.Channels) == 0 {
		return fmt.Errorf("notify.enabled=true 但 channels 为空——请至少配置一个推送渠道，或设 enabled=false")
	}
	return nil
}

func isKnownEvent(e string) bool {
	for _, k := range allEvents {
		if k == e {
			return true
		}
	}
	return false
}

func allEventsSorted() []string {
	out := append([]string(nil), allEvents...)
	sort.Strings(out)
	return out
}
