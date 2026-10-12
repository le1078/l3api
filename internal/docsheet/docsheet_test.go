// docsheet_test.go 腾讯文档 MCP 推送客户端单测（httptest 桩）+ live 验证
// （L3API_LIVE_PROBE=1 + DOCSHEET_LIVE_TOKEN 时打真实端点）。
package docsheet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubMCP 记录收到的 tools/call 序列并按需注入错误。
func stubMCP(t *testing.T, toolErr string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)
		calls = append(calls, req.Params.Name)
		e := ""
		if req.Params.Name == toolErr {
			e = "boom"
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"error":"`+e+`"},"isError":false}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSyncModelsHappyPath(t *testing.T) {
	srv, calls := stubMCP(t, "")
	c := &Client{Endpoint: srv.URL, Token: "tk", FileID: "f", SheetID: "s"}
	rows := [][]string{
		{"qoder", "gmodel", "GLM-5.3", "是", "是", "180000", "src"},
		{"trae", "glm-5.3", "GLM-5.3", "是", "是", "116000", "src"},
	}
	if err := c.SyncModels(context.Background(), rows); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// 顺序（2026-10-12 修正）：先写后清尾——写失败时旧表原样保留，
	// 不出现「已清空、未写入」的空表窗口。
	if len(*calls) != 2 || (*calls)[0] != "sheet.set_range_value" || (*calls)[1] != "sheet.clear_range_cells" {
		t.Fatalf("calls = %v, want write then clear-tail", *calls)
	}
}

func TestSyncModelsWriteErrorKeepsOldSheet(t *testing.T) {
	srv, calls := stubMCP(t, "sheet.set_range_value")
	c := &Client{Endpoint: srv.URL, Token: "tk", FileID: "f", SheetID: "s"}
	err := c.SyncModels(context.Background(), [][]string{{"a"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom error, got %v", err)
	}
	// 写失败 → 不执行清尾（表格保持旧数据完整）。
	if len(*calls) != 1 {
		t.Fatalf("calls = %v, clear-tail should not happen after write failure", *calls)
	}
}

func TestSyncModelsTailClearErrorAfterWrite(t *testing.T) {
	srv, calls := stubMCP(t, "sheet.clear_range_cells")
	c := &Client{Endpoint: srv.URL, Token: "tk", FileID: "f", SheetID: "s"}
	err := c.SyncModels(context.Background(), [][]string{{"a"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom error, got %v", err)
	}
	// 写已成功、仅尾清失败：数据完整，下轮推送自愈。
	if len(*calls) != 2 {
		t.Fatalf("calls = %v, want write then attempted clear-tail", *calls)
	}
}

func TestSyncModelsConfigMissing(t *testing.T) {
	c := &Client{}
	if err := c.SyncModels(context.Background(), nil); err == nil {
		t.Fatal("empty config should error")
	}
}
