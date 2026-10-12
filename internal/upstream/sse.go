// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 流内 error 帧：整流按失败处理（此前会被当成空 content 的假成功）。
					// 返回 *StreamFrameError 携带原始 payload——handler 据此做
					// FrameKind 分类冷却（与流式路径同口径；此前聚合路径一律
					// 502 upstream_parse，6004/余额耗尽不冷却也不换号）。
					if _, ok := chunk["error"].(map[string]any); ok {
						return nil, &StreamFrameError{Payload: payload}
					}
					// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, fmt.Errorf("upstream stream contained no valid data events")
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// finish_reason==length 且 tool_call 的 arguments 是残缺 JSON（解析失败）
		// 时不把脏参数交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。
		// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
		if finishReason == "length" {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// 首现：保留 name 键原样（上游首片通常带非空 name；空 name 也照发，
			// 与 OpenAI 对「首帧无 name」的容忍一致），随后分片统一删除。
			seen[idx] = true
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-l3api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// StreamFrameError 流内错误帧（HTTP 200 开流后出现的 error 帧，如 6004 限流、
// 审核拦截）：流内容已照常透传给客户端（error-passthrough），此错误仅用于
// 调用方的事后记账——handler 据此撤销「开流即成功」的乐观记账（NoteSuccess/
// 负缓存清除），改按帧内容分类冷却（2026-09-28 修复：此前这类失败被记为成功，
// 熔断/连败计数被清、11102 负缓存被误清）。
type StreamFrameError struct {
	Payload string // 首个 error 帧的原始 payload（供 FrameKind 分类）
}

func (e *StreamFrameError) Error() string {
	return "upstream in-stream error frame: " + e.Payload
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
//
// StreamHint 变体（gateway_hint）：上游 error 帧透传时附加 error.gateway_hint
// 字段——message 原文不动，hint 并列补充；hintFn 返回空串或 nil 时与 Stream
// 行为逐字节一致。
// StreamOption StreamHint 的可选行为开关。
// WithMarkupRepair 会改写出站帧（把正文里的原生工具调用标记还原成
// delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	markup *MarkupRepair
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具
// 一次都没跑。挂上修复器后，这类标记在透传前被还原成 delta.tool_calls。
// 传 nil、或未启用的修复器（客户端没声明过 tools）→ 与原行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
// 空流兜底 error 帧（"empty upstream stream"）不带 hint（网关本地故障形态
// 未覆盖，不编造）。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	var o streamOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen := map[int]bool{}

	// firstErrFrame 记录首个透传的 error 帧 payload（流尾以 *StreamFrameError 返回）。
	firstErrFrame := ""

	// 标记修复（dsml.go）：正文里的原生工具调用标记还原成 delta.tool_calls。
	// 未启用（客户端没声明 tools）→ 整条流零改写零分配。
	// synthIdx 本流已还原调用数；maxToolIdx 上游已用过的最大 tool_call index
	// （合成 index 从其后接续，避免撞车）；toolFinishSent 供流尾兜底判定。
	repair := o.markup
	synthIdx := 0
	maxToolIdx := -1
	toolFinishSent := false

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-l3api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""

	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。上游 error 帧
	// （error-passthrough）与空流错误帧需保留 error 字段，不能被白名单剥掉，
	// 故经此写出。gateway_hint：上游 error 帧透出前按 hintFn 附加
	// error.gateway_hint 字段（error 对象上加一个键，message/code/requestId 等
	// 原文不动；hintFn 为 nil / 空串 / 非 JSON 帧 → 原样写出，零改写）。
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被
	// 摘掉后的可见文本）+ N 个 delta.tool_calls 帧 + 收尾 finish_reason 帧。
	// 没有命中标记时原样返回入参帧（单元素），与修复前逐字节一致。
	repairFrame := func(obj map[string]any) []map[string]any {
		if repair == nil || !repair.Enabled() {
			return []map[string]any{obj}
		}
		chs, ok := obj["choices"].([]any)
		if !ok {
			return []map[string]any{obj}
		}
		var calls []MarkupCall
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			d, ok := c["delta"].(map[string]any)
			if !ok {
				continue
			}
			txt, ok := d["content"].(string)
			if !ok || txt == "" {
				continue
			}
			emit, got := repair.Feed(txt)
			calls = append(calls, got...)
			if emit == "" {
				delete(d, "content")
			} else {
				d["content"] = emit
			}
		}
		if len(calls) == 0 {
			// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
			// tool_calls，客户端据此知道「回合以工具调用结束」。
			if synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
				toolFinishSent = true
			}
			return []map[string]any{obj}
		}
		// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前
		// 就认为回合已经结束。本帧本来没有 finish_reason 时**不得**补一帧收尾
		// ——上游的收尾帧随后就到，补了会变成两个 finish_reason。
		fr := takeFinishReason(obj)
		out := []map[string]any{obj}
		for _, call := range calls {
			idx := maxToolIdx + 1 + synthIdx
			synthIdx++
			out = append(out, markupCallFrame(obj, idx, call))
		}
		if fr != "" {
			if fr == "stop" {
				fr = "tool_calls"
			}
			out = append(out, finishOnlyFrame(obj, fr))
			toolFinishSent = fr == "tool_calls"
		}
		return out
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			// 非 JSON 帧：原样降级写出，不计数（与修复前一致）。
			if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
				return 0, werr
			}
			if fl != nil {
				fl.Flush()
			}
			return 0, nil
		}
		// 上游错误帧透传（error-passthrough）：带 error 键的帧**原样写出**，不走
		// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
		// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
		// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
		if _, hasErr := obj["error"]; hasErr {
			if firstErrFrame == "" {
				firstErrFrame = payload
			}
			if werr := writeRaw(payload); werr != nil {
				return 0, werr
			}
			return 1, nil
		}
		for _, ch := range repairFrame(obj) {
			// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
			stripToolCallNames(ch, toolCallSeen)
			// id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id 一律用缓存值，
			// 有自己 id 的帧保持原样（不同流分裂的帧允许各自 id）。
			if firstID == "" {
				if v, ok := ch["id"].(string); ok && v != "" {
					firstID = v
				}
			} else {
				if v, ok := ch["id"].(string); !ok || v == "" {
					ch["id"] = firstID
				}
			}
			// maxToolIdx 跟踪：上游已用过的最大 tool_call index，合成帧接续编号。
			if chs, ok := ch["choices"].([]any); ok {
				for _, ci := range chs {
					c, _ := ci.(map[string]any)
					if c == nil {
						continue
					}
					d, _ := c["delta"].(map[string]any)
					if d == nil {
						continue
					}
					tcs, _ := d["tool_calls"].([]any)
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						if v, ok := tc["index"].(float64); ok && int(v) > maxToolIdx {
							maxToolIdx = int(v)
						}
					}
				}
			}
			out, err := json.Marshal(normalizeFrame(ch))
			if err != nil {
				out = []byte(payload)
			}
			if _, werr := io.WriteString(w, "data: "+string(out)+"\n\n"); werr != nil {
				return 0, werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		return 1, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
			// [DONE] 统一在循环结束后写出，保证恰好一个。
			break readLoop
		case strings.HasPrefix(trimmed, "data: "):
			n, werr := writeFrame(strings.TrimPrefix(trimmed, "data: "))
			validFrames += n
			if werr != nil {
				return werr
			}
		case trimmed != "":
			// 注释/其他行：原样透传
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// 标记修复的尾部收尾：流结束时仍未判定的字节一律原文回吐（绝不吞字节，
	// 未闭合的块连起始标记一起交还）；若本流还原过调用、但上游从未给出收尾帧，
	// 补一帧 finish_reason: tool_calls，保证客户端不把「工具调用回合」读成
	// 「模型只说了话」。
	if repair != nil && repair.Enabled() {
		if tail := repair.Flush(); tail != "" {
			tailObj := map[string]any{
				"object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": tail},
				}},
			}
			raw, _ := json.Marshal(normalizeFrame(tailObj))
			if _, werr := io.WriteString(w, "data: "+string(raw)+"\n\n"); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
			validFrames++
		}
		if synthIdx > 0 && !toolFinishSent {
			raw, _ := json.Marshal(normalizeFrame(finishOnlyFrame(map[string]any{}, "tool_calls")))
			if _, werr := io.WriteString(w, "data: "+string(raw)+"\n\n"); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	// 网关本地空流兜底帧走 hintFn=nil 的直写路径：该形态未覆盖（不编造 hint），
	// 且 writeRaw 的 hintFn 闭包在空流路径下可能携带上一帧的上下文造成误配。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	if firstErrFrame != "" {
		return &StreamFrameError{Payload: firstErrFrame}
	}
	return nil
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
// message/code/requestId 等既有键原样保留（只加不改）；非 JSON / 无 error 对象 →
// payload 原样返回（宁可不加 hint 也不破坏原文透传）。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则
// 客户端在读到调用之前就认为回合结束了。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
			changed = true
		}
	}
	return changed
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// finishOnlyFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}
