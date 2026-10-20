// trae_models.go Trae 静态模型目录（chat 品牌路由用）。
//
// 来源：2026-09-28 云机实测 POST {TraeChatHost}/api/ide/v1/get_detail_param
// （body {"function":"chat_v3"}，466KB 响应）下发的 config_info_list——
// 2026-10-09 复测：新增 Seed-Evolving / MiMo-V2.6-Pro / MiMo-V2.6-Flash /
// Qwen3.7-Plus / DeepSeek-V4-Pro-Official / DeepSeek-V4-Flash-Official 6 款
// （均实测 chat 200）。
// ID 即上游消费的 config_name（llm_utils_chat 请求体的 config_name/model 字段）。
// 旧客户端的 seed_m8/deepseek-V3 等名字已失效（实测 4001 param invalid），勿回填。
//
// 纯静态的原因：get_detail_param 响应体积大且含大量自定义模型槽位
// （custom_model_* 需用户自备 API key，不属于免费池），动态全量透出会污染
// /v1/models。这里只列预设模型（config_source=1 且 config_switch=true）。
// 上游下架某模型时其请求会收到流内 4023/4001 → ErrClient 换号/换模型即可，
// 目录按下回实测更新。
package upstream

// TraeModel Trae 模型条目。
// JSON tag 供 model_sync.json 覆盖文件落盘/读取（modelsync.go）。
//
// 2026-10-21 补齐面板「模型与档位」五列（与 WorkBuddy 动态目录同口径）：
// consumption_rate→credits、reasoning_effort_config→efforts/default_effort、
// model_detail_list[0].max_tokens→MaxOutputTokens。
type TraeModel struct {
	ID                 string   `json:"id"`   // 上游 config_name（大小写敏感，原样透出）
	Name               string   `json:"name"` // 展示名
	IsReasoning        bool     `json:"is_reasoning"`
	IsVL               bool     `json:"is_vl"`
	MaxTokens          int      `json:"max_tokens"`                     // context_window_tokens.dev（客户端实际生效窗）
	Credits            string   `json:"credits,omitempty"`              // 消耗倍率（display_contact_config.consumption_rate，归一 "x0.78"）
	MaxOutputTokens    int64    `json:"max_output_tokens,omitempty"`    // model_detail_list[0].max_tokens（0=未知）
	Efforts            []string `json:"efforts,omitempty"`              // reasoning_effort_config.options（空=固定档/不可调）
	DefaultEffort      string   `json:"default_effort,omitempty"`       // reasoning_effort_config.default_level
	CanDisableThinking bool     `json:"can_disable_thinking,omitempty"` // 目前恒 false（Trae 未下发 disabled 档）

	// 免费/促销（2026-10-21）：与 WorkBuddy 的 badge+promotion 同口径透出。
	// MaxTokens 保持 context_window_tokens.dev（客户端默认窗）；ContextTiers
	// 列出官方全部档位，面板上下文列取最大值（与 WorkBuddy 一致）。
	ContextTiers    []int64 `json:"context_tiers,omitempty"`
	OriginalCredits string  `json:"original_credits,omitempty"` // 活动前原价倍率
	Badge           string  `json:"badge,omitempty"`
	PromotionNote   string  `json:"promotion_note,omitempty"`
	PromotionWindow string  `json:"promotion_window,omitempty"`
	PromotionTZ     string  `json:"promotion_tz,omitempty"`
	PromotionFactor float64 `json:"promotion_factor,omitempty"`
	PromotionCredit string  `json:"promotion_credit,omitempty"`
	ValidUntil      string  `json:"valid_until,omitempty"` // 限时活动截止（RFC3339）
	Description     string  `json:"description,omitempty"` // 官方模型说明/活动文案（display_config 或客户端介绍）
}

// TraeChatModels 返回 Trae 预设模型目录（2026-10-09 get_detail_param 复测）。
// glm-5.3 / kimi-k3 / DeepSeek-V4-Pro 及新增 6 款（Seed-Evolving、MiMo-V2.6-Pro/Flash、
// Qwen3.7-Plus、DeepSeek-V4-Pro/Flash-Official）已逐一实测 200 应答。
//
// 读取口径（v1.12 model-sync）：model_sync.json 覆盖文件有非空 trae 段时
// 整体替换本种子（官方下架随之生效）；无/损坏回落种子（modelsync.go）。
func TraeChatModels() []TraeModel {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	loadModelSyncLocked()
	if len(modelSyncState.file.Trae) > 0 {
		return append([]TraeModel(nil), modelSyncState.file.Trae...)
	}
	return traeSeedModels()
}

// traeSeedModels 种子目录（Go 源码内嵌，人工实测口径；兜底用）。
// 2026-10-21 get_detail_param 复测补齐 credits/efforts/max_output（详见
// TraeModel 字段注释与 modelsync.go 的 parseTraeDetailParam）。
func traeSeedModels() []TraeModel {
	return []TraeModel{
		{ID: "glm-5.3", Name: "GLM-5.3", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.78", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", IsReasoning: true, IsVL: false, MaxTokens: 116000,
			Credits: "x0.08", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX", IsReasoning: false, IsVL: false, MaxTokens: 116000,
			Credits: "x0.31", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "glm-5.2", Name: "GLM-5.2", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.78", MaxOutputTokens: 16000, Efforts: []string{"high", "extra_high"}, DefaultEffort: "high"},
		{ID: "glm-5.1", Name: "GLM-5.1", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.83", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "glm-4.7", Name: "GLM-4.7", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.38", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "kimi-k3", Name: "Kimi-K3", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x1.83", MaxOutputTokens: 32000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
		{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x0.98", MaxOutputTokens: 32000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
		{ID: "kimi-k2.7-code", Name: "Kimi-K2.7-Code", IsReasoning: true, IsVL: false, MaxTokens: 200000,
			Credits: "x0.83", MaxOutputTokens: 32000, DefaultEffort: "on"},
		{ID: "kimi-k2", Name: "Kimi-K2", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.48", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek-V4-Pro", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.36", MaxOutputTokens: 16000, Efforts: []string{"high", "extra_high"}, DefaultEffort: "high"},
		{ID: "DeepSeek-V4-Flash", Name: "DeepSeek-V4-Flash", IsReasoning: true, IsVL: false, MaxTokens: 116000,
			Credits: "x0.10", MaxOutputTokens: 16000, Efforts: []string{"high", "extra_high"}, DefaultEffort: "high"},
		{ID: "DeepSeek-V4-Pro-Official", Name: "DeepSeek-V4-Pro-Official", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.36", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek-V4-Flash-Official", IsReasoning: true, IsVL: false, MaxTokens: 116000,
			Credits: "x0.10", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", IsReasoning: false, IsVL: false, MaxTokens: 116000,
			Credits: "x0.06", MaxOutputTokens: 16000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "Doubao-Seed-2.1-Pro", Name: "Doubao-Seed-2.1-Pro", IsReasoning: true, IsVL: true, MaxTokens: 256000,
			Credits: "x0.80", MaxOutputTokens: 32000, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
		{ID: "Doubao-Seed-2.1-Turbo", Name: "Doubao-Seed-2.1-Turbo", IsReasoning: false, IsVL: true, MaxTokens: 256000,
			Credits: "x0.20", MaxOutputTokens: 32000, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
		{ID: "Doubao-Seed-Code", Name: "Doubao-Seed-Code", IsReasoning: true, IsVL: false, MaxTokens: 256000,
			Credits: "x0.06", MaxOutputTokens: 32000, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
		{ID: "Doubao-Seed-Evolving", Name: "Seed-Evolving", IsReasoning: true, IsVL: true, MaxTokens: 256000,
			Credits: "x0.80", MaxOutputTokens: 32000, DefaultEffort: "on"},
		{ID: "qwen3.8-max", Name: "Qwen3.8-Max", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x1.50", MaxOutputTokens: 32000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", IsReasoning: false, IsVL: false, MaxTokens: 200000,
			Credits: "x0.08", MaxOutputTokens: 32000, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
		{ID: "qwen-3.7-plus", Name: "Qwen3.7-Plus", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x0.25", MaxOutputTokens: 32000, DefaultEffort: "on"},
		{ID: "qwen3-coder", Name: "Qwen3-Coder", IsReasoning: true, IsVL: false, MaxTokens: 116000,
			Credits: "x0.26", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "minimax-m3", Name: "MiniMax-M3", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.26", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "minimax-m2.1", Name: "MiniMax-M2.1", IsReasoning: true, IsVL: false, MaxTokens: 116000,
			Credits: "x0.19", MaxOutputTokens: 16000, DefaultEffort: "on"},
		{ID: "mimo-v2.6-pro", Name: "MiMo-V2.6-Pro", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x0.15", MaxOutputTokens: 32000, DefaultEffort: "on"},
		{ID: "mimo-v2.6-flash", Name: "MiMo-V2.6-Flash", IsReasoning: true, IsVL: true, MaxTokens: 200000,
			Credits: "x0.06", MaxOutputTokens: 32000, DefaultEffort: "on"},
		{ID: "step-5-preview", Name: "Step-5-Preview", IsReasoning: true, IsVL: true, MaxTokens: 116000,
			Credits: "x0.48", MaxOutputTokens: 16000, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
	}
}
