// qoder_models.go Qoder 模型目录（GET /algo/api/v2/model/list?Encode=1 探测结果，
// 2026-09-22 v1.1.57 实测 chat 组 14 个；2026-10-09 复测：keys 不变，
// auto/dmodel/dfmodel/gfmodel/kmodel_latest/mmodel 的窗口或能力标记有更新）。
//
// 为什么静态硬编码而非动态拉取：model/list 端点与 chat 同源（Bearer COSY），
// 但面板 /v1/models 是无状态的公开目录（不携带某个具体账号的会话），不适合在
// 列表接口里隐式消耗某个账号的配额探测。Qoder 模型集变化低频（季度级），
// 静态表 + 面板「模型与档位」页可手动校正，比动态拉取更稳。
package upstream

// QoderModel 单个 Qoder chat 模型（/model/list chat 组条目的最小子集）。
// JSON tag 供 model_sync.json 覆盖文件落盘/读取（modelsync.go）。
type QoderModel struct {
	ID             string `json:"id"`              // 模型 key（qfmodel/kmodel_latest/...），chat 请求 model_config.key
	DisplayName    string `json:"display_name"`    // 面板展示名
	IsVL           bool   `json:"is_vl"`           // 视觉能力（抓包 is_vl）
	IsReasoning    bool   `json:"is_reasoning"`    // 推理能力（抓包 is_reasoning）
	MaxInputTokens int64  `json:"max_input_tokens"` // 上下文窗口（抓包 max_input_tokens）
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
func qoderSeedModels() []QoderModel {
	return []QoderModel{
		{ID: "auto", DisplayName: "Auto", IsVL: true, IsReasoning: true, MaxInputTokens: 200000},
		{ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "qfmodel", DisplayName: "Qwen3.8-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "qmodel_latest", DisplayName: "Qwen3.7-Max", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "qmodel", DisplayName: "Qwen3.7-Plus", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "q37fmodel", DisplayName: "Qwen3.7-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "dmodel", DisplayName: "DeepSeek-V4-Pro", IsVL: true, IsReasoning: true, MaxInputTokens: 96000},
		{ID: "dfmodel", DisplayName: "DeepSeek-Flash", IsVL: true, IsReasoning: false, MaxInputTokens: 180000},
		{ID: "gmodel", DisplayName: "GLM-5.3", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "gfmodel", DisplayName: "GLM-5.3-Flash", IsVL: true, IsReasoning: true, MaxInputTokens: 1000000},
		{ID: "gm51model", DisplayName: "GLM-5.2", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "kmodel_latest", DisplayName: "Kimi-K3", IsVL: true, IsReasoning: false, MaxInputTokens: 180000},
		{ID: "kmodel", DisplayName: "Kimi-K2.8-Preview", IsVL: true, IsReasoning: true, MaxInputTokens: 180000},
		{ID: "mmodel", DisplayName: "MiniMax-M2.7", IsVL: false, IsReasoning: false, MaxInputTokens: 180000},
	}
}
