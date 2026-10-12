// keys.go 面板「API Keys」管理页接口（v1.11 多租户）。
//
// 分工与 config.go 相同：此处只做 HTTP 编排（列表/掩码/请求解析），
// 校验与落盘在 main 注入的 SaveKeys 闭包内完成——同一套 config normalize
// 校验口径（key 空检查、重复检查、default 合成），避免两处漂移。
//
// 热生效路径：SaveKeys 落盘后调 Registry.Store——注册表指针在 livecfg 快照里
// 稳定不变，handler 每请求 Load 最新快照，改完即生效，无需动 livecfg。
//
// 安全口径：列表只回掩码（前 6 后 4）；key 原文只在创建响应里出现一次
// （面板展示后由用户自行保存，服务端不提供再次查看）。
package panel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/l3api/internal/apikeys"
)

// keysList GET /panel/api/keys：当前注册表条目（key 掩码）。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return
	}
	entries := p.cfg.Keys.Entries()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"name":       e.Name,
			"key":        e.Mask(),
			"disabled":   e.Disabled,
			"models":     e.Models,
			"rpm":        e.RPM,
			"note":       e.Note,
			"free_guard": e.FreeGuard,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": out})
}

// keysUpsertReq POST /panel/api/keys/upsert 请求体。
type keysUpsertReq struct {
	Name     string   `json:"name"`
	Key      string   `json:"key"` // 新建时留空 = 服务端生成；编辑时留空 = 保留原值
	Disabled bool     `json:"disabled"`
	Models   []string `json:"models"`
	RPM      int      `json:"rpm"`
	Note     string   `json:"note"`
	// FreeGuard 免费额度守护（apikeys.Entry.FreeGuard 透传）。
	FreeGuard bool `json:"free_guard"`
}

// keysUpsert POST /panel/api/keys/upsert：按 name 新建或更新条目后落盘热生效。
// 新建且未提供 key 时服务端生成强随机 sk-（响应里回原文，仅此一次）。
func (p *Panel) keysUpsert(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil || p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return
	}
	var req keysUpsertReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	entries := p.cfg.Keys.Entries()
	generated := ""
	idx := -1
	for i := range entries {
		if entries[i].Name == req.Name {
			idx = i
			break
		}
	}
	if idx >= 0 {
		// 编辑：key 留空保留原值（掩码态不可回原文）。
		if strings.TrimSpace(req.Key) == "" {
			req.Key = entries[idx].Key
		}
	} else {
		// 新建：key 留空 → 服务端生成。
		if strings.TrimSpace(req.Key) == "" {
			raw := make([]byte, 24)
			if _, err := rand.Read(raw); err != nil {
				writeErr(w, http.StatusInternalServerError, "gen key: "+err.Error())
				return
			}
			generated = "sk-" + base64.RawURLEncoding.EncodeToString(raw)
			req.Key = generated
		}
	}

	entry := apikeys.Entry{
		Name:      req.Name,
		Key:       strings.TrimSpace(req.Key),
		Disabled:  req.Disabled,
		Models:    req.Models,
		RPM:       req.RPM,
		Note:      req.Note,
		FreeGuard: req.FreeGuard,
	}
	if idx >= 0 {
		entries[idx] = entry
	} else {
		entries = append(entries, entry)
	}
	if err := p.cfg.SaveKeys(entries); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	logKeysChanged("upsert", req.Name)
	resp := map[string]any{"ok": true}
	if generated != "" {
		resp["generated_key"] = generated // 仅创建响应出现一次
	}
	writeJSON(w, http.StatusOK, resp)
}

// keysRemoveReq POST /panel/api/keys/remove 请求体。
type keysRemoveReq struct {
	Name string `json:"name"`
}

// keysRemove POST /panel/api/keys/remove：按 name 删除条目后落盘热生效。
// 注意：删除 name=default 不影响 config.api_key 原文——重启后 default 条目会
// 从 api_key 重新合成（前端提示里已说明）。
func (p *Panel) keysRemove(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil || p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return
	}
	var req keysRemoveReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	entries := p.cfg.Keys.Entries()
	out := entries[:0]
	found := false
	for _, e := range entries {
		if e.Name == req.Name {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("key %q 不存在", req.Name))
		return
	}
	if err := p.cfg.SaveKeys(out); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	logKeysChanged("remove", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// logKeysChanged key 变更审计日志（与面板其他操作的 log 风格一致）。
func logKeysChanged(action, name string) {
	log.Printf("panel: api_keys %s %s（热生效完成）", action, name)
}
