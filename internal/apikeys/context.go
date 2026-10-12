// context.go 请求上下文携带已解析的 key 条目：withAuth 鉴权通过后写入，
// chat/messages/responses 各 handler 读取，用于白名单判定、限流与用量归因。
package apikeys

import "context"

type ctxKey struct{}

// WithEntry 把已解析的 key 条目挂进请求上下文。
func WithEntry(ctx context.Context, e Entry) context.Context {
	return context.WithValue(ctx, ctxKey{}, e)
}

// FromContext 取回 withAuth 写入的条目；不存在（裸用/未走 withAuth）返回 false。
func FromContext(ctx context.Context) (Entry, bool) {
	e, ok := ctx.Value(ctxKey{}).(Entry)
	return e, ok
}
