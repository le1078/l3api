// Package docsheet 腾讯文档在线表格推送（模型目录同步结果 → 共享表格）。
//
// 通道：腾讯文档 MCP 端点（POST {endpoint}，JSON-RPC 2.0 tools/call，
// Bearer token 鉴权）。2026-10-09 实测可用工具族：sheet.get_sheet_info /
// sheet.get_cell_data / sheet.set_range_value / sheet.clear_range_cells。
//
// 写入策略：整块重写（clear_range_cells + set_range_value）——目录是
// 「全量快照」语义，diff 增量写在共享表格里没有读者价值；整块重写保证
// 表格永远等于当前生效目录。A1 单元格保留（用户存的 API key），表格区
// 从第 3 行（0-based row 2）开始。
package docsheet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client 腾讯文档 MCP 推送客户端。
type Client struct {
	Endpoint string // MCP 端点，默认 DefaultEndpoint
	Token    string // Bearer token（腾讯文档 MCP 访问令牌）
	FileID   string // 文档 file_id（分享链接 /sheet/ 后的串）
	SheetID  string // 子表 tab id（分享链接 ?tab= 后的串）
	HTTP     *http.Client
}

// DefaultEndpoint 腾讯文档 MCP 官方端点。
const DefaultEndpoint = "https://docs.qq.com/openapi/mcp"

// 表格区布局（0-based 行）：row 0 = 用户保留区（A1 API key，不动）；
// row 2 = 标题；row 3 = 表头；row 4+ = 数据。同步时先写 row2..(dataRow+len-1)，
// 再清 dataRow+len .. clearLast 的旧数据尾行（见 SyncModels）。
const (
	titleRow  = 2
	headerRow = 3
	dataRow   = 4
	clearLast = 199 // 尾清结束行（覆盖历史最大行数）
	clearCols = 10  // 尾清列数（A..K）
)

// Header 表头列。
var Header = []string{"品牌", "模型ID", "展示名", "多模态", "推理", "上下文 tokens", "来源/口径"}

// rpcReq/rpcResp JSON-RPC 2.0 最小封装。
type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResp struct {
	Result *struct {
		StructuredContent *struct {
			Error string `json:"error"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call 发起一次 tools/call。HTTP 非 200 / JSON-RPC error / 工具内 error 均报错。
func (c *Client) call(ctx context.Context, tool string, args any) error {
	body, err := json.Marshal(rpcReq{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return err
	}
	ep := c.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docsheet mcp status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var rr rpcResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("docsheet mcp parse: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("docsheet mcp rpc error %d: %s", rr.Error.Code, rr.Error.Message)
	}
	if rr.Result != nil && rr.Result.IsError {
		return fmt.Errorf("docsheet mcp tool %s returned isError", tool)
	}
	if rr.Result != nil && rr.Result.StructuredContent != nil && rr.Result.StructuredContent.Error != "" {
		return fmt.Errorf("docsheet tool %s: %s", tool, rr.Result.StructuredContent.Error)
	}
	return nil
}

// cell set_range_value 的单元格值形态。
type cell struct {
	Row         int    `json:"row"`
	Col         int    `json:"col"`
	ValueType   string `json:"value_type"`
	StringValue string `json:"string_value"`
	NumberValue int64  `json:"number_value"`
	BoolValue   bool   `json:"bool_value"`
	Formula     string `json:"formula"`
}

// SyncModels 把模型目录整块写入共享表格（清空旧表区 → 写标题/表头/数据）。
// SyncModels 把模型目录整块写入共享表格（先写新表区 → 再清尾行残留）。
// rows 每行 7 列，语义对齐 Header；ctx 带超时由调用方控制。
// A1 保留区不动（用户存的 key）。
//
// 写入顺序（2026-10-12 修正）：先 set_range_value 写标题/表头/数据（覆盖旧值），
// 成功后再清掉新表最后一行之后的旧数据尾行。旧实现先清后写，写失败会留下
// 「已清空、未写入」的空表——2026-10-12 实际踩中（写入 >30s 超时后表格被清空），
// 与「失败保持旧表完整」的设计意图相悖。新顺序下写失败 = 旧表原样保留。
// 注意：调用方 ctx 与 Client.HTTP 超时需覆盖大表写入耗时（~650 单元格实测可达
// 数十秒），见 main.docsSheetClient 与 scheduler.pushModelSheet 的超时口径。
func (c *Client) SyncModels(ctx context.Context, rows [][]string) error {
	if c.Token == "" || c.FileID == "" || c.SheetID == "" {
		return fmt.Errorf("docsheet: token/file_id/sheet_id 未配置")
	}
	// 1) 写标题 + 表头 + 数据（覆盖旧表区）。
	values := make([]cell, 0, (len(rows)+2)*len(Header))
	putStr := func(r, col int, v string) {
		values = append(values, cell{Row: r, Col: col, ValueType: "STRING", StringValue: v})
	}
	putStr(titleRow, 0, fmt.Sprintf("l3api 可调用模型目录（自动同步，最近同步 %s）",
		time.Now().Format("2006-01-02 15:04")))
	for col, h := range Header {
		putStr(headerRow, col, h)
	}
	for i, row := range rows {
		for col := range Header {
			v := ""
			if col < len(row) {
				v = row[col]
			}
			putStr(dataRow+i, col, v)
		}
	}
	if err := c.call(ctx, "sheet.set_range_value", map[string]any{
		"file_id": c.FileID, "sheet_id": c.SheetID, "values": values,
	}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// 2) 清尾部残留（新表最后一行之后的历史数据行；新表比旧表短时才有实际作用）。
	// 失败返回错误但数据已完整写入（只留旧尾行，下轮推送自愈）。
	tailFirst := dataRow + len(rows)
	if tailFirst <= clearLast {
		if err := c.call(ctx, "sheet.clear_range_cells", map[string]any{
			"file_id": c.FileID, "sheet_id": c.SheetID,
			"start_row": tailFirst, "start_col": 0, "end_row": clearLast, "end_col": clearCols,
		}); err != nil {
			return fmt.Errorf("clear tail: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
