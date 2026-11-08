package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/domain/money"
	"miniaccount/internal/mcpserver"
	"miniaccount/internal/service"
)

func newPair(t *testing.T, readonly bool) *mcp.ClientSession {
	_, cs := newPairSvc(t, readonly)
	return cs
}

// newPairSvc 与 newPair 相同，但把底层 service 一并返回，
// 供需要先造业务数据再调工具的测试用。
func newPairSvc(t *testing.T, readonly bool) (*service.Service, *mcp.ClientSession) {
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
	return svc, cs
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

// 只读模式：写工具根本不注册（协议层报工具不存在），读工具正常。
func TestReadonlyMode(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, true)

	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tl.Tools {
		if strings.HasPrefix(tool.Name, "create_") ||
			strings.HasPrefix(tool.Name, "update_") ||
			strings.HasPrefix(tool.Name, "delete_") ||
			strings.HasPrefix(tool.Name, "upsert_") ||
			strings.HasPrefix(tool.Name, "save_") {
			t.Fatalf("只读模式不应注册写工具 %s", tool.Name)
		}
	}

	// 写工具不存在 → CallTool 直接是协议层错误。
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "lines": []map[string]any{
				{"accountCode": "1001", "debit": "1"},
				{"accountCode": "1002", "credit": "1"},
			},
		},
	}); err == nil {
		t.Fatal("只读模式调用不存在的写工具应报协议错误")
	}

	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "book_info", Arguments: map[string]any{},
	}); err != nil || res.IsError {
		t.Fatalf("只读模式下读工具应可用: err=%v res=%v", err, res.IsError)
	}
}

// 元字符串契约：分页凭证与报表输出的金额必须是 "12.34" 形态。
func TestMoneyFieldsAreYuanStrings(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	if res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "remark": "金额契约",
			"lines": []map[string]any{
				{"accountCode": "1001", "debit": "12.34"},
				{"accountCode": "1002", "credit": "12.34"},
			},
		},
	}); res.IsError {
		t.Fatalf("建凭证失败: %v", res.Content)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "query_vouchers", Arguments: map[string]any{"limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("查询失败: %v", res.Content)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	items, _ := sc["items"].([]any)
	if len(items) == 0 {
		t.Fatal("应有凭证")
	}
	item, _ := items[0].(map[string]any)
	if _, isStr := item["amount"].(string); !isStr {
		t.Fatalf("amount 应为元字符串，实际 %T(%v)", item["amount"], item["amount"])
	}
	if _, isNum := sc["total"].(float64); !isNum {
		t.Fatalf("total 应保持数字（分页计数），实际 %T(%v)", sc["total"], sc["total"])
	}
}

// 乐观锁：expectedUpdatedAt 缺失/空串必须报 invalid_params，不能静默放行。
func TestExpectedUpdatedAtRequired(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	cr, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "remark": "锁测试",
			"lines": []map[string]any{
				{"accountCode": "1001", "debit": "1"},
				{"accountCode": "1002", "credit": "1"},
			},
		},
	})
	v := cr.StructuredContent.(map[string]any)["voucher"].(map[string]any)
	vid := v["id"].(float64)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "delete_draft_voucher",
		Arguments: map[string]any{"id": vid, "expectedUpdatedAt": ""},
	})
	if !res.IsError {
		t.Fatal("空 expectedUpdatedAt 应报错")
	}
	tc, _ := json.Marshal(res.Content)
	if !strings.Contains(string(tc), "invalid_params") {
		t.Fatalf("应为 invalid_params: %s", tc)
	}
}

// patch 类型混淆：数字发给 remark 必须报 invalid_params，字段不动。
func TestPatchTypeMismatchRejected(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "upsert_contact",
		Arguments: map[string]any{
			"kind": "customer", "name": "类型测试",
			"remark": float64(123),
		},
	})
	if !res.IsError {
		t.Fatal("remark 发数字应报错")
	}
	tc, _ := json.Marshal(res.Content)
	if !strings.Contains(string(tc), "invalid_params") {
		t.Fatalf("应为 invalid_params: %s", tc)
	}
	// 确认没有写入半个联系人
	lst, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_contacts", Arguments: map[string]any{},
	})
	sc := lst.StructuredContent.(map[string]any)
	if sc["total"] != float64(0) {
		t.Fatalf("类型错误的请求不应落库: %v", sc)
	}
}

// 验收口径：update_employee 传白名单外字段（baseSalary/enabled）
// 必须报错，不得静默忽略。
func TestEmployeePatchWhitelist(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	for _, key := range []string{"baseSalary", "enabled", "departmentId"} {
		res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: "update_employee",
			Arguments: map[string]any{
				"id": float64(1), key: "x",
			},
		})
		if !res.IsError {
			t.Fatalf("%s 应被拒", key)
		}
		tc, _ := json.Marshal(res.Content)
		if !strings.Contains(string(tc), "白名单") {
			t.Fatalf("%s 应报白名单错误: %s", key, tc)
		}
	}
	// 白名单内字段（即使员工不存在）不应被键校验拦下——
	// 错误来自「员工不存在」而非 invalid_params。
	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "update_employee",
		Arguments: map[string]any{
			"id": float64(1), "phone": "13800000000",
		},
	})
	tc, _ := json.Marshal(res.Content)
	if strings.Contains(string(tc), "白名单") {
		t.Fatalf("白名单字段不应被拦: %s", tc)
	}
}

// §2.10：报表输出带 balanced 标志（勾稽异常不是工具错误）。
func TestReportsHaveBalancedFlag(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	for _, name := range []string{"balance_sheet", "income_statement"} {
		args := map[string]any{"year": float64(2025), "month": float64(3)}
		if name == "balance_sheet" {
			args = map[string]any{"asOf": "2025-03-31"}
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: name, Arguments: args,
		})
		if err != nil || res.IsError {
			tc, _ := json.Marshal(res.Content)
			t.Fatalf("%s 调用失败: %v %s", name, err, tc)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s 应返回结构化内容: %#v", name, res.StructuredContent)
		}
		bal, ok := sc["balanced"].(bool)
		if !ok {
			t.Fatalf("%s 缺 balanced 布尔标志: %v", name, sc)
		}
		if issues, has := sc["issues"].([]any); has && len(issues) > 0 && bal {
			t.Fatalf("%s 有 issues 时 balanced 应为 false: %v", name, sc)
		}
	}
}

// 复查发现：update_draft_voucher 不带回 word，保存时
// 「付」字凭证被改成默认「记」。
func TestUpdatePreservesVoucherWord(t *testing.T) {
	ctx := context.Background()
	svc, cs := newPairSvc(t, false)

	d, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "付", Date: "2025-01-15", Remark: "原摘要", CreatedBy: "李会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money.Money(10000)},
			{AccountCode: "5001", Summary: "贷", Credit: money.Money(10000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "update_draft_voucher",
		Arguments: map[string]any{
			"id": float64(d.ID), "expectedUpdatedAt": d.UpdatedAt,
			"remark": "新摘要",
		},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("更新失败: %s", b)
	}
	sc := res.StructuredContent.(map[string]any)
	v, _ := sc["voucher"].(map[string]any)
	if v["word"] != "付" {
		t.Fatalf("word = %v，期望保留「付」", v["word"])
	}
}

// 复查发现：list_invoices 公布 in|out 而底层用 input|output，
// 按文档传值返回空表。现在 in|out 映射、非法值报错。
func TestInvoiceDirectionMapping(t *testing.T) {
	ctx := context.Background()
	svc, cs := newPairSvc(t, false)

	if _, err := svc.SaveInvoice(ctx, service.InvoiceInput{
		Direction: "input", Kind: "special",
		Number: "DIR-1", InvoiceDate: "2025-01-08",
		SellerName: "供应商甲", AmountExTax: money.Money(100000),
		TaxRatePPM: 130000,
	}); err != nil {
		t.Fatal(err)
	}

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_invoices",
		Arguments: map[string]any{"direction": "in"},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("direction=in 调用失败: %s", b)
	}
	if sc := res.StructuredContent.(map[string]any); sc["total"] != float64(1) {
		t.Fatalf("direction=in 应查到 1 条进项: %v", sc)
	}
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_invoices",
		Arguments: map[string]any{"direction": "bogus"},
	})
	if res == nil || !res.IsError {
		t.Fatal("非法 direction 应报错而非返回空表")
	}
}

// 复查发现：list_bank_flows 公布的状态枚举与域值不符
// （unmatched/booked 都不存在）；其余非法值应报错。
func TestBankFlowStatusMapping(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_bank_flows",
		Arguments: map[string]any{"status": "unmatched"},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("别名 unmatched 应可用: %s", b)
	}
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_bank_flows",
		Arguments: map[string]any{"status": "bogus"},
	})
	if res == nil || !res.IsError {
		t.Fatal("非法 status 应报错")
	}
}

// 复查同类：upsert_contact 传未知字段不得静默忽略。
func TestContactPatchWhitelist(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "upsert_contact",
		Arguments: map[string]any{
			"kind": "customer", "name": "白名单测试",
			"typo": "x",
		},
	})
	if res == nil || !res.IsError {
		t.Fatal("未知字段应被拒")
	}
	tc, _ := json.Marshal(res.Content)
	if !strings.Contains(string(tc), "白名单") {
		t.Fatalf("应报白名单错误: %s", tc)
	}
}

// 复查发现：kind 枚举外的值会原样落库（saveContactTx 只验名称）。
func TestContactKindValidated(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "upsert_contact",
		Arguments: map[string]any{
			"kind": "bogus", "name": "脏数据测试",
		},
	})
	if res == nil || !res.IsError {
		t.Fatal("非法 kind 应被拒")
	}
	// 确认没有落库。
	lst, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_contacts", Arguments: map[string]any{},
	})
	if sc := lst.StructuredContent.(map[string]any); sc["total"] != float64(0) {
		t.Fatalf("非法 kind 不应落库: %v", sc)
	}
}

// 复查发现：map 输入的 id 非数字时静默落成 0，
// upsert 的「更新」会变成「新建」。
func TestNonNumericIDRejected(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "upsert_contact",
		Arguments: map[string]any{
			"id": "5", "kind": "customer", "name": "字符串id",
		},
	})
	if res == nil || !res.IsError {
		t.Fatal("非数字 id 应被拒（否则静默新建）")
	}
}

// 复查发现：create_draft_voucher 无 word 字段时凭证字
// 永远落成「记」，付款凭证数据就错了。
func TestCreateVoucherWord(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"word": "付", "date": "2025-01-15", "remark": "付款",
			"lines": []any{
				map[string]any{"accountCode": "1002", "debit": "100.00"},
				map[string]any{"accountCode": "5001", "credit": "100.00"},
			},
		},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("创建失败: %s", b)
	}
	sc := res.StructuredContent.(map[string]any)
	v, _ := sc["voucher"].(map[string]any)
	if v["word"] != "付" {
		t.Fatalf("word = %v，期望「付」", v["word"])
	}
	// 非法凭证字应被拒（域校验 ErrBadWord 直通）。
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"word": "bogus", "date": "2025-01-15",
			"lines": []any{
				map[string]any{"accountCode": "1002", "debit": "1.00"},
				map[string]any{"accountCode": "5001", "credit": "1.00"},
			},
		},
	})
	if res == nil || !res.IsError {
		t.Fatal("非法凭证字应被拒")
	}
}

// 复查发现：来源字段在更新路径被静默刷成 manual
// （repo 的 v.Source=="" 保留判断被 New 的默认 manual 绕过，
// 是死代码）。MCP 建的凭证更新后来源应仍是 mcp。
func TestUpdatePreservesVoucherSource(t *testing.T) {
	ctx := context.Background()
	cs := newPair(t, false)

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-01-15", "remark": "原摘要",
			"lines": []any{
				map[string]any{"accountCode": "1002", "debit": "100.00"},
				map[string]any{"accountCode": "5001", "credit": "100.00"},
			},
		},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("创建失败: %s", b)
	}
	created := res.StructuredContent.(map[string]any)["voucher"].(map[string]any)
	if created["source"] != "mcp" {
		t.Fatalf("MCP 创建来源 = %v", created["source"])
	}

	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "update_draft_voucher",
		Arguments: map[string]any{
			"id": created["id"], "expectedUpdatedAt": created["updatedAt"],
			"remark": "改摘要",
		},
	})
	if res == nil || res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("更新失败: %s", b)
	}
	v := res.StructuredContent.(map[string]any)["voucher"].(map[string]any)
	if v["source"] != "mcp" {
		t.Fatalf("更新后来源 = %v，应为 mcp（被刷成 manual 了）", v["source"])
	}
}
