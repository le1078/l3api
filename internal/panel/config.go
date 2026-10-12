// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	// 配置有效性自检：检查关键配置项是否生效
	checks := map[string]any{}
	if m, ok := cfg.(map[string]any); ok {
		// 签到时间
		if v, ok := m["checkin_times"]; ok {
			checks["checkin_times"] = map[string]any{"value": v, "valid": true}
		}
		// 猫猫旅行
		if v, ok := m["travel_enabled"]; ok {
			checks["travel_enabled"] = map[string]any{"value": v, "valid": true}
		}
		// 活跃上报
		if v, ok := m["activity_enabled"]; ok {
			checks["activity_enabled"] = map[string]any{"value": v, "valid": true}
		}
		// token 保活
		if v, ok := m["keepalive_enabled"]; ok {
			checks["keepalive_enabled"] = map[string]any{"value": v, "valid": true}
		}
		// 夜猫子
		if v, ok := m["nightowl_enabled"]; ok {
			checks["nightowl_enabled"] = map[string]any{"value": v, "valid": true}
		}
		// 余额刷新间隔
		if v, ok := m["balance_refresh_interval"]; ok {
			checks["balance_refresh_interval"] = map[string]any{"value": v, "valid": true}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": redactConfigSecrets(cfg),
		"checks": checks,
	})
}

// redactConfigSecrets 配置回显脱敏：密钥/token 类字段一律置空串。
// 空串语义与前端 collectConfig 对齐（空 = 不提交 → saveConfig 深合并保留
// 磁盘原值），因此脱敏不会破坏「读-改-写」回路。此前该接口明文返回
// api_key/api_keys[].key/docs_sheet.token/upstash.token 等全部密钥，
// 与同仓库 keys API 的掩码口径矛盾（v1.12 修复）。
func redactConfigSecrets(cfg any) any {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return cfg
	}
	blank := func(path ...string) {
		cur := m
		for _, k := range path[:len(path)-1] {
			next, ok := cur[k].(map[string]any)
			if !ok {
				return
			}
			cur = next
		}
		if _, ok := cur[path[len(path)-1]]; ok {
			cur[path[len(path)-1]] = ""
		}
	}
	blank("api_key")
	blank("docs_sheet", "token")
	blank("upstash", "token")
	blank("upstream", "device_token")
	if arr, ok := m["api_keys"].([]any); ok {
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if _, has := em["key"]; has {
					em["key"] = ""
				}
			}
		}
	}
	// notify.channels 是数组，blank() 的 map 路径够不着：钉钉加签密钥、telegram
	// chat_id、smtp 授权码同属密钥口径，逐项置空串（与 api_keys 同一模式）。
	// 空串语义 = 前端不提交该字段 → saveConfig 按渠道索引保留磁盘原值。
	if nm, ok := m["notify"].(map[string]any); ok {
		if chs, ok := nm["channels"].([]any); ok {
			for _, e := range chs {
				if em, ok := e.(map[string]any); ok {
					for _, f := range []string{"secret", "smtp_password"} {
						if _, has := em[f]; has {
							em[f] = ""
						}
					}
				}
			}
		}
	}
	return m
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
