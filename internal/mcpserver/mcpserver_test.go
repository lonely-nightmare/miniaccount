package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/mcpserver"
	"miniaccount/internal/service"
)

func newPair(t *testing.T, readonly bool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	svc, err := service.Open(ctx, service.Options{
		Path: filepath.Join(t.TempDir(), "t.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Shutdown() })
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "MCP 测试公司", TaxType: "general",
		StartYear: 2025, StartMonth: 1, ThroughYear: 2025,
		CurrentYear: 2025, CurrentMonth: 3,
	}); err != nil {
		t.Fatal(err)
	}

	srv := mcpserver.New(svc, mcpserver.Options{ReadOnly: readonly, Operator: "测试员"})
	st, ct := mcp.NewInMemoryTransports()
	go srv.Run(ctx, st)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// 契约面：恰好 14 读 + 6 写工具，3 个资源，无 prompts。
func TestToolSurface(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 20 {
		names := []string{}
		for _, tl := range tools.Tools {
			names = append(names, tl.Name)
		}
		t.Fatalf("工具数 = %d，应为 20: %v", len(tools.Tools), names)
	}
	want := map[string]bool{
		"book_info": false, "list_accounts": false, "list_periods": false,
		"query_vouchers": false, "get_voucher": false, "get_ledger": false,
		"trial_balance": false, "balance_sheet": false, "income_statement": false,
		"list_contacts": false, "list_departments": false, "list_employees": false,
		"list_invoices": false, "list_bank_flows": false,
		"create_draft_voucher": false, "update_draft_voucher": false,
		"delete_draft_voucher": false, "upsert_contact": false,
		"save_department": false, "update_employee": false,
	}
	for _, tl := range tools.Tools {
		if _, ok := want[tl.Name]; !ok {
			t.Fatalf("清单外工具: %s", tl.Name)
		}
		want[tl.Name] = true
	}
	for n, ok := range want {
		if !ok {
			t.Fatalf("缺少工具: %s", n)
		}
	}

	res, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range res.Resources {
		uris[r.URI] = true
	}
	for _, u := range []string{"book://info", "book://accounts", "book://periods"} {
		if !uris[u] {
			t.Fatalf("缺少资源 %s", u)
		}
	}
}

// 金额契约：返回的 structuredContent 里金额是元字符串，不是分。
func TestAmountContractIsYuan(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "remark": "契约测试",
			"lines": []map[string]any{
				{"accountCode": "1001", "debit": "12.34"},
				{"accountCode": "1002", "credit": "12.34"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("创建失败: %v", res.Content)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	v, _ := sc["voucher"].(map[string]any)
	if v["totalDebit"] != "12.34" {
		t.Fatalf("金额应按元字符串返回，实际 %v", v["totalDebit"])
	}
	if v["source"] != "mcp" {
		t.Fatalf("MCP 凭证来源 = %v", v["source"])
	}
}

// 只读模式：写工具返回 readonly 错误，读工具正常。
func TestReadonlyMode(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, true)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "lines": []map[string]any{
				{"accountCode": "1001", "debit": "1"},
				{"accountCode": "1002", "credit": "1"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("只读模式下写工具应报错")
	}
	tc, _ := json.Marshal(res.Content)
	if !strings.Contains(string(tc), "readonly") {
		t.Fatalf("错误应为 readonly: %s", tc)
	}

	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "book_info", Arguments: map[string]any{},
	}); err != nil || res.IsError {
		t.Fatalf("只读模式下读工具应可用: err=%v res=%v", err, res.IsError)
	}
}
