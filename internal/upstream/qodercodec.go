// qodercodec.go Qoder（通义灵码 Cosy 平台）"Encode=1" 通道的自定义 Base64 编解码。
//
// 算法（经真实抓包 203KB 请求体解码验证，见 docs/qoder-protocol.md §1）：
//  1. 私有字母表 64 字符 + '$' 填充，与标准表逐位对应；
//  2. 三段交换：把串按长度 n 切成 [0,a) [a,n-a) [n-a,n)（a=n/3），首尾两段互换
//     （该置换为对合，编解码共用同一函数）；
//  3. 映射与交换的先后：解码 = 先按表映射回标准字符，再段交换，再标准 b64 解码；
//     编码为逆序（对合性保证同一交换函数）。
//
// 用途：Qoder jobToken 换取 / chat 请求体（body 直接为编码串，鉴权端点外层另包
// {"payload":...,"encodeVersion":"1"}，与 codec 无关）。
package upstream

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	qoderB64Alphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderB64Pad      = '$'
	qoderStdB64      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

// qoderSwapSegs 三段首尾互换（对合置换，a=n/3）。n<3 时恒等。
func qoderSwapSegs(s string) string {
	n := len(s)
	a := n / 3
	if a == 0 {
		return s
	}
	return s[n-a:] + s[a:n-a] + s[:a]
}

// QoderDecode 解码 Encode=1 载荷 → 原始字节（Qoder chat 请求体即编码后的 JSON）。
func QoderDecode(enc string) ([]byte, error) {
	n := len(enc)
	if n == 0 {
		return nil, fmt.Errorf("qoder decode: empty input")
	}
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := enc[i]
		switch {
		case c == qoderB64Pad:
			mapped[i] = '='
		default:
			idx := strings.IndexByte(qoderB64Alphabet, c)
			if idx < 0 {
				return nil, fmt.Errorf("qoder decode: illegal char %q at %d", c, i)
			}
			mapped[i] = qoderStdB64[idx]
		}
	}
	return base64.StdEncoding.DecodeString(qoderSwapSegs(string(mapped)))
}

// QoderEncode 编码原始字节 → Encode=1 载荷（QoderDecode 的精确逆）。
func QoderEncode(data []byte) string {
	std := base64.StdEncoding.EncodeToString(data)
	swapped := qoderSwapSegs(std)
	var b strings.Builder
	b.Grow(len(swapped))
	for i := 0; i < len(swapped); i++ {
		c := swapped[i]
		switch {
		case c == '=':
			b.WriteByte(qoderB64Pad)
		default:
			if idx := strings.IndexByte(qoderStdB64, c); idx >= 0 {
				b.WriteByte(qoderB64Alphabet[idx])
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
