// Package apikeys 多租户 API Key 注册表与限流。
//
// 背景：l3api 此前只有单个全局 api_key——分享即失控：用量混在一起、限不了速、
// 停不掉单个使用者。本包把「谁能调 /v1/*」从单一密钥升级为密钥集合：
//
//   - 每把 key 有 name（用量归因/展示）、enabled、模型白名单（空 = 不限）、
//     RPM 限流（0 = 不限）；
//   - 注册表是不可变快照 + atomic 指针（读路径无锁，与 livecfg 同风格）；
//     面板热改后 Store 整体替换，在途请求继续用旧快照跑完，无撕裂视图；
//   - 限流窗口状态按 name 记在 Registry 内、跨热更新保留（换配置不重置计数）。
//
// 与既有单 key 的兼容：main 装配时把 config.api_key 合成 name="default" 的
// 条目并入注册表（api_keys 数组为空时注册表只有这一条），老客户端零回归。
package apikeys

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entry 单把 API Key 的声明（config.api_keys 数组元素）。
type Entry struct {
	// Name 租户标识：用量归因（usage 的 key 维度）、面板展示、限流窗口键。
	// 空名条目在 normalize 阶段被拒绝。
	Name string `json:"name"`
	// Key 密钥原文（sk- 前缀建议但强制）。仅存于 config.json 与内存；
	// 面板列表接口只回掩码。
	Key string `json:"key"`
	// Disabled 禁用开关（缺省 false = 启用）。禁用 key 的请求一律 401。
	Disabled bool `json:"disabled,omitempty"`
	// Models 模型白名单：允许调用的模型名（与请求 model 原文或剥前缀后的裸名
	// 匹配，大小写不敏感；"*" 或空 = 不限）。白名单外的模型一律 403。
	Models []string `json:"models,omitempty"`
	// RPM 每分钟请求数上限（固定窗口，0 = 不限）。超限 429 带 Retry-After。
	RPM int `json:"rpm,omitempty"`
	// Note 备注（面板展示用，纯注释性）。
	Note string `json:"note,omitempty"`
	// FreeGuard 免费额度守护（缺省 false = 关闭）。开启后：模型处于生效的
	// 免费促销窗口（EffectivePromotion 折后 0x）时，若某账号实测开始扣积分
	// （usage.credit>0，免费额度已尽），该账号对此模型被排除出选号；
	// 全尽则请求直接报错 402 free_quota_exhausted。请求模型名加 "@paid"
	// 后缀可单次强制放行付费。窗口外（无生效免费促销）守护不生效，照常计费。
	FreeGuard bool `json:"free_guard,omitempty"`
}

// AllowsModel 报告该条目的白名单是否放行 model。匹配是「双侧剥 realm 前缀」
// 的：请求原文与白名单条目各自生成候选（完整串 + 剥掉 cn:/global: 域前缀后的
// 裸名），任一组合相等即放行——白名单写 "cn:glm-5.2" 或裸名 "glm-5.2" 都能
// 命中 "glm-5.2"/"cn:glm-5.2" 请求。品牌前缀（qoder:/trae:）不剥，
// 跨品牌不可互通。大小写不敏感。白名单空（或仅 "*"）= 不限。
func (e Entry) AllowsModel(models ...string) bool {
	if len(e.Models) == 0 {
		return true
	}
	for _, m := range e.Models {
		if strings.TrimSpace(m) == "*" {
			return true
		}
	}
	for _, req := range models {
		for _, w := range e.Models {
			for _, a := range modelCandidates(req) {
				for _, b := range modelCandidates(w) {
					if strings.EqualFold(a, b) {
						return true
					}
				}
			}
		}
	}
	return false
}

// modelCandidates 模型名的匹配候选集：原文 + 剥 **realm 域前缀**（cn:/global:）
// 后的裸名。品牌前缀（qoder:/trae:）不剥——品牌是路由选择器，剥掉会让
// 白名单 "cn:glm-5.2" 静默放行 "qoder:glm-5.2"（跨域越权，v1.12 修复）。
func modelCandidates(s string) []string {
	s = strings.TrimSpace(s)
	for _, pfx := range []string{"cn:", "global:"} {
		if rest, ok := strings.CutPrefix(strings.ToLower(s), pfx); ok && rest != "" {
			return []string{s, s[len(pfx):]}
		}
	}
	return []string{s}
}

// Mask 掩码展示：保留前 6 后 4（sk-xxx…xxxx），过短的 key 全掩码。
func (e Entry) Mask() string {
	k := e.Key
	if len(k) <= 10 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// set 一次不可变快照。条目数即配置内 key 数（小集合），Verify 全量遍历
// 逐条常量时间比较即可——耗时形状与命中位置无关，无需哈希索引。
type set struct {
	entries []Entry
}

// Registry 密钥注册表：快照 + 限流窗口状态。
type Registry struct {
	p atomic.Pointer[set]

	mu      sync.Mutex
	windows map[string]*rateWindow // name → 当前固定窗口计数
}

// rateWindow 固定窗口限流状态（每 name 一个，跨热更新保留）。
type rateWindow struct {
	bucket int64 // unix 分钟数
	count  int
}

// NewRegistry 构建注册表；entries 中的空 key 条目被丢弃（防御脏数据）。
// 返回的注册表即使条目为空也可用（Verify 恒 false → 全部 401）。
func NewRegistry(entries []Entry) *Registry {
	r := &Registry{windows: map[string]*rateWindow{}}
	r.Store(entries)
	return r
}

// Store 整体替换快照（面板热改路径）。限流窗口状态保留。
func (r *Registry) Store(entries []Entry) {
	s := &set{entries: make([]Entry, 0, len(entries))}
	for _, e := range entries {
		if strings.TrimSpace(e.Key) == "" {
			continue
		}
		s.entries = append(s.entries, e)
	}
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].Name < s.entries[j].Name })
	r.p.Store(s)
}

// Verify 常量时间逐条比对 token，命中且未禁用 → 返回条目。
// 未配置任何 key（空注册表）时返回 false（调用方应视作鉴权关闭前的兜底：
// main 装配保证注册表至少含 default 条目，空表只在测试/裸用出现）。
func (r *Registry) Verify(token string) (Entry, bool) {
	if r == nil || token == "" {
		return Entry{}, false
	}
	s := r.p.Load()
	if s == nil || len(s.entries) == 0 {
		return Entry{}, false
	}
	d := []byte(digestHex(token))
	idx, hit := -1, false
	// 全量比对保持耗时形状与条目数无关（每条一次常量时间比较）。
	for i, e := range s.entries {
		if subtle.ConstantTimeCompare(d, []byte(digestHex(e.Key))) == 1 {
			idx, hit = i, true
		}
	}
	if !hit {
		return Entry{}, false
	}
	e := s.entries[idx]
	if e.Disabled {
		return Entry{}, false
	}
	return e, true
}

// Lookup 按名取条目（面板/观测用）。
func (r *Registry) Lookup(name string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	s := r.p.Load()
	if s == nil {
		return Entry{}, false
	}
	for _, e := range s.entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Len 返回当前快照内的条目数。0 = 注册表为空 = 鉴权关闭（与启动时
// "api_key/api_keys 全空 = 不鉴权" 同语义；server.authenticate 据此回落放行）。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	s := r.p.Load()
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Entries 返回快照内全部条目（面板列表用；key 原文由调用方自行掩码）。
func (r *Registry) Entries() []Entry {
	if r == nil {
		return nil
	}
	s := r.p.Load()
	if s == nil {
		return nil
	}
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Allow 固定窗口限流判定：该 name 在当前 1 分钟窗口内请求数 < limit → 记数放行。
// limit<=0 不限（不记数）。返回本次是否放行 + 建议重试等待（仅超限时非零）。
func (r *Registry) Allow(name string, limit int) (ok bool, retryAfter time.Duration) {
	if r == nil || limit <= 0 {
		return true, 0
	}
	bucket := time.Now().Unix() / 60
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.windows[name]
	if w == nil || w.bucket != bucket {
		w = &rateWindow{bucket: bucket}
		r.windows[name] = w
		// 窗口状态机只增不减：name 集合有限（配置内 key 数），无需后台清理；
		// 同 name 重复建窗直接覆盖旧桶。
	}
	if w.count >= limit {
		// 距下一窗口整分钟的秒数。
		return false, time.Duration((w.bucket+1)*60-time.Now().Unix()) * time.Second
	}
	w.count++
	return true, 0
}

// digestHex key 的 sha256 hex（与 httpauth.digest 同源算法，吸收长度差异）。
func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}
