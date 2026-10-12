package scheduler

// livesheet_test.go 人工触发的腾讯文档表格推送（live 探针，非常规测试）。
//
// 用法（在项目根执行）：
//
//	L3API_LIVE_PROBE=1 go test ./internal/scheduler -run TestLivePushModelSheet -v
//
// 复用线上真实数据链路：auths 目录加载账号 → 池 + upstream 现拉目录 →
// config.json 的 docs_sheet 配置整块重写共享表格。日常触发点是模型同步
// （qoder/trae 有变化）自动推送；workbuddy 动态目录（含国际版）变化不触发
// 自动推送，本探针用于「目录已变但自动触发未到」时手动补推
// （2026-10-12 国际版新增 deepseek-v4.1-flash 等 9 模型的场景即此）。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/docsheet"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/upstream"
)

func TestLivePushModelSheet(t *testing.T) {
	if os.Getenv("L3API_LIVE_PROBE") != "1" {
		t.Skip("set L3API_LIVE_PROBE=1 to run live sheet push")
	}
	root := "../.." // 测试运行于 internal/scheduler/，项目根在上两级
	raw, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		StateFile string `json:"state_file"`
		DocsSheet struct {
			Enabled bool   `json:"enabled"`
			Token   string `json:"token"`
			FileID  string `json:"file_id"`
			SheetID string `json:"sheet_id"`
		} `json:"docs_sheet"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if !cfg.DocsSheet.Enabled || cfg.DocsSheet.Token == "" {
		t.Fatal("docs_sheet 未启用或缺少 token（config.json）")
	}

	auths, err := auth.LoadDir(filepath.Join(root, "auths"))
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	p := pool.New(cfg.StateFile)
	p.SyncToDir(auths)
	p.SetBreaker(3, 30*time.Minute, 6*time.Hour) // 与线上默认同款（PeekHealthy 需熔断参数）

	s := New(Config{
		Pool:     p,
		Upstream: upstream.New(),
		DocsSheet: &docsheet.Client{
			Token:   cfg.DocsSheet.Token,
			FileID:  cfg.DocsSheet.FileID,
			SheetID: cfg.DocsSheet.SheetID,
			// MCP 写入大表（~650 单元格）实测 >30s（默认 client 超时），探针放宽到 5 分钟。
			HTTP: &http.Client{Timeout: 5 * time.Minute},
		},
	})

	rows, err := s.modelSheetRows()
	if err != nil {
		t.Fatalf("compose rows: %v", err)
	}
	t.Logf("composed %d rows", len(rows))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.cfg.DocsSheet.SyncModels(ctx, rows); err != nil {
		t.Fatalf("sheet push: %v", err)
	}
	t.Logf("sheet push OK (%d rows)", len(rows))
}
