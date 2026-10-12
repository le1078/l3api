// brand_registry.go 品牌接入的 upstream 侧注册点（v1.11 品牌插件化）。
//
// 接入新品牌时在 upstream 侧要做的两件事收拢到这里：
//  1. 注册静态模型目录（init → provider.RegisterChatModels）——/v1/models 的
//     品牌区块按注册表输出，server 不再逐品牌散写；
//  2. 注册 chat 桥接（New → registerBrandChatStream）——ChatStreamContext 按
//     注册表分发，不再 if-else 逐品牌判断（qoder/trae 两桥签名一致，
//     正好收敛为统一函数形态）。
//
// 品牌能力声明（签到/余额/任务等运营能力）在 internal/provider/brands.go——
// 调度器/面板按能力显隐，与本文件各司其职。
package upstream

import (
	"context"
	"io"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/provider"
)

func init() {
	RegisterBrandCatalogs()
}

// RegisterBrandCatalogs 把当前生效的 qoder/trae 目录（种子 + model_sync.json
// 覆盖，见 modelsync.go）重新注册进 provider 目录注册表。
//
// 三个调用点：init（启动注册种子）；SetModelSyncPath（接线后立刻重注册——
// init 时 path 未接线只能读到种子，磁盘上已有的覆盖文件要在此刻生效）；
// saveModelSync（同步落盘后重注册，运行期变化进 /v1/models）。
// RegisterChatModels 是整体覆盖语义，重复调用安全。
func RegisterBrandCatalogs() {
	// Qoder 静态目录（qoder_models.go：2026-09-22 真号探测）。
	qoderEntries := make([]provider.ModelEntry, 0, len(QoderChatModels()))
	for _, m := range QoderChatModels() {
		qoderEntries = append(qoderEntries, provider.ModelEntry{
			ID:            m.ID,
			DisplayName:   m.DisplayName,
			IsVL:          m.IsVL,
			IsReasoning:   m.IsReasoning,
			ContextLength: m.MaxInputTokens,
		})
	}
	provider.RegisterChatModels(provider.Qoder, qoderEntries)

	// Trae 静态目录（trae_models.go：2026-09-28 get_detail_param 实测）。
	traeEntries := make([]provider.ModelEntry, 0, len(TraeChatModels()))
	for _, m := range TraeChatModels() {
		traeEntries = append(traeEntries, provider.ModelEntry{
			ID:            m.ID,
			DisplayName:   m.Name,
			IsVL:          m.IsVL,
			IsReasoning:   m.IsReasoning,
			ContextLength: int64(m.MaxTokens),
		})
	}
	provider.RegisterChatModels(provider.Trae, traeEntries)
}

// brandChatStream 品牌 chat 桥接的统一函数形态（与 Qoder/Trae 两桥的
// 方法签名同构；codebuddy 走 ChatStreamContext 内置的 chatPaths 循环，不进表）。
type brandChatStream func(c *Client, ctx context.Context, a *auth.Auth, body []byte, conversationID, model string) (io.ReadCloser, int, []byte, error)

// registerBrandChatStream 注册品牌 chat 桥（New 构造期调用；运行期只读）。
func (c *Client) registerBrandChatStream(b provider.Brand, fn brandChatStream) {
	if c.brandStreams == nil {
		c.brandStreams = map[provider.Brand]brandChatStream{}
	}
	c.brandStreams[b] = fn
}

// brandStreamFor 取品牌桥；未注册（codebuddy/未知）返回 nil——调用方回落
// 内置循环。品牌串先经 provider.Normalize 归一（老凭证空 brand 归 codebuddy，
// 不进表，行为不变）。
func (c *Client) brandStreamFor(brand string) brandChatStream {
	return c.brandStreams[provider.Normalize(brand)]
}
