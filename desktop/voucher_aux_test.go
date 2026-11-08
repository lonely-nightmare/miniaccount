package main

import (
	"testing"

	"miniaccount/internal/domain/money"
)

// 股东维度的凭证存成草稿 → 详情读回 → 原样再存，contactId 必须原路往返。
//
// 界面编辑草稿靠 detail 下发的辅助 id 回填；哪一层把 contactId 丢了，
// 「什么都不改再保存」就会被 checkAux 以「未填写往来单位」拒绝 ——
// 而界面上股东名照常显示（auxDesc 还在），用户根本看不出数据丢了。
func TestShareholderVoucherAuxRoundTrip(t *testing.T) {
	a, path := newApp(t)
	if _, f := a.OpenBook(path); f != nil {
		t.Fatal(f)
	}
	if _, f := a.CreateDemoBook(1); f != nil {
		t.Fatal(f)
	}
	// 演示账套已带股东「张三」；找到他的 id
	meta, f := a.VoucherMetaInfo()
	if f != nil {
		t.Fatal(f)
	}
	var shID int64
	for _, c := range meta.Contacts {
		if c.Kind == "shareholder" {
			shID = c.ID
		}
	}
	if shID == 0 {
		t.Fatal("演示账套没有股东")
	}
	d, err := a.SaveVoucher(VoucherRequest{
		Word: "记", Date: "2025-01-20",
		Lines: []VoucherLineRequest{
			{AccountCode: "1002", Summary: "收投资款", DebitYuan: "100.00"},
			{AccountCode: "3001", Summary: "收投资款", CreditYuan: "100.00", ContactID: &shID},
		},
		CreatedBy: "测试",
	})
	if err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}
	det, err := a.VoucherDetail(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range det.Lines {
		if l.AccountCode == "3001" {
			found = true
			if l.ContactID == nil || *l.ContactID != shID {
				t.Fatalf("★ 详情里 3001 行 contactId=%v，应为 %d", l.ContactID, shID)
			}
		}
	}
	if !found {
		t.Fatal("详情里没有 3001 行")
	}
	// 模拟编辑器「什么都不改再保存」：把 detail 的字段原样发回
	req2 := VoucherRequest{
		ID: d.ID, Word: det.Word, Date: det.Date, Remark: det.Remark,
		AttachCount: det.AttachCount, CreatedBy: det.CreatedBy,
	}
	for _, l := range det.Lines {
		dy, cy := "", ""
		if l.Debit != 0 {
			dy = money.Money(l.Debit).String()
		}
		if l.Credit != 0 {
			cy = money.Money(l.Credit).String()
		}
		req2.Lines = append(req2.Lines, VoucherLineRequest{
			AccountCode: l.AccountCode, Summary: l.Summary,
			DebitYuan: dy, CreditYuan: cy,
			ContactID: l.ContactID, EmployeeID: l.EmployeeID,
			DeptID: l.DeptID, ProjectID: l.ProjectID,
		})
	}
	if _, err := a.SaveVoucher(req2); err != nil {
		t.Fatalf("★ 重开草稿原样再保存失败: %v", err)
	}
}
