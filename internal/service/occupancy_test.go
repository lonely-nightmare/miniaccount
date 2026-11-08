package service_test

// 外部复查（验收前）发现的占用协议缺陷回归：
// 同一账套经硬链接/符号链接被第二个读写进程打开时，
// 两边各写各的 WAL，关闭后丢数据。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"miniaccount/internal/service"
)

// 第二个读写打开同一账套必须被拒。
func TestWriterLockBlocksSecondRWOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")

	svc, err := service.Open(ctx, service.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "占用测试", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Open(ctx, service.Options{Path: db}); err == nil {
		t.Fatal("第二个读写打开应被拒")
	} else if !strings.Contains(err.Error(), "读写") {
		t.Fatalf("应报读写占用: %v", err)
	}
}

// 硬链接别名：不同路径、同一 inode —— 身份锁归并，
// 第二个读写打开一样被拒。
func TestWriterLockCoversHardlinkAlias(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")
	link := filepath.Join(dir, "alias.db")

	svc, err := service.Open(ctx, service.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "别名测试", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(db, link); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Open(ctx, service.Options{Path: link}); err == nil {
		t.Fatal("硬链接别名的第二个读写打开应被拒")
	}
}

// 只读打开与读写打开共存（WAL 多读单写是安全的）；
// 两个只读打开也共存。
func TestReadonlyCoexistsWithRW(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")

	svc, err := service.Open(ctx, service.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "共存测试", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	ro1, err := service.Open(ctx, service.Options{Path: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("读写打开时应能只读共存: %v", err)
	}
	defer ro1.Shutdown()
	ro2, err := service.Open(ctx, service.Options{Path: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("两个只读打开应共存: %v", err)
	}
	defer ro2.Shutdown()
}

// 硬链接别名的只读打开必须归并到持锁者的打开路径——
// SQLite 的 WAL 按「打开路径字符串」命名，经别名打开就用
// 另一份（不存在的）WAL，把已写入的草稿读成 0 条。
func TestReadonlyAliasSeesWriterWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")
	link := filepath.Join(dir, "alias.db")

	svc, err := service.Open(ctx, service.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Shutdown()
	if _, err := svc.CreateBook(ctx, service.CreateBookInput{
		CompanyName: "别名可见性", StartYear: 2025, StartMonth: 1,
		ThroughYear: 2025, CurrentYear: 2025, CurrentMonth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 写一张草稿——它躺在 book.db-wal 里，未经别名规约的
	// 只读打开只看得见主库，会漏读。
	sh := mustContact(t, svc, "shareholder", "张三")
	in := basicInput()
	in.Lines[1].ContactID = &sh
	d, err := svc.SaveVoucher(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Link(db, link); err != nil {
		t.Skip("此平台不支持硬链接")
	}
	ro, err := service.Open(ctx, service.Options{Path: link, ReadOnly: true})
	if err != nil {
		t.Fatalf("硬链接别名的只读打开应成功: %v", err)
	}
	defer ro.Shutdown()
	if _, err := ro.Voucher(ctx, d.ID); err != nil {
		t.Fatalf("别名只读端应看到写者 WAL 里的草稿: %v", err)
	}
	// 归并后服务报告的账套路径也必须是持锁者的路径。
	if ro.Path() != svc.Path() {
		t.Fatalf("别名只读应归并到 %q，实际 %q", svc.Path(), ro.Path())
	}
}
