package service_test

// 外部复查发现的查询缺陷回归：
// VouchersPage 金额恒 0（分录未加载）、LedgerDetail 的
// closingBalance 被 limit 截断。

import (
	"context"
	"fmt"
	"testing"

	"miniaccount/internal/domain/period"
	"miniaccount/internal/service"
)

// query_vouchers 的 amount 必须来自分录合计。
func TestVouchersPageAmountFromLines(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)

	if _, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-15", Remark: "列表金额", CreatedBy: "李会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money100(200)},
			{AccountCode: "5001", Summary: "贷", Credit: money100(200)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := svc.VouchersPage(ctx, service.VoucherQuery{Year: 2025, Month: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应有 1 张凭证，实际 %d", len(page.Items))
	}
	if page.Items[0].Amount != money100(200) {
		t.Fatalf("列表金额 = %s，期望 200.00（分录合计）", page.Items[0].Amount)
	}
	if page.Items[0].Lines != 2 {
		t.Fatalf("分录行数 = %d，期望 2", page.Items[0].Lines)
	}
}

// get_ledger 的 closingBalance 是全期余额，不受 limit 截断。
func TestLedgerClosingIgnoresLimit(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)

	for i, amt := range []int64{100, 100, 100} {
		if _, err := svc.SaveVoucher(ctx, service.VoucherInput{
			Word: "记", Date: fmt.Sprintf("2025-01-%02d", i+1),
			Remark: "明细", CreatedBy: "李会计",
			Lines: []service.VoucherLineInput{
				{AccountCode: "1002", Summary: "借", Debit: money100(amt)},
				{AccountCode: "5001", Summary: "贷", Credit: money100(amt)},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.PostPeriodDrafts(ctx, period.NewKey(2025, 1), "王主管"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.LedgerDetail(ctx, service.LedgerDetailQuery{
		AccountPrefix: "1002", From: "2025-01-01", To: "2025-01-31", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Rows) != 1 {
		t.Fatalf("limit=1 应截断为 1 行: %+v", res)
	}
	// 期末余额是全期 300.00，不是截断位置的 100.00。
	if res.ClosingBalance != int64(money100(300)) {
		t.Fatalf("closingBalance = %d，期望 300.00", res.ClosingBalance)
	}
}
