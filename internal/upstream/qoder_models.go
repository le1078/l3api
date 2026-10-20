// qoder_models.go Qoder 模型目录（GET /algo/api/v2/model/list?Encode=1 探测结果，
// 2026-09-22 v1.1.57 实测 chat 组 14 个；2026-10-09 复测：keys 不变，
// auto/dmodel/dfmodel/gfmodel/kmodel_latest/mmodel 的窗口或能力标记有更新）。
//
// 为什么静态硬编码而非动态拉取：model/list 端点与 chat 同源（Bearer COSY），
// 但面板 /v1/models 是无状态的公开目录（不携带某个具体账号的会话），不适合在
// 列表接口里隐式消耗某个账号的配额探测。Qoder 模型集变化低频（季度级），
// 静态表 + 面板「模型与档位」页可手动校正，比动态拉取更稳。
package upstream

// QoderModel 单个 Qoder chat 模型（/model/list chat 组条目）。
// JSON tag 供 model_sync.json 覆盖文件落盘/读取（modelsync.go）。
//
// 2026-10-21 补齐面板「模型与档位」五列所需的档位/倍率/输出字段（与 WorkBuddy
// 动态目录同口径）：price_factor→credits、thinking_config→efforts/default_effort/
// can_disable_thinking、model_detail 无 max_output 时按窗口折算 MaxOutputTokens。
type QoderModel struct {
	ID                 string   `json:"id"`                             // 模型 key（qfmodel/kmodel_latest/...），chat 请求 model_config.key
	DisplayName        string   `json:"display_name"`                   // 面板展示名
	IsVL               bool     `json:"is_vl"`                          // 视觉能力（抓包 is_vl）
	IsReasoning        bool     `json:"is_reasoning"`                   // 推理能力（抓包 is_reasoning）
	MaxInputTokens     int64    `json:"max_input_tokens"`               // 上下文窗口（抓包 max_input_tokens，默认 context_config 档）
	Credits            string   `json:"credits,omitempty"`              // 积分倍率（price_factor 归一为 "x0.50"；x0.00=免费）
	MaxOutputTokens    int64    `json:"max_output_tokens,omitempty"`    // 最大输出（0=未知）
	Efforts            []string `json:"efforts,omitempty"`              // 支持的思考档位（thinking_config.enabled.efforts；空=固定档）
	DefaultEffort      string   `json:"default_effort,omitempty"`       // 默认思考档（efforts 里的 is_default 项）
	CanDisableThinking bool     `json:"can_disable_thinking,omitempty"` // thinking_config 含 disabled 档（off 可用）
}

// QoderChatModels 返回 Qoder chat 组模型目录（按上游返回顺序）。
// 数据来自 2026-09-22 真号探测（scripts/modellist_probe.py 同链路），
// 2026-10-09 复测修正字段。
//
// 读取口径（v1.12 model-sync）：model_sync.json 覆盖文件有非空 qoder 段时
// 整体替换本种子（官方下架随之生效）；无/损坏回落种子（modelsync.go）。
func QoderChatModels() []QoderModel {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	loadModelSyncLocked()
	if len(modelSyncState.file.Qoder) > 0 {
		return append([]QoderModel(nil), modelSyncState.file.Qoder...)
	}
	return qoderSeedModels()
}

// qoderSeedModels 种子目录（Go 源码内嵌，人工实测口径；兜底用）。
// 2026-10-21 真号探测补齐 credits/efforts（/model/list 的 price_factor +
// thinking_config）；MaxOutputTokens 为窗口 1/4 的估算值（官方 model/list 不下发
// 输出上限，口径见 modelsync.go 的 brandMaxOutputGuess）。
func qoderSeedModels() []QoderModel {
	return []QoderModel{
		{ID: "auto", DisplayName: "Auto", IsVL: true, IsReasoning: true, MaxInputTokens: 200000,
			Credits: "x0.50", MaxOutputTokens: 50000, DefaultEffort: "on"},
		{ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.20", MaxOutputTokens: 45000, Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", CanDisableThinking: true},
		{ID: "qfmodel", DisplayName: "Qwen3.8-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.00", MaxOutputTokens: 45000, Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", CanDisableThinking: true},
		{ID: "qmodel_latest", DisplayName: "Qwen3.7-Max", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.20", MaxOutputTokens: 45000, Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", CanDisableThinking: true},
		{ID: "qmodel", DisplayName: "Qwen3.7-Plus", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.04", MaxOutputTokens: 45000, DefaultEffort: "on", CanDisableThinking: true},
		{ID: "q37fmodel", DisplayName: "Qwen3.7-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.04", MaxOutputTokens: 45000, DefaultEffort: "on", CanDisableThinking: true},
		{ID: "dmodel", DisplayName: "DeepSeek-V4-Pro", IsVL: true, IsReasoning: true, MaxInputTokens: 96000,
			Credits: "x0.50", MaxOutputTokens: 24000, Efforts: []string{"high", "max"}, DefaultEffort: "max", CanDisableThinking: true},
		{ID: "dfmodel", DisplayName: "DeepSeek-Flash", IsVL: true, IsReasoning: false, MaxInputTokens: 180000,
			Credits: "x0.00", MaxOutputTokens: 45000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "max", CanDisableThinking: true},
		{ID: "gmodel", DisplayName: "GLM-5.3", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.80", MaxOutputTokens: 45000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		{ID: "gfmodel", DisplayName: "GLM-5.3-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 1000000,
			Credits: "x0.10", MaxOutputTokens: 250000, Efforts: []string{"high", "max"}, DefaultEffort: "max"},
		{ID: "gm51model", DisplayName: "GLM-5.2", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.80", MaxOutputTokens: 45000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		{ID: "kmodel_latest", DisplayName: "Kimi-K3", IsVL: true, IsReasoning: false, MaxInputTokens: 180000,
			Credits: "x1.40", MaxOutputTokens: 45000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		{ID: "kmodel", DisplayName: "Kimi-K2.8-Preview", IsVL: true, IsReasoning: true, MaxInputTokens: 180000,
			Credits: "x0.80", MaxOutputTokens: 45000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		{ID: "mmodel", DisplayName: "MiniMax-M2.7", IsVL: false, IsReasoning: false, MaxInputTokens: 180000,
			Credits: "x0.10", MaxOutputTokens: 45000},
	}
}
