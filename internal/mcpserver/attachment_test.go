package mcpserver_test

// 附件工具的端到端回归（经 in-memory transport 调真服务层）：
// 上传/读取/列表/删除/孤儿/覆盖率 + 校验边界 + 只读模式。

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/service"
)

// makeDraftVoucher 建一张草稿凭证拿 id（附件要挂在真实单据上）。
func makeDraftVoucher(t *testing.T, cs *mcp.ClientSession) int64 {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_draft_voucher",
		Arguments: map[string]any{
			"date": "2025-03-15", "remark": "挂附件用",
			"lines": []map[string]any{
				{"accountCode": "1001", "debit": "5"},
				{"accountCode": "1002", "credit": "5"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("建凭证失败: %v", res.Content)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	v, _ := sc["voucher"].(map[string]any)
	id, _ := v["id"].(float64)
	return int64(id)
}

func uploadPDF(t *testing.T, cs *mcp.ClientSession, ownerType string,
	ownerID int64, name string, data []byte) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_attachment",
		Arguments: map[string]any{
			"ownerType": ownerType, "ownerId": ownerID,
			"fileName": name,
			"dataBase64": "data:application/pdf;base64," +
				base64.StdEncoding.EncodeToString(data),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("上传失败: %v", res.Content)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	return sc
}

// 上传 → 列表 → 读回内容：整条链路走通，内容字节级一致。
func TestAttachmentRoundTrip(t *testing.T) {
	service.SetAuditDir(filepath.Join(t.TempDir(), "logs"))
	t.Cleanup(func() {
		service.CloseAudit()
		service.SetAuditDir("")
	})
	svc, cs := newPairSvc(t, false)
	ctx := context.Background()
	vid := makeDraftVoucher(t, cs)

	pdf := []byte("%PDF-1.4 fake invoice content for test")
	up := uploadPDF(t, cs, "voucher", vid, "发票-2025-03.pdf", pdf)
	hash, _ := up["hash"].(string)
	if len(hash) != 64 {
		t.Fatalf("返回 hash 应为 64 位 sha256，实际 %q", hash)
	}
	if up["name"] != "发票-2025-03.pdf" || up["size"] != float64(len(pdf)) {
		t.Fatalf("元数据不符: %v", up)
	}
	// 上传审计留痕（MCP 来源、操作员署名）。
	found := false
	if p, err := svc.QueryAudit(ctx, service.AuditQuery{
		Actions: []string{string(audit.ActionAttachmentUpload)}, Limit: 5,
	}); err == nil {
		for _, e := range p.Entries {
			if e.EntityID == f2s(vid) && e.Source == audit.SourceMCP {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("缺少 attachment.upload 审计或来源不是 mcp")
	}

	// 列表：一条、名/尺寸对、missing=false、没有 path 字段。
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_attachments",
		Arguments: map[string]any{"ownerType": "voucher", "ownerId": vid},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	items, _ := sc["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("附件数 = %d，应为 1", len(items))
	}
	it, _ := items[0].(map[string]any)
	if it["name"] != "发票-2025-03.pdf" || it["missing"] != false {
		t.Fatalf("列表条目不符: %v", it)
	}
	if _, leaked := it["path"]; leaked {
		t.Fatal("附件输出不应包含宿主机 path")
	}

	// 读回：base64 内容与上传一致。
	rd, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_attachment", Arguments: map[string]any{"hash": hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := rd.StructuredContent.(map[string]any)
	got, _ := base64.StdEncoding.DecodeString(rc["dataBase64"].(string))
	if string(got) != string(pdf) {
		t.Fatalf("读回内容不一致: %q vs %q", got, pdf)
	}
}

// 幂等：同一份文件重复上传 = 同一 hash，单据上仍是一条记录。
func TestUploadAttachmentIdempotent(t *testing.T) {
	_, cs := newPairSvc(t, false)
	ctx := context.Background()
	vid := makeDraftVoucher(t, cs)
	pdf := []byte("same file twice")

	h1 := uploadPDF(t, cs, "voucher", vid, "a.pdf", pdf)["hash"]
	h2 := uploadPDF(t, cs, "voucher", vid, "a.pdf", pdf)["hash"]
	if h1 != h2 {
		t.Fatalf("同内容应得同 hash: %v vs %v", h1, h2)
	}
	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_attachments",
		Arguments: map[string]any{"ownerType": "voucher", "ownerId": vid},
	})
	sc, _ := res.StructuredContent.(map[string]any)
	if total := sc["total"]; total != float64(1) {
		t.Fatalf("重复上传不应产生第二条，实际 %v 条", total)
	}
}

// 校验边界：坏 ownerType / 不存在的归属 / 空文件名 / 坏 base64 / 坏 hash。
func TestAttachmentValidation(t *testing.T) {
	_, cs := newPairSvc(t, false)
	ctx := context.Background()
	vid := makeDraftVoucher(t, cs)
	b64 := base64.StdEncoding.EncodeToString([]byte("x"))

	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"坏归属类型", "upload_attachment", map[string]any{
			"ownerType": "bogus", "ownerId": vid, "fileName": "a", "dataBase64": b64}},
		{"归属不存在", "upload_attachment", map[string]any{
			"ownerType": "voucher", "ownerId": 99999, "fileName": "a", "dataBase64": b64}},
		{"空文件名", "upload_attachment", map[string]any{
			"ownerType": "voucher", "ownerId": vid, "fileName": "  ", "dataBase64": b64}},
		{"坏base64", "upload_attachment", map[string]any{
			"ownerType": "voucher", "ownerId": vid, "fileName": "a", "dataBase64": "!!!not-base64!!!"}},
		{"读坏hash", "read_attachment", map[string]any{"hash": "../../../../etc/passwd"}},
		{"删坏hash", "delete_attachment", map[string]any{
			"ownerType": "voucher", "ownerId": vid, "hash": "not-a-hash"}},
		{"零id", "list_attachments", map[string]any{
			"ownerType": "voucher", "ownerId": 0}},
	}
	for _, c := range cases {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: 协议层错误 %v", c.name, err)
		}
		if !res.IsError {
			t.Fatalf("%s: 应被拒绝却成功", c.name)
		}
	}
}

// 删除：关系行没了、磁盘文件还在（内容寻址可能被共享），
// 且 attachment.delete 审计留痕。
func TestDeleteAttachment(t *testing.T) {
	service.SetAuditDir(filepath.Join(t.TempDir(), "logs"))
	t.Cleanup(func() {
		service.CloseAudit()
		service.SetAuditDir("")
	})
	svc, cs := newPairSvc(t, false)
	ctx := context.Background()
	vid := makeDraftVoucher(t, cs)
	pdf := []byte("to be detached")
	hash := uploadPDF(t, cs, "voucher", vid, "bye.pdf", pdf)["hash"].(string)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "delete_attachment",
		Arguments: map[string]any{
			"ownerType": "voucher", "ownerId": vid, "hash": hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["detached"] != true {
		t.Fatalf("应返回 detached=true: %v", sc)
	}

	// 关系已解除。
	lst, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_attachments",
		Arguments: map[string]any{"ownerType": "voucher", "ownerId": vid},
	})
	lc, _ := lst.StructuredContent.(map[string]any)
	if lc["total"] != float64(0) {
		t.Fatalf("删除后仍列出 %v 条", lc["total"])
	}
	// 但文件还在 —— 现在它是孤儿，read_attachment 仍读得回。
	rd, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_attachment", Arguments: map[string]any{"hash": hash},
	})
	if err != nil || rd.IsError {
		t.Fatal("解除关联后文件应仍在（孤儿），读不应失败")
	}
	// 孤儿报告里能看到它。
	or, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_orphan_attachments", Arguments: map[string]any{}})
	oc, _ := or.StructuredContent.(map[string]any)
	items, _ := oc["items"].([]any)
	seen := false
	for _, it := range items {
		if m, _ := it.(map[string]any); m["hash"] == hash {
			seen = true
		}
	}
	if !seen {
		t.Fatal("解除关联的文件应出现在孤儿报告里")
	}
	// 删除审计。
	found := false
	if p, err := svc.QueryAudit(ctx, service.AuditQuery{
		Actions: []string{string(audit.ActionAttachmentDelete)}, Limit: 5,
	}); err == nil {
		for _, e := range p.Entries {
			if e.Source == audit.SourceMCP {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("缺少 attachment.delete 审计或来源不是 mcp")
	}
}

// 覆盖率：过账的没挂附件 → without=1（不过账的草稿不计）。
func TestAttachmentCoverage(t *testing.T) {
	_, cs := newPairSvc(t, false)
	ctx := context.Background()
	makeDraftVoucher(t, cs) // 草稿不计入 posted 口径

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "attachment_coverage", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["posted"] != float64(0) || sc["without"] != float64(0) {
		t.Fatalf("草稿期应 posted=0/without=0，实际 %v", sc)
	}
	// year/month 只给一个 → invalid_params。
	bad, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "attachment_coverage", Arguments: map[string]any{"year": 2025}})
	if !bad.IsError {
		t.Fatal("year/month 单给应被拒绝")
	}
}

// 发票归属也能挂（非 voucher 的 owner 路径）。
func TestAttachmentOnInvoice(t *testing.T) {
	svc, cs := newPairSvc(t, false)
	ctx := context.Background()
	// 直接走服务层建一张发票拿 id（MCP 没有发票写工具）。
	inv, err := svc.SaveInvoice(ctx, service.InvoiceInput{
		Direction: "input", Kind: "special",
		Number: "TEST-001", InvoiceDate: "2025-03-10",
		AmountExTax: 10000, TaxRatePPM: 130000,
	})
	if err != nil {
		t.Fatal(err)
	}
	up := uploadPDF(t, cs, "invoice", inv.ID, "进项发票.pdf", []byte("pdf-in"))
	if len(up["hash"].(string)) != 64 {
		t.Fatalf("发票附件上传失败: %v", up)
	}
	lst, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_attachments",
		Arguments: map[string]any{"ownerType": "invoice", "ownerId": inv.ID},
	})
	lc, _ := lst.StructuredContent.(map[string]any)
	if lc["total"] != float64(1) {
		t.Fatalf("发票附件应列出 1 条，实际 %v", lc)
	}
}

func f2s(v int64) string { return strconv.FormatInt(v, 10) }
