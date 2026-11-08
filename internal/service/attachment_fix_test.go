package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"miniaccount/internal/backup"
	"miniaccount/internal/service"
)

// ---------------------------------------------------------------------------
// 附件链路复查回归（验收实测 9 项）
// ---------------------------------------------------------------------------

// 文件名带 ?：SQLite URI 把 ? 当参数分隔符，未转义时
// `pre?copy.db` 实际打开的是 `pre` —— 附件关联写进错的账套，
// 文件却落在请求账套的 .files 里（实测复现）。
func TestOpenPathWithQuestionMark(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	svc, err := service.Open(ctx, service.Options{
		Path: filepath.Join(dir, "bookprefix?copy.db"),
	})
	if err != nil {
		t.Fatalf("带 ? 的账套路径应能打开: %v", err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "问号账套", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rd, _ := filepath.EvalSymlinks(dir)
	if _, err := os.Stat(filepath.Join(rd, "bookprefix?copy.db")); err != nil {
		t.Fatalf("实际数据库应是 bookprefix?copy.db: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rd, "bookprefix")); !os.IsNotExist(err) {
		t.Fatal("不应在 bookprefix 上另建数据库 —— ? 被当成了 DSN 参数分隔")
	}
}

// same.db 与 same.sqlite 曾经共用 same.files：互相列孤儿、
// 互相读取、互相打进备份（实测复现）。附件目录必须按完整
// 文件名派生。
func TestFilesDirDistinctPerExtension(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	a, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "same.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	b, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "same.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()

	if a.FilesDir() == b.FilesDir() {
		t.Fatalf("same.db 与 same.sqlite 不应共用附件目录: %s", a.FilesDir())
	}
	rd, _ := filepath.EvalSymlinks(dir)
	for want, got := range map[string]string{
		filepath.Join(rd, "same.db.files"):     a.FilesDir(),
		filepath.Join(rd, "same.sqlite.files"): b.FilesDir(),
	} {
		if got != want {
			t.Errorf("附件目录 = %s，期望 %s", got, want)
		}
	}
}

// 旧布局迁移：<stem>.files 与共享 .files 两代目录里、本账套
// 引用的文件（含 audit_evidence 的证据原件）都要搬进新目录
// （实测复现：仅被 audit_evidence 引用的文件被漏迁）。
func TestMigrateLegacyFilesDirCoversEvidence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")

	svc, err := service.Open(ctx, service.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "迁移测试", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	putLegacy := func(dir, name string, content []byte) string {
		sum := sha256.Sum256(content)
		h := hex.EncodeToString(sum[:])
		p := filepath.Join(dir, h[:2])
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, h), content, 0o644); err != nil {
			t.Fatal(err)
		}
		return h
	}
	rd, _ := filepath.EvalSymlinks(dir)

	// 两代旧目录各放一份：attachment 引用的在 <stem>.files，
	// audit_evidence 引用的在共享 .files。
	attHash := putLegacy(filepath.Join(rd, "x.files"), "发票.pdf", []byte("发票原件"))
	eviHash := putLegacy(filepath.Join(rd, ".files"), "证据.pdf", []byte("底稿原件"))

	if _, err := svc.DB().SQL().ExecContext(ctx, `
		INSERT INTO attachment
			(owner_type, owner_id, file_name, sha256, mime, size, kind, created_at)
		VALUES ('invoice', 1, '发票.pdf', ?, 'application/pdf', 4, '', '2025-01-01')`,
		attHash); err != nil {
		t.Fatalf("写 attachment 行失败: %v", err)
	}
	if _, err := svc.DB().SQL().ExecContext(ctx, `
		INSERT INTO audit_evidence
			(year, month, owner_type, owner_id, ref_kind, ref_id, ref_label,
			 sha256, file_name, file_size, note, linked_by, created_at)
		VALUES (2025, 1, 'conclusion', 1, 'attachment', 0, '',
		        ?, '证据.pdf', 4, '', '审计', '2025-01-01')`, eviHash); err != nil {
		t.Fatalf("写 audit_evidence 行失败: %v", err)
	}
	svc.Shutdown()

	// 重开触发迁移 —— 两类引用都要进 <完整名>.files。
	svc2, err := service.Open(ctx, service.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Shutdown()
	st, err := svc2.Attachments()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{attHash, eviHash} {
		if !st.Exists(h) {
			t.Errorf("迁移后 %s 应在新附件目录 %s 中", h[:12], svc2.FilesDir())
		}
	}
}

// 经符号链接恢复且 FilesDir 留空：附件必须落到规约后的
// 目标旁边（实测复现：DB 进真实路径、附件进链接名目录）。
func TestRestoreThroughSymlinkDerivesFilesDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 源账套：建账 + 挂附件 + 备份。
	src, err := service.Open(ctx, service.Options{Path: filepath.Join(dir, "src.db")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "源账套", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	d, err := src.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-10", Remark: "x", CreatedBy: "会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money100(100)},
			{AccountCode: "5001", Summary: "贷", Credit: money100(100)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	att, err := src.AttachDocument(ctx, "voucher", d.ID, "发票.pdf", []byte("原件"))
	if err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "b.mabak")
	if _, err := src.Backup(ctx, service.BackupOptions{Dest: bak, IncludeFiles: true}); err != nil {
		t.Fatal(err)
	}
	src.Shutdown()

	// 目标真实路径 + 指向它的符号链接；经链接恢复。
	target := filepath.Join(dir, "real.db")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rr, err := (&service.Service{}).Restore(ctx, service.RestoreOptions{
		Archive: bak, DBPath: link, // FilesDir 留空 → 由规约路径推导
	})
	if err != nil {
		t.Fatalf("经符号链接恢复失败: %v", err)
	}
	rd, _ := filepath.EvalSymlinks(dir)
	wantDir := filepath.Join(rd, "real.db.files")
	if rr.FilesDir != wantDir {
		t.Fatalf("附件落点 = %s，期望 %s（链接名目录会读不到）", rr.FilesDir, wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, att.Hash[:2], att.Hash)); err != nil {
		t.Fatalf("附件应落在真实路径旁: %v", err)
	}
	// 链接名目录不该被创建。
	if _, err := os.Stat(filepath.Join(rd, "link.db.files")); !os.IsNotExist(err) {
		t.Error("不应创建链接名的 .files 目录")
	}
	// 经链接重开，附件不应缺失。
	reopened, err := service.Open(ctx, service.Options{Path: link})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	list, err := reopened.ListAttachments(ctx, "voucher", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0].Missing {
		t.Fatalf("经链接打开后附件不应缺失: %+v", list)
	}
}

// 备份必须拒绝已损坏的附件：内容寻址下文件名即校验和，
// 内容与文件名不符的东西不能打包进备份（实测复现）。
func TestBackupRejectsCorruptedAttachment(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 1)

	d, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-10", Remark: "x", CreatedBy: "会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money100(100)},
			{AccountCode: "5001", Summary: "贷", Credit: money100(100)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	att, err := svc.AttachDocument(ctx, "voucher", d.ID, "发票.pdf", []byte("原件内容"))
	if err != nil {
		t.Fatal(err)
	}
	// 原地改坏：文件名还是原 hash，内容已经不同。
	p := filepath.Join(svc.FilesDir(), att.Hash[:2], att.Hash)
	if err := os.WriteFile(p, []byte("被篡改的内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Backup(ctx, service.BackupOptions{
		Dest: filepath.Join(t.TempDir(), "b.mabak"), IncludeFiles: true,
	})
	if !errors.Is(err, backup.ErrChecksumFail) {
		t.Fatalf("损坏的附件应让备份报 ErrChecksumFail，得到 %v", err)
	}
}

// 上传归属不存在 → 报错，且磁盘上不能留下实物文件
// （实测复现：报错后文件仍可读、进孤儿清单、进备份）。
func TestAttachToMissingOwnerLeavesNoFile(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 1)

	_, err := svc.AttachDocument(ctx, "voucher", 999999, "幽灵.pdf", []byte("x"))
	if err == nil {
		t.Fatal("往不存在的凭证上传应报错")
	}
	st, err := svc.Attachments()
	if err != nil {
		t.Fatal(err)
	}
	files, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("失败的上传不应留下文件，实际留下 %d 个", len(files))
	}
	orphans, err := svc.OrphanAttachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("孤儿清单应为空，实际 %d 项", len(orphans))
	}
}

// 覆盖率必须把「原件已丢失」的凭证算作无附件：
// 只查关联行会让删除文件后的凭证仍计入 withAttach
// （实测复现）。
func TestAttachmentCoverageCountsMissingFiles(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 1)

	d, err := svc.SaveVoucher(ctx, service.VoucherInput{
		Word: "记", Date: "2025-01-10", Remark: "x", CreatedBy: "会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money100(100)},
			{AccountCode: "5001", Summary: "贷", Credit: money100(100)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PostVoucher(ctx, d.ID, "会计"); err != nil {
		t.Fatal(err)
	}
	att, err := svc.AttachDocument(ctx, "voucher", d.ID, "发票.pdf", []byte("原件"))
	if err != nil {
		t.Fatal(err)
	}

	cov, err := svc.AttachmentCoverage(ctx, 2025, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Posted != 1 || cov.WithAttach != 1 || cov.Without != 0 {
		t.Fatalf("删除前应 posted=1 withAttach=1 without=0，实际 %+v", cov)
	}

	// 删掉物理文件（数据库关联仍在 —— 正是「原件丢失」形态）。
	if err := os.Remove(filepath.Join(svc.FilesDir(), att.Hash[:2], att.Hash)); err != nil {
		t.Fatal(err)
	}
	cov, err = svc.AttachmentCoverage(ctx, 2025, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Posted != 1 || cov.WithAttach != 0 || cov.Without != 1 {
		t.Fatalf("文件丢失后应 posted=1 withAttach=0 without=1，实际 %+v", cov)
	}
}
