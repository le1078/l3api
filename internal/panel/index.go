// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js         全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//   - echarts.min.js 用量时序图的图表库。走 go:embed 同源自加载而非 CDN：内网部署节点
//     访问外网 CDN 不稳定，且 CSP script-src 'self' 本身就不允许外部域。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

//go:embed theme.js
var themeJS []byte

//go:embed echarts.min.js
var echartsJS []byte

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// Cache-Control: no-cache——每次加载都回源协商（ETag/304 由浏览器自发），
// 防止旧 HTML 配新 app.js 的混搭（2026-10-10 实测：启发式缓存导致样式错位）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// echartsScript 输出 ECharts 运行时（同源脚本，供 CSP script-src 'self' 加载）。
// 用量趋势图用它渲染（smooth 曲线 + 富 tooltip），与手绘 SVG 相比省了交互代码，
// 代价是二进制多 ~1MB，且必须是本文件随二进制分发（CDN 外链会被 CSP 拦）。
// Cache-Control: immutable + 内容寻址由 app.js?v 版本号间接保证——echarts 自身
// 不随面板功能更新，改面板不动它，浏览器可长期缓存这份大文件；开发期改 echarts
// 必须手动递增 app.js?v（不影响此文件），否则可能拿到旧缓存导致 echarts 未定义。
func (p *Panel) echartsScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=86400") // 1 天；面板 no-cache，库文件可缓存
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(echartsJS)
}
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // 同上：随版本更新即时生效
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}

// themeScript 输出首帧主题引导脚本（head 内同步加载，绘制前定好 data-theme，
// 消除「深色闪一下再变白」）。no-cache：与 app.js 同口径，随版本即时生效。
func (p *Panel) themeScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(themeJS)
}
