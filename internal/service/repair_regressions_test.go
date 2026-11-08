package service_test

// 十一项体检问题的回归测试（docs/十一项体检问题修复.md）。
// 每个用例都先按审计报告里的复现路径走一遍，再断言修好的行为。

import (
	"context"
	"path/filepath"
	"testing"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/domain/voucher"
	"miniaccount/internal/service"
)

// A1：同目录的两本账不得共用附件目录。
// 以前是共享的 <dir>/.files —— 备份 A 打包 B 的附件，恢复 A 还删掉 B 的。
func TestFilesDirIsolatedPerBook(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	svcA, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "a.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer svcA.Shutdown()
	if _, err := svcA.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "甲公司", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	svcB, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "b.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer svcB.Shutdown()
	if _, err := svcB.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "乙公司", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if svcA.FilesDir() == svcB.FilesDir() {
		t.Fatalf("同目录两本账共用附件目录 %s，备份会互相污染", svcA.FilesDir())
	}
	if got := filepath.Base(svcA.FilesDir()); got != "a.files" {
		t.Errorf("甲账套附件目录 = %s，期望 a.files", got)
	}
	if got := filepath.Base(svcB.FilesDir()); got != "b.files" {
		t.Errorf("乙账套附件目录 = %s，期望 b.files", got)
	}
}

// A2：同一张发票不能生成两张凭证（串行重放；并发由事务内
// 条件更新 WHERE voucher_id IS NULL 保证）。
func TestPostInvoiceSecondCallRejected(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)
	sup := mustContact(t, svc, "supplier", "发票供应商")
	dept := mustDepartment(t, svc, "发票部门")

	v, err := svc.SaveInvoice(ctx, service.InvoiceInput{
		Direction: "input", Kind: "special",
		Number: "DUP-1", InvoiceDate: "2025-01-08",
		SellerName: "发票供应商", ContactID: &sup,
		AmountExTax: money100(1000), TaxRatePPM: 130000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyInvoice(ctx, v.ID, "王主管"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PostInvoice(ctx, v.ID, "王主管", "", &dept); err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if _, err := svc.PostInvoice(ctx, v.ID, "王主管", "", &dept); err == nil {
		t.Fatal("同一发票第二次生成应被拒绝")
	}
}

// A4：删除发票生成的草稿凭证，应把发票退回「已认证」、清掉 voucher_id。
func TestDeleteDraftUnlinksInvoice(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)
	sup := mustContact(t, svc, "supplier", "删草稿供应商")
	dept := mustDepartment(t, svc, "删草稿部门")

	v, err := svc.SaveInvoice(ctx, service.InvoiceInput{
		Direction: "input", Kind: "special",
		Number: "UNLINK-1", InvoiceDate: "2025-01-09",
		SellerName: "删草稿供应商", ContactID: &sup,
		AmountExTax: money100(800), TaxRatePPM: 130000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyInvoice(ctx, v.ID, "王主管"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PostInvoice(ctx, v.ID, "王主管", "", &dept); err != nil {
		t.Fatal(err)
	}
	vs, err := svc.Vouchers(ctx, service.VoucherQuery{Year: 2025, Month: 1, Keyword: "UNLINK-1"})
	if err != nil || len(vs) != 1 {
		t.Fatalf("找不到发票生成的草稿: %v", err)
	}
	vid := vs[0].ID
	// 以前这一步直接撞外键约束报错；现在删除前先解除单据关联。
	if err := svc.DeleteVoucher(ctx, vid, ""); err != nil {
		t.Fatalf("删除生成草稿失败: %v", err)
	}
	list, err := svc.Invoices(ctx, service.InvoiceQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, iv := range list {
		if iv.ID == v.ID {
			if iv.Posted {
				t.Error("发票仍标记为已入账，voucher_id 未解除")
			}
			if iv.Status != "verified" {
				t.Errorf("发票状态 = %s，期望回退到 verified", iv.Status)
			}
			return
		}
	}
	t.Fatal("发票不见了")
}

// B6：小规模纳税人取得进项专票，税额计入成本费用而不是挂进项税额。
func TestPostInvoiceSmallScaleFoldsTaxIntoCost(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "small.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "小规模公司", VATStatus: "small_scale",
		StartYear: 2025, StartMonth: 1, ThroughYear: 2025,
		CurrentYear: 2025, CurrentMonth: 3,
	}); err != nil {
		t.Fatal(err)
	}
	sup := mustContact(t, svc, "supplier", "小规模供应商")
	dept := mustDepartment(t, svc, "小规模部门")

	v, err := svc.SaveInvoice(ctx, service.InvoiceInput{
		Direction: "input", Kind: "special",
		Number: "SMALL-1", InvoiceDate: "2025-01-10",
		SellerName: "小规模供应商", ContactID: &sup,
		AmountExTax: money100(1000), TaxRatePPM: 130000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyInvoice(ctx, v.ID, "王主管"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PostInvoice(ctx, v.ID, "王主管", "560201", &dept); err != nil {
		t.Fatalf("小规模生成凭证失败: %v", err)
	}
	vs, err := svc.Vouchers(ctx, service.VoucherQuery{Year: 2025, Month: 1, Keyword: "SMALL-1"})
	if err != nil || len(vs) != 1 {
		t.Fatalf("找不到发票生成的草稿: %v", err)
	}
	det, err := svc.Voucher(ctx, vs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var expenseDebit money.Money
	for _, l := range det.Lines {
		if l.AccountCode == "560201" {
			expenseDebit = l.Debit
		}
		if l.AccountCode == "22210101" {
			t.Error("小规模账套不该出现进项税额分录")
		}
	}
	// 价税合计 1,130 全额计入费用。
	if expenseDebit != money100(1130) {
		t.Errorf("费用借方 = %s，期望 1130.00（含税入成本）", expenseDebit)
	}
}

// B7：只被草稿引用的部门不能被删除。
func TestDepartmentUsageCountsDraft(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)
	dept := mustDepartment(t, svc, "仅草稿引用部")

	if _, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-10", Remark: "草稿引部门", CreatedBy: "李会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "560201", Summary: "借", Debit: money100(100), DeptID: &dept},
			{AccountCode: "1002", Summary: "贷", Credit: money100(100)},
		},
	}); err != nil {
		t.Fatalf("保存草稿失败: %v", err)
	}

	u, err := svc.DepartmentUsageOf(ctx, dept)
	if err != nil {
		t.Fatal(err)
	}
	if u.Entries == 0 {
		t.Error("草稿里的部门引用没有计入用量（以前只数已过账总账）")
	}
	if err := svc.DeleteDepartment(ctx, dept); err == nil {
		t.Error("被草稿引用的部门不该能删除")
	}
}

// B9：结账预览要和真实结账算同一套账 —— 有收入草稿时，
// 预览必须显示草稿过账后的收入和结转分录。
func TestPreviewCloseMatchesDraftsIncluded(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)

	if _, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-15", Remark: "收入草稿", CreatedBy: "李会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "收款", Debit: money100(200)},
			{AccountCode: "5001", Summary: "收入", Credit: money100(200)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	pv, err := svc.PreviewClose(ctx, period.NewKey(2025, 1))
	if err != nil {
		t.Fatalf("结账预览失败: %v", err)
	}
	if pv.Income != money100(200) {
		t.Errorf("预览收入 = %s，期望 200.00（草稿应计入）", pv.Income)
	}
	if len(pv.Entries) == 0 {
		t.Error("预览应给出结转分录（以前按已过账总账算是空的）")
	}
}

// C：红字冲销成功必须写操作日志。
func TestReverseVoucherWritesAudit(t *testing.T) {
	ctx := context.Background()
	svc := withAudit(t)

	det, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-15", Remark: "待冲销", CreatedBy: "李会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "收款", Debit: money100(100)},
			{AccountCode: "5001", Summary: "收入", Credit: money100(100)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 草稿先过账（本应用唯一过账时机是结账/按期过账）。
	if _, err := svc.PostPeriodDrafts(ctx, period.NewKey(2025, 1), "王主管"); err != nil {
		t.Fatalf("过账失败: %v", err)
	}
	if _, err := svc.ReverseVoucher(ctx, det.ID, "王主管", ""); err != nil {
		t.Fatalf("红字冲销失败: %v", err)
	}
	entries := findAudit(t, svc, audit.ActionVoucherReverse)
	if len(entries) != 1 {
		t.Fatalf("冲销的 voucher.reverse 日志 = %d 条，期望 1", len(entries))
	}
}

// ★ 来源保留死代码（二轮自查复现）：voucher.New 恒置
// SourceManual，repo「v.Source=="" 才保留旧来源」恒不触发 ——
// 不带 Source 的更新把 bank/mcp 来源静默刷成 manual。
// 约定改为「更新路径空 Source = 沿用旧值」。
func TestUpdatePreservesSource(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)
	sh := mustContact(t, svc, "shareholder", "张三")

	in := basicInput()
	in.Lines[1].ContactID = &sh
	sid := int64(42)
	in.Source = voucher.SourceBank
	in.SourceID = &sid
	d, err := svc.SaveVoucher(ctx, in)
	if err != nil {
		t.Fatalf("建来源凭证失败: %v", err)
	}
	if d.Source != "bank" {
		t.Fatalf("创建后来源 = %q", d.Source)
	}

	upd := basicInput()
	upd.ID = d.ID
	upd.Remark = "改摘要"
	upd.Lines[1].ContactID = &sh
	upd.ExpectedUpdatedAt = d.UpdatedAt
	d2, err := svc.SaveVoucher(ctx, upd)
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if d2.Source != "bank" {
		t.Fatalf("更新后来源 = %q，应为 bank（被刷成 manual 了）", d2.Source)
	}
}
