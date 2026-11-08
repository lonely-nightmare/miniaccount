package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/service"
)

// ★ 幂等（§2.5 验收）：同键同内容重试返回原凭证；
// 同键异内容报 conflict；删后重试报 target_deleted。
func TestIdempotentCreateDraftVoucher(t *testing.T) {
	ctx := context.Background()
	svc := withAudit(t)
	sh := mustContact(t, svc, "shareholder", "张三")

	in := basicInput()
	in.Lines[1].ContactID = &sh
	const key, hash = "req-1", "h-aaa"

	d1, oc1, err := svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if oc1.Idempotent {
		t.Fatal("首次创建不该标 idempotent")
	}

	// 同键同内容重试：返回原凭证、不新建
	d2, oc2, err := svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !oc2.Idempotent || d2.ID != d1.ID {
		t.Fatalf("重试应命中原凭证: %+v id=%d", oc2, d2.ID)
	}
	if oc2.ModifiedSince {
		t.Fatal("未修改就标 modifiedSince")
	}
	if oc2.AuditState != "recorded" {
		t.Fatalf("audit_state = %q", oc2.AuditState)
	}

	// 凭证被改过 → modifiedSince
	upd := basicInput()
	upd.ID = d1.ID
	upd.Remark = "改过的草稿"
	upd.Lines[1].ContactID = &sh
	upd.ExpectedUpdatedAt = d2.UpdatedAt
	if _, err := svc.SaveVoucher(ctx, upd); err != nil {
		t.Fatal(err)
	}
	d3, oc3, err := svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !oc3.ModifiedSince || d3.Remark != "改过的草稿" {
		t.Fatalf("应返回当前态并标 modifiedSince: %+v", oc3)
	}

	// 同键异内容 → conflict
	_, _, err = svc.CreateDraftVoucherIdempotent(ctx, in, key, "h-bbb")
	if !errors.Is(err, service.ErrIdempotencyConflict) {
		t.Fatalf("异内容应报 conflict, got %v", err)
	}

	// 原凭证删后重试 → target_deleted，不重建
	cur, _ := svc.Voucher(ctx, d1.ID)
	if err := svc.DeleteVoucher(ctx, d1.ID, cur.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if !errors.Is(err, service.ErrIdempotentTargetDeleted) {
		t.Fatalf("删后重试应报 target_deleted, got %v", err)
	}

	// 没带键 → 退化为普通创建（不保证幂等）
	d4, oc4, err := svc.CreateDraftVoucherIdempotent(ctx, in, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if oc4.Idempotent || d4.ID == 0 {
		t.Fatal("无键路径应正常新建")
	}
	// 来源固定 mcp（凭证红线）
	if d4.Source != "mcp" {
		t.Fatalf("MCP 创建的凭证来源 = %q", d4.Source)
	}
}

// ★ 审计锁竞争修复：audit_state=failed 不该是终态——
// 同键重试要补记日志并把状态修回 recorded，
// 而不是永久回放旧失败（实测复现的缺口）。
func TestIdempotentRetryRepairsFailedAudit(t *testing.T) {
	ctx := context.Background()

	// 先不开审计：创建时 recordAudit 必败 → failed。
	service.SetAuditDir("")
	service.CloseAudit()
	svc := newSvc(t, 3)
	sh := mustContact(t, svc, "shareholder", "张三")
	in := basicInput()
	in.Lines[1].ContactID = &sh
	const key, hash = "req-repair", "h-rep"

	d1, oc1, err := svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if oc1.AuditState != "failed" {
		t.Fatalf("无审计目录时创建应标 failed，实际 %+v", oc1)
	}

	// 打开审计后同键重试：补记一条并修回 recorded。
	service.SetAuditDir(filepath.Join(t.TempDir(), "logs"))
	t.Cleanup(func() {
		service.CloseAudit()
		service.SetAuditDir("")
	})

	d2, oc2, err := svc.CreateDraftVoucherIdempotent(ctx, in, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !oc2.Idempotent || d2.ID != d1.ID {
		t.Fatalf("重试应命中原凭证: %+v", oc2)
	}
	if oc2.AuditState != "recorded" {
		t.Fatalf("补记后应 recorded，实际 %q: %s", oc2.AuditState, oc2.AuditError)
	}
	if entries := findAudit(t, svc, audit.ActionVoucherCreate); len(entries) == 0 {
		t.Fatal("补记应真的落一条创建日志")
	}
}
