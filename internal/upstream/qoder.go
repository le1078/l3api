// qoder.go Qoder（通义灵码 Cosy 平台）provider 实现。
//
// 认证链路（docs/qoder-protocol.md）：
//  1. PAT → openapi /api/v1/jobToken/exchange（明文）→ jobToken（24h）+ refreshToken（48h）；
//  2. 过期前用 refreshToken → /api/v1/jobToken/refresh 续期（滚动 refresh）；
//  3. chat 走 algo 域 Bearer COSY（RSA 临时密钥 + AES identity + MD5 签名，见 qoder_bearer.go）。
//
// Auth 字段映射：AccessToken=PAT，RefreshToken=refreshToken（滚动），UID=用户 UUID。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

const (
	qoderOpenapiBase = "https://openapi.qoder.com.cn"
	qoderChatBase    = "https://gateway.qoder.com.cn"
	qoderChatPath    = "/algo/api/v2/service/pro/sse/agent_chat_generation"
)

// qoderJobTokenResp /api/v1/jobToken/exchange 与 /refresh 的响应形态。
type qoderJobTokenResp struct {
	Token                 string `json:"token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresAt             string `json:"expires_at"`               // RFC3339
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"` // RFC3339
}

// QoderChatRequest agent_chat_generation 请求体（Encode=1 编码后发送）。
type QoderChatRequest struct {
	RequestID string `json:"request_id"`
	Stream    bool   `json:"stream"`
	AgentID   string `json:"agent_id"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// QoderSession 会话状态（jobToken 滚动更新）。
type QoderSession struct {
	JobToken      string
	RefreshToken  string
	ExpiresAt     time.Time
	RefreshExpire time.Time
	UID           string
	Nickname      string
}

// QoderUserinfo /api/v1/userinfo 响应（取 id 与 name；id 即 chat 身份的 uid）。
type QoderUserinfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// qoderUserinfo 拉用户档案（明文，openapi 域；Bearer jobToken）。
// PAT 导入路径用它取 uid/nickname 落盘。
func (c *Client) qoderUserinfo(ctx context.Context, jobToken string) (*QoderUserinfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderOpenapiBase+"/api/v1/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jobToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("userinfo %d: %s", resp.StatusCode, string(b))
	}
	var out QoderUserinfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("userinfo parse: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("userinfo: empty id")
	}
	return &out, nil
}

// qoderRefreshJobToken 用 refreshToken 换新 jobToken（明文，openapi 域）。
func (c *Client) qoderRefreshJobToken(ctx context.Context, refreshToken string) (*QoderSession, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, "POST",
		qoderOpenapiBase+"/api/v1/jobToken/refresh", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jobToken refresh %d: %s", resp.StatusCode, string(b))
	}
	var out qoderJobTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("jobToken refresh parse: %w", err)
	}
	return qoderSessionFromResp(&out)
}

// QoderExchangePAT PAT → jobToken 会话（面板「添加账号」用；明文，openapi 域）。
//
// 返回的会话含 uid/nickname（随 userinfo 一并拉取，供面板落盘命名与展示）。
// PAT 本身只用于换 jobToken，不随会话透出（jobToken 才是短期可用的 chat 凭证）。
func (c *Client) QoderExchangePAT(ctx context.Context, pat string) (*QoderSession, error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, err := http.NewRequestWithContext(ctx, "POST",
		qoderOpenapiBase+"/api/v1/jobToken/exchange", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("PAT exchange %d: %s", resp.StatusCode, string(b))
	}
	var out qoderJobTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("PAT exchange parse: %w", err)
	}
	sess, err := qoderSessionFromResp(&out)
	if err != nil {
		return nil, err
	}
	// 取 uid/nickname（chat 身份必需；失败不阻断导入，留空由调用方兜底）。
	if ui, uerr := c.qoderUserinfo(ctx, sess.JobToken); uerr == nil && ui != nil {
		sess.UID = ui.ID
		sess.Nickname = ui.Name
	}
	return sess, nil
}

func qoderSessionFromResp(r *qoderJobTokenResp) (*QoderSession, error) {
	exp, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("jobToken expires_at parse %q: %w", r.ExpiresAt, err)
	}
	rexp, err := time.Parse(time.RFC3339, r.RefreshTokenExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("jobToken refresh_expires_at parse %q: %w", r.RefreshTokenExpiresAt, err)
	}
	return &QoderSession{
		JobToken:      r.Token,
		RefreshToken:  r.RefreshToken,
		ExpiresAt:     exp,
		RefreshExpire: rexp,
	}, nil
}

// QoderRefreshIfNeeded 检查 auth 中 Qoder 凭证是否需要刷新（jobToken 过期或 30 分钟内到期），
// 需要时用 refreshToken 续期并原子写回 auth 文件。
func (c *Client) QoderRefreshIfNeeded(a *auth.Auth) error {
	if a.Brand() != "qoder" {
		return nil
	}
	if !a.NeedsRefresh(30 * time.Minute) {
		return nil
	}
	if a.RefreshToken == "" {
		return fmt.Errorf("qoder refresh_token missing (uid=%s)", a.UID)
	}
	return c.qoderRefreshSingleflight(a)
}

// qoderRefreshSingleflight qoder jobToken 滚动的唯一实现（2026-10-20 单飞化）。
//
// 四条调用路径（chat / ProbeQoderModels / checkin / RefreshIfNeeded）共用：
// 同账号并发调用只有 leader 真打 refresh 端点，follower 等 leader 写回后
// 直接看到新 token。此前四条路径各自「NeedsRefresh → 刷新」，同账号 8 并发
// 进过期窗口时 8 次滚动刷新互相覆盖（后写覆盖先写），且同 uid 短时多次
// 刷 token 在风控视角是异常行为。
//
// ctx 仅用于 leader 的 HTTP 调用；follower 等待不感知 ctx 取消（刷新通常
// <2s，等待有界由上游 HTTP.Timeout 保证）。
func (c *Client) qoderRefreshSingleflight(a *auth.Auth) error {
	RefreshAuthOnce(a.UID, func() {
		// fn 内不再判 NeedsRefresh：调用方已判过，且本函数只被「需要刷新」
		// 的路径调用（QoderRefreshIfNeeded 也在调用前判过）。follower 醒来
		// 时 auth 已是新 token，无需再刷。
		if a.RefreshToken == "" {
			return // 无 refresh_token：保持旧 token，由调用方按需报错
		}
		sess, err := c.qoderRefreshJobToken(context.Background(), a.RefreshToken)
		if err != nil {
			return // 刷新失败：保持旧 token（调用方随后打上游会拿到 401，
			// 走 ErrSessionDead 阈值化——与旧行为一致）
		}
		a.Lock()
		a.AccessToken = sess.JobToken // AccessToken 滚动为当前 jobToken
		a.RefreshToken = sess.RefreshToken
		a.ExpiresAt = sess.ExpiresAt.Unix()
		a.Unlock()
		_ = a.SaveAtomic() // 落盘失败不阻断本次（下次冷交换）
	})
	// 单飞完成后：刷新成功则 NeedsRefresh 已失效；失败/无 refresh_token 时
	// 仍为真——调用方据此区分「已续期」与「仍需按 session dead 处理」。
	return nil
}
