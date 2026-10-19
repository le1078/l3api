// sseline.go SSE 行读取的上界保护（2026-10-20）。
//
// 背景：所有 SSE 读循环（StreamHint / Aggregate / qoder / trae / chatStatsReader）
// 都用 bufio.Reader.ReadString('\n') 读行。ReadString 遇「不带换行的超长行」
// 会反复 append 扩容——**无上界**。上游（或中间层）故障时吐一条几十 MB 的
// 坏行（误把二进制/压缩流当 SSE、或畸形 JSON），单请求即可吃数百 MB 内存；
// 并发几个请求就能把容器 OOM。bufio 的 64KB 只影响填充块大小，不约束行长度。
//
// 本文件提供 ErrLineTooLong 与判定辅助：读循环在行超过上限时断流报错，
// 与既有「上游流解析失败 → 502 upstream_parse」路径汇合（客户端收到明确
// 错误，账号走 NoteFailures 连败兜底，而不是被一条坏流拖死）。
package upstream

import (
	"errors"
	"fmt"
)

// MaxSSELineLen 单条 SSE 行的字节上界（不含换行符）。
//
// 取 8MB 的理由：正常 SSE 帧远小于此——text delta 帧 1~5KB，最大的合法载荷
// 是 base64 图片/长工具结果回显，也在 1MB 量级。8MB 给足余量同时把最坏
// 单行内存钉死（并发 8 路 = 64MB，容器 512MB 限额内安全）。
// 超过即判定上游异常，断流报 ErrLineTooLong。
const MaxSSELineLen = 8 << 20

// ErrLineTooLong 单行 SSE 超过 MaxSSELineLen：上游行为异常（故障/被注入），
// 断流。读循环应返回包装本错误的 error，交由调用方走既有解析失败路径。
var ErrLineTooLong = errors.New("sse line exceeds maximum length")

// LineTooLongError 带实测行长的 ErrLineTooLong 包装（排障时能看到实际多大）。
type LineTooLongError struct {
	// Limit MaxSSELineLen（ offending 行已读到的字节下限）。
	Limit int
}

func (e *LineTooLongError) Error() string {
	return fmt.Sprintf("%s (limit %d bytes)", ErrLineTooLong.Error(), e.Limit)
}

func (e *LineTooLongError) Unwrap() error { return ErrLineTooLong }

// CheckSSELineLen 行长度判定：len > max 时返回 *LineTooLongError，否则 nil。
// max<=0 用 MaxSSELineLen。读循环每行调用（O(1)，仅比较）。
func CheckSSELineLen(line string, max int) error {
	limit := max
	if limit <= 0 {
		limit = MaxSSELineLen
	}
	if len(line) > limit {
		return &LineTooLongError{Limit: limit}
	}
	return nil
}
