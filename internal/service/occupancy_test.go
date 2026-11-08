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
