package panel

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSIDReferences app.js 里 $('id') / getElementById('id') 引用的每个 id
// 必须在 index.html 中存在。
//
// 为什么需要：app.js/index.html 是两份独立维护的静态资源，Go 编译器与 JS 语法
// 检查都发现不了引用漂移——一次「JS 里 $('notify_channels')、HTML 里只有 name
// 没有 id」就让 loadConfig 在设置 value 时 TypeError，整页配置读取失败（用户
// 看到的报错是「读取配置失败：Cannot set properties of null」），且后续加载
// 步骤（如 key 列表）被连带中断。本测试把这类引用一致性固定为常驻质量门。
func TestAppJSIDReferences(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	idRe := regexp.MustCompile(`(?:\$\(|getElementById\()'([^']+)'`)
	htmlIDRe := regexp.MustCompile(`id="([^"]+)"`)

	ids := map[string]bool{}
	for _, m := range htmlIDRe.FindAllStringSubmatch(string(html), -1) {
		ids[m[1]] = true
	}
	missing := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(string(js), -1) {
		if !ids[m[1]] {
			missing[m[1]] = true
		}
	}
	if len(missing) > 0 {
		out := make([]string, 0, len(missing))
		for id := range missing {
			out = append(out, id)
		}
		sort.Strings(out)
		t.Fatalf("app.js 引用了 index.html 中不存在的 id（$() 将返回 null → 运行时 TypeError）: %v", out)
	}
}
