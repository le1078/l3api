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
type TraeModel struct {
	ID          string `json:"id"`           // 上游 config_name（大小写敏感，原样透出）
	Name        string `json:"name"`         // 展示名
	IsReasoning bool   `json:"is_reasoning"`
	IsVL        bool   `json:"is_vl"`
	MaxTokens   int    `json:"max_tokens"`   // context_window_tokens.dev（客户端实际生效窗）
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
func traeSeedModels() []TraeModel {
	return []TraeModel{
		{ID: "glm-5.3", Name: "GLM-5.3", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", IsReasoning: true, IsVL: false, MaxTokens: 116000},
		{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX", IsReasoning: false, IsVL: false, MaxTokens: 116000},
		{ID: "glm-5.2", Name: "GLM-5.2", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "glm-5.1", Name: "GLM-5.1", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "glm-4.7", Name: "GLM-4.7", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "kimi-k3", Name: "Kimi-K3", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "kimi-k2.7-code", Name: "Kimi-K2.7-Code", IsReasoning: true, IsVL: false, MaxTokens: 200000},
		{ID: "kimi-k2", Name: "Kimi-K2", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek-V4-Pro", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "DeepSeek-V4-Flash", Name: "DeepSeek-V4-Flash", IsReasoning: true, IsVL: false, MaxTokens: 116000},
		{ID: "DeepSeek-V4-Pro-Official", Name: "DeepSeek-V4-Pro-Official", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek-V4-Flash-Official", IsReasoning: true, IsVL: false, MaxTokens: 116000},
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", IsReasoning: false, IsVL: false, MaxTokens: 116000},
		{ID: "Doubao-Seed-2.1-Pro", Name: "Doubao-Seed-2.1-Pro", IsReasoning: true, IsVL: true, MaxTokens: 256000},
		{ID: "Doubao-Seed-2.1-Turbo", Name: "Doubao-Seed-2.1-Turbo", IsReasoning: false, IsVL: true, MaxTokens: 256000},
		{ID: "Doubao-Seed-Code", Name: "Doubao-Seed-Code", IsReasoning: true, IsVL: false, MaxTokens: 256000},
		{ID: "Doubao-Seed-Evolving", Name: "Seed-Evolving", IsReasoning: true, IsVL: true, MaxTokens: 256000},
		{ID: "qwen3.8-max", Name: "Qwen3.8-Max", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", IsReasoning: false, IsVL: false, MaxTokens: 200000},
		{ID: "qwen-3.7-plus", Name: "Qwen3.7-Plus", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "qwen3-coder", Name: "Qwen3-Coder", IsReasoning: true, IsVL: false, MaxTokens: 116000},
		{ID: "minimax-m3", Name: "MiniMax-M3", IsReasoning: true, IsVL: true, MaxTokens: 116000},
		{ID: "minimax-m2.1", Name: "MiniMax-M2.1", IsReasoning: true, IsVL: false, MaxTokens: 116000},
		{ID: "mimo-v2.6-pro", Name: "MiMo-V2.6-Pro", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "mimo-v2.6-flash", Name: "MiMo-V2.6-Flash", IsReasoning: true, IsVL: true, MaxTokens: 200000},
		{ID: "step-5-preview", Name: "Step-5-Preview", IsReasoning: true, IsVL: true, MaxTokens: 116000},
	}
}
