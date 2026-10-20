// catalog.go 统一品牌模型目录注册表（v1.11 品牌插件化）。
//
// 目标：/v1/models 的品牌区块不再逐品牌散写（handler 里每品牌一段 if）——
// 各品牌在 upstream 侧把静态模型目录换算成 ModelEntry 注册进来，handler 按
// 注册表遍历输出。接入新品牌的静态模型目录 = 实现目录函数 + 一行 Register。
package provider

import (
	"strings"
	"sync"
)

// ModelEntry 统一模型目录条目（各品牌模型描述字段的并集形态）。
type ModelEntry struct {
	// ID 上游模型标识（不带品牌前缀；前缀是网关路由协议，输出时统一拼）。
	ID string
	// DisplayName 面板/客户端展示名。
	DisplayName string
	// IsVL 多模态（支持图片输入）。
	IsVL bool
	// IsReasoning 推理模型。
	IsReasoning bool
	// ContextLength 上下文窗口（token 数；0 = 未知，输出时省略）。
	ContextLength int64
	// 以下为面板「模型与档位」五列的能力字段（与 WorkBuddy 动态目录同口径）。
	// 0/空 = 未知，输出时省略——不编造。
	MaxOutputTokens    int64    // 最大输出 token（0=未知）
	Credits            string   // 积分倍率原文（"x0.50"；x0.00=免费）
	Efforts            []string // 支持的思考档位（空=固定档/未知）
	DefaultEffort      string   // 默认思考档
	CanDisableThinking bool     // 思考可关（off 档可用）
}

// catalog 品牌模型目录注册表（启动期注册、运行期只读——无需并发控制，
// mutex 只为防御测试期的重复注册）。
var (
	catalogMu sync.Mutex
	catalog   = map[Brand][]ModelEntry{}
)

// RegisterChatModels 注册品牌静态模型目录（重复注册整体覆盖，后者为准）。
func RegisterChatModels(b Brand, entries []ModelEntry) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalog[b] = append([]ModelEntry(nil), entries...)
}

// ChatModels 取品牌静态目录（拷贝；调用方可安全持有/修改）；未注册返回 nil。
func ChatModels(b Brand) []ModelEntry {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return append([]ModelEntry(nil), catalog[b]...)
}

// ChatBrands 返回已注册静态目录的品牌（按字母序，输出稳定）。
func ChatBrands() []Brand {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	out := make([]Brand, 0, len(catalog))
	for b := range catalog {
		out = append(out, b)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// NormalizeStrict 归一化品牌串（严格版）：合法值原样返回；未知值 ok=false，
// 由调用方决定拒绝（面板导入）或回落+告警（旧数据容错路径）。
// 与 Normalize 的关系：Normalize 是「回落 + 零拒绝」的容错口径（老文件兜底），
// 本函数是「显式暴露未知」的可观测口径——两者并存，调用方按场景选择。
func NormalizeStrict(s string) (Brand, bool) {
	b := Brand(strings.ToLower(strings.TrimSpace(s)))
	if Valid(string(b)) {
		return b, true
	}
	return b, false
}
