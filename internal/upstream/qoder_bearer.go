// qoder_bearer.go Qoder「Bearer COSY」本地签名构造。
//
// 协议来源（三重交叉验证，2026-09-22）：
//  1. 本机 Qoder CN 客户端 v1.1.57 真实抓包（~/.mitmproxy/round2.flows），
//     4 个 HTTP 200 的 agent_chat_generation 请求；
//  2. 客户端源码反读：D:\IDE\Qoder\resources\app\out\vs\code\electron-utility\
//     sharedProcessMain.js 中的 zw（加密工具类）与 C8（签名类）；
//  3. 活体验证：scripts/qoder_live.py 用真实 PAT 打通 PAT→jobToken→chat SSE。
//
// 算法（与客户端逐行等价）：
//
//	temp_key = 16 字节随机 ASCII（客户端：uuid4 去连字符后取前 16 位）
//	cosy_key = base64(RSA-1024 / PKCS1v15(temp_key))     ← 官方公钥见下
//	info     = base64(AES-128-CBC(key=temp_key, iv=temp_key, PKCS7)(identity_json))
//	payload  = base64(JSON{version, requestId, info, cosyVersion, ideVersion})
//	path     = URL.pathname 去掉 query string、去掉前导 "/algo"
//	sig      = MD5(payload + "\n" + cosy_key + "\n" + cosy_date + "\n" + body + "\n" + path)
//	Authorization: Bearer COSY.<payload>.<sig>
//
// 两个易错点（前一版即栽在此）：
//   - cosy_date 用 **Unix 秒**，不是 RFC1123（RFC1123 只用于 algo 域的 signature 头）；
//   - identity 是 **2 字段** {uid, security_oauth_token}，不是参考实现注释里的 9 字段。
//     抓包 info 密文 112/128 字节（b64 152/172 字符）只能装下 99/123 字节明文，
//     9 字段实测 277 字节，装不下。见 TestQoderIdentitySizeMatchesCapture。
package upstream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// qoderServerPubKeyPEM 服务器 RSA-1024 公钥（SPKI）。
// 取自客户端 sharedProcessMain.js 常量 Vw，与社区参考实现 fengyinxia/qoder2api 的
// SERVER_PUBKEY_PEM 完全一致（模数首字节 0xc0f22307…）。
//
// 注意：scripts/qoder_verify.py 早期版本用过另一把模数 0xc2fd5b8b… 的 PKCS#1 公钥，
// 与官方不符，会导致服务端解不出 temp_key —— 不要再用那把。
const qoderServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// qoderCosyVersion 随客户端版本上报；服务端目前不做强校验。
const qoderCosyVersion = "1.1.57"

// qoderCosyIdentity Bearer COSY 的 info 明文。
// 字段顺序与客户端一致（uid 在前）；实测只发这两个字段即可通过鉴权。
// 抓包中另有 172 字符的 info（多带一个 name 字段），服务端同样接受，
// 因此这里保持最小集——少一个字段少一分对不上的风险。
type qoderCosyIdentity struct {
	UID                string `json:"uid"`
	SecurityOauthToken string `json:"security_oauth_token"`
}

// qoderCosyPayload payload_b64 的明文结构。
// 字段顺序必须与客户端一致：Go 的 json.Marshal 按结构体声明顺序输出，
// 顺序错了虽然不影响 MD5（签名只覆盖 base64 后的字符串），但服务端解包会失败。
type qoderCosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IdeVersion  string `json:"ideVersion"`
}

// QoderCosySession 一次请求的会话材料（temp_key 用于 AES，需要留存到签名后）。
type QoderCosySession struct {
	TempKey []byte // 16 字节 AES 密钥（同时作 IV）
	CosyKey string // base64(RSA(temp_key))，进 Cosy-Key 头与签名
	Info    string // base64(AES(identity))，进 payload.info
}

// qoderLoadServerPubKey 解析官方 RSA 公钥。
func qoderLoadServerPubKey() (*rsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(qoderServerPubKeyPEM))
	if blk == nil {
		return nil, fmt.Errorf("qoder: server public key PEM decode failed")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("qoder: parse server public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok || rsaPub == nil {
		return nil, fmt.Errorf("qoder: server public key is not RSA (%T)", pub)
	}
	return rsaPub, nil
}

// qoderNewTempKey 生成 16 字节密钥。
//
// ⚠️ 必须与客户端完全一致：uuid4().hex[:16] —— 即 **16 个 ASCII 十六进制字符**
// （0-9a-f），不是 16 个任意随机字节。
//
// 这一条是 code 101 "Signature invalid" 的根因（2026-09-23 定位）：
// crypto/rand.Read 得到的随机字节含不可打印字节，虽然字节数同样是 16、
// 服务端也能解开 AES/RSA，但网关侧对 temp_key 做字符集校验或规范化处理，
// 非 hex 字符会导致后续校验失败。定位过程见 TestQoderRawReplay ——
// 原样重放 Python 生成的请求可通过，Go 自己生成则 101，且 identity/AES/
// payload 逐步比对全部一致，唯一差异就是这个字符集。
func qoderNewTempKey() ([]byte, error) {
	b := make([]byte, 16) // 32 hex chars → 取前 16
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("qoder: temp key rand: %w", err)
	}
	hexed := hex.EncodeToString(b) // 32 个 ASCII hex 字符
	return []byte(hexed[:16]), nil
}

// qoderAESEncrypt AES-128-CBC，key 即 iv，PKCS7 填充（Node createCipheriv 默认行为）。
func qoderAESEncrypt(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("qoder: aes cipher: %w", err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, buf)
	return out, nil
}

// QoderNewCosySession 构造一次请求所需的会话材料（对应客户端 encryptUserInfo）。
func QoderNewCosySession(identity qoderCosyIdentity) (*QoderCosySession, error) {
	pub, err := qoderLoadServerPubKey()
	if err != nil {
		return nil, err
	}
	tempKey, err := qoderNewTempKey()
	if err != nil {
		return nil, err
	}
	// identity 必须用紧凑序列化（无空格），与客户端 JSON.stringify 一致。
	identJSON, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("qoder: marshal identity: %w", err)
	}
	encAES, err := qoderAESEncrypt(tempKey, identJSON)
	if err != nil {
		return nil, err
	}
	encRSA, err := rsa.EncryptPKCS1v15(rand.Reader, pub, tempKey)
	if err != nil {
		return nil, fmt.Errorf("qoder: rsa encrypt temp key: %w", err)
	}
	return &QoderCosySession{
		TempKey: tempKey,
		CosyKey: base64.StdEncoding.EncodeToString(encRSA),
		Info:    base64.StdEncoding.EncodeToString(encAES),
	}, nil
}

// qoderBuildPayloadB64 构造 payload_b64（字段顺序 version→requestId→info→cosyVersion→ideVersion）。
func qoderBuildPayloadB64(info, requestID, cosyVersion, ideVersion string) (string, error) {
	raw, err := json.Marshal(qoderCosyPayload{
		Version:     "v1",
		RequestID:   requestID,
		Info:        info,
		CosyVersion: cosyVersion,
		IdeVersion:  ideVersion,
	})
	if err != nil {
		return "", fmt.Errorf("qoder: marshal cosy payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// QoderSigPath 由完整 URL 推导签名用的 path：
// 取 pathname → 去掉 query string → 去掉前导 "/algo"。
// 对应客户端 C8.removeQueryParameters()。
func QoderSigPath(rawURL string) string {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		p = u.Path
	}
	if i := strings.IndexByte(p, '?'); i > 0 {
		p = p[:i]
	}
	return strings.TrimPrefix(p, "/algo")
}

// QoderSign COSY 签名：MD5(payload_b64 \n cosy_key \n cosy_date \n body \n path)。
// cosyDate 为 Unix 秒字符串（与 Cosy-Date 头同一个值）。
func QoderSign(payloadB64, cosyKey, cosyDate, body, path string) string {
	sum := md5.Sum([]byte(payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + path))
	return hex.EncodeToString(sum[:])
}

// QoderBuildBearer 端到端构造 Authorization 头与 Cosy-Key 头。
//
// rawURL 必须是完整 URL（含 query），body 必须是 Encode=1 编码后的字符串
// ——签名覆盖的是**编码后**的 body，不是明文 JSON。
func QoderBuildBearer(identity qoderCosyIdentity, rawURL, body, requestID string) (bearer, cosyKey, cosyDate string, err error) {
	sess, err := QoderNewCosySession(identity)
	if err != nil {
		return "", "", "", err
	}
	payload, err := qoderBuildPayloadB64(sess.Info, requestID, qoderCosyVersion, "")
	if err != nil {
		return "", "", "", err
	}
	// Cosy-Date 与签名里用的是同一个值，必须同一次取，否则必然 Signature invalid。
	cosyDate = strconv.FormatInt(time.Now().Unix(), 10)
	bearer = "Bearer COSY." + payload + "." +
		QoderSign(payload, sess.CosyKey, cosyDate, body, QoderSigPath(rawURL))
	return bearer, sess.CosyKey, cosyDate, nil
}
