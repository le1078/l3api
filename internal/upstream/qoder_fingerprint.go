// qoder_fingerprint.go Qoder 设备指纹（Cosy-Machine* 请求头）加载与注入。
//
// 背景（2026-09-28 修正，推翻本包早前的「桌面端专属、服务端无法代领」结论）：
// Qoder 每日 100 积分的真实领取路径是 Campaign 体系——
//
//	GET  /sash/api/v1/me/campaigns            筛 actionType=CLAIM_BENEFIT 且 claimStatus=CLAIMABLE
//	POST /sash/api/v1/me/campaigns/{id}/claim 幂等（响应带 replayed，重复领不重复发放）
//
// 但该接口有设备指纹门控：**不带 Cosy-Machine* 头时 campaigns 返回空数组**
// （表现为「今天没活动」），伪造指纹则 503 风控。早前的实现正是没带指纹头，
// 于是把「看不到活动」误判成了「服务端不开放代领」——典型的观测偏差。
//
// 指纹只能由本机 Qoder 客户端自带的 resources/umid/runtime-info.exe
// （阿里 securityguard SDK）生成；实测**指纹不绑 IP**，本机生成的指纹可在服务器
// 原样重放——云机网关只要拿到指纹即可代领，无需本机在线。
//
// 因此指纹作为独立运行时数据落盘（不混进 auths/ 凭证文件，避免与 token 的
// SaveAtomic 写回互相覆盖），由本机采集后推送，网关按 uid 索引取用，
// 文件变更按 (mtime,size) 自动热加载，无需重启网关。
//
// 文件形态（两种都支持）：
//
//	A 多账号（推荐）：{"<uid>": {"machineToken":"...", ...}, "<uid2>": {...}}
//	B 单账号兜底：   {"fingerprint": {"machineToken":"...", ...}}  —— 任意 uid 共用
//
// 字段名与本机采集侧（runtime-info 输出）保持一致。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 指纹文件位置：环境变量覆盖，默认 ./data/qoder_fingerprints.json。
// 容器工作目录 /app、data 卷挂载在 /app/data，默认路径即 /app/data/qoder_fingerprints.json。
const (
	qoderFingerprintEnv     = "L3API_QODER_FP_FILE"
	qoderFingerprintDefault = "./data/qoder_fingerprints.json"
)

// qoderFingerprint 一次请求所需的设备指纹（对应 Cosy-Machine* 头族）。
type qoderFingerprint struct {
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
	MachineCode  string `json:"machineCode"`
	MachineOS    string `json:"machineOS"`
	MachineID    string `json:"machineId"`
	CosyVersion  string `json:"cosyVersion"`
}

// valid 指纹可用的最低标准：machineToken 与 machineCode 至少有一个非空
// （与采集侧判定一致，避免空指纹被当成有效而发出无意义请求）。
func (f qoderFingerprint) valid() bool {
	return strings.TrimSpace(f.MachineToken) != "" || strings.TrimSpace(f.MachineCode) != ""
}

// qoderFingerprintPath 指纹文件路径（环境变量优先）。
func qoderFingerprintPath() string {
	if p := strings.TrimSpace(os.Getenv(qoderFingerprintEnv)); p != "" {
		return p
	}
	return qoderFingerprintDefault
}

// 指纹文件缓存：签到是低频调用（每日两次），不必每次解析；但本机守护会周期
// 覆盖推送新指纹（指纹有效期未公开，需定期刷新），故按 (mtime,size) 变更热加载。
var (
	qoderFPMu       sync.RWMutex
	qoderFPStamp    string
	qoderFPByUID    map[string]qoderFingerprint
	qoderFPShared   qoderFingerprint
	qoderFPSharedOK bool
)

// loadQoderFingerprint 取 uid 对应的指纹；取不到返回 nil（调用方降级为仅监测）。
// 文件缺失/损坏一律返回 nil 而非报错：指纹是「增强项」，不应阻断签到主流程。
func loadQoderFingerprint(uid string) *qoderFingerprint {
	path := qoderFingerprintPath()
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	stamp := st.ModTime().UTC().Format(time.RFC3339Nano) + "/" + strconv.FormatInt(st.Size(), 10)

	qoderFPMu.RLock()
	cached := qoderFPStamp
	byUID, shared, sharedOK := qoderFPByUID, qoderFPShared, qoderFPSharedOK
	qoderFPMu.RUnlock()

	if cached != stamp {
		byUID, shared, sharedOK = parseQoderFingerprintFile(path)
		qoderFPMu.Lock()
		qoderFPStamp, qoderFPByUID, qoderFPShared, qoderFPSharedOK = stamp, byUID, shared, sharedOK
		qoderFPMu.Unlock()
	}

	if fp, ok := byUID[uid]; ok && fp.valid() {
		return &fp
	}
	// 单账号兜底形态：任意 uid 共用 "fingerprint" 条目。
	if sharedOK && shared.valid() {
		return &shared
	}
	return nil
}

// parseQoderFingerprintFile 解析指纹文件，返回 (按 uid 索引, 共享条目, 共享条目是否有效)。
func parseQoderFingerprintFile(path string) (map[string]qoderFingerprint, qoderFingerprint, bool) {
	empty := func() (map[string]qoderFingerprint, qoderFingerprint, bool) {
		return nil, qoderFingerprint{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return empty()
	}
	defer f.Close()
	// 1MB 上限：防止误指向异常大文件把内存打满。
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return empty()
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return empty()
	}

	byUID := make(map[string]qoderFingerprint, len(generic))
	for k, v := range generic {
		if k == "fingerprint" {
			continue // 共享条目，下面单独解析
		}
		var fp qoderFingerprint
		if json.Unmarshal(v, &fp) != nil {
			continue
		}
		byUID[k] = fp
	}
	var shared qoderFingerprint
	sharedOK := false
	if v, ok := generic["fingerprint"]; ok {
		if err := json.Unmarshal(v, &shared); err == nil {
			sharedOK = true
		}
	}
	return byUID, shared, sharedOK
}

// qoderApplyFingerprint 把 token 与设备指纹写入请求头。
//
// fp 为 nil 时只写最小可用头（Authorization + Accept）——即「仅监测」模式；
// 该模式下 campaigns 大概率返回空数组，调用方须据此降级，
// **绝不可把空结果当成「今日无活动」**（这正是早前误判的根因）。
func qoderApplyFingerprint(req *http.Request, token string, fp *qoderFingerprint) {
	h := req.Header
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/json")
	if fp == nil {
		return
	}
	// 服务端目前不强校验客户端版本，但缺头会被按「陌生客户端」处理。
	cosyVersion := strings.TrimSpace(fp.CosyVersion)
	if cosyVersion == "" {
		cosyVersion = qoderCosyVersion
	}
	h.Set("User-Agent", "Qoder")
	h.Set("Cosy-ClientType", "10")
	h.Set("Cosy-Version", cosyVersion)
	h.Set("Cosy-MachineToken", fp.MachineToken)
	h.Set("Cosy-MachineType", fp.MachineType)
	h.Set("Cosy-MachineCode", fp.MachineCode)
	h.Set("Cosy-MachineOS", fp.MachineOS)
	h.Set("Cosy-MachineId", fp.MachineID)
}
