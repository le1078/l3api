package upstream

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"testing"
)

// 真实抓包测试向量：2026-09-22 Qoder CN v1.1.57 agent_chat_generation 请求体
// 解码后明文前 240 字节（UUID/stream 等真实字段），再用同一算法编码回的完整
// 编码串。真实字母表 + 真实字段 + 完整长度（三段交换依赖全长，截断向量不可用）。
// 提取过程见 docs/qoder-protocol.md。
const qoderVecPlain = `{"request_id":"738a802e-f864-4055-ba1d-f84c23a60bd6","request_set_id":"d5642e4f-7e45-4a2b-a26b-cf5ca3b0a272","chat_record_id":"738a802e-f864-4055-ba1d-f84c23a60bd6","session_id":"d8af884e-67bd-4235-86f2-17f402885f15","stream":true,"chat_tas`

const qoderVecEnc = `Kzj*BrBEjLNkj)u(QePSp*l*KzlHgOlR&wD,w(j(KzG@pYV^atlRDOV*w%j(#wVRBIlRFOV@a*GoK^JZKL#Sw*lECYJSarBOjn#xCeJHg^wkD@p.G@CiVOBt#@B(#*.MDMFM#@j*VHTYj^B*GoKMPHgwxkKL#(WYDgW)DoByBMN^lHTIVRKLGS#IjMptjR_.j,.*#@gzGS#IjHVYV(T(VHmYKYDxg.Dxjwx(LzBMn*j^VI#@&fVOatDM&(jowwVRa.GSKiVSptDM&w#^B^#@#f#Op(B*f*NOF%JSF^JgW^DxCePSp*l*Kzj@#wVOawD*`

func TestQoderDecodeCapturedSample(t *testing.T) {
	dec, err := QoderDecode(qoderVecEnc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != qoderVecPlain {
		t.Fatalf("decoded mismatch:\n got %q\nwant %q", string(dec), qoderVecPlain)
	}
	// 反向：真实明文 → 编码 → 与真实抓包编码串逐字节一致。
	if enc := QoderEncode([]byte(qoderVecPlain)); enc != qoderVecEnc {
		t.Fatalf("encode mismatch:\n got %q\nwant %q", enc, qoderVecEnc)
	}
}

func TestQoderRoundTrip(t *testing.T) {
	// 随机 payload round-trip：decode(encode(x)) == x（对合置换的自逆性）。
	rng := rand.New(rand.NewSource(42))
	for _, size := range []int{1, 2, 3, 57, 60, 61, 300, 4096, 100_000} {
		payload := make([]byte, size)
		rng.Read(payload)
		got, err := QoderDecode(QoderEncode(payload))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: round-trip mismatch", size)
		}
	}
}

func TestQoderRoundTripJSON(t *testing.T) {
	// 真实请求体形状：JSON → 编码 → 解码 → JSON 语义等价。
	obj := map[string]any{
		"request_id": "738a802e-f864-4055-ba1d-f84c23a60bd6",
		"stream":     "True",
		"messages":   []map[string]any{{"role": "user", "content": "你好"}},
	}
	raw, _ := json.Marshal(obj)
	dec, err := QoderDecode(QoderEncode(raw))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(dec, &back); err != nil {
		t.Fatal(err)
	}
	if back["stream"] != "True" {
		t.Fatalf("round-trip json mismatch: %v", back)
	}
}
