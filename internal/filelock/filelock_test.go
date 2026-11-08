package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedExclusive(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.db")
	os.WriteFile(f, []byte("x"), 0o600)
	s1, e := AcquireShared(f)
	if e != nil {
		t.Fatal(e)
	}
	s2, e := AcquireShared(f)
	if e != nil {
		t.Fatal("共享不互斥", e)
	}
	if _, e := AcquireExclusive(f); e == nil {
		t.Fatal("有共享时独占应失败")
	}
	s1.Release()
	if _, e := AcquireExclusive(f); e == nil {
		t.Fatal("仍有共享时独占应失败")
	}
	s2.Release()
	x, e := AcquireExclusive(f)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := AcquireShared(f); e == nil {
		t.Fatal("有独占时共享应失败")
	}
	x.Release()
	if _, e := AcquireShared(f); e != nil {
		t.Fatal("释放后应可取锁", e)
	}
}

// 闸门协议（§2.4 追补）：路径锁是 Open/Restore 共同入口。
// 有人占着 p-锁（如建账途中、或 stat 时文件不存在的恢复），
// 身份锁那条路必须同样被挡——两把锁互斥不到一起曾是漏洞。
func TestGateBlocksAcrossIdentities(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")

	// 模拟「恢复正在独占一个尚不存在的文件」：
	pid, err := PathIdentity(db)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := AcquireIdentity(pid, true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()

	// 另一个进程此刻创建并想打开这个文件：
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireShared(db); !errors.Is(err, ErrOccupied) {
		t.Fatalf("闸门持独占时应挡住打开: %v", err)
	}
	if _, err := AcquireExclusive(db); !errors.Is(err, ErrOccupied) {
		t.Fatalf("闸门持独占时应挡住恢复: %v", err)
	}
}

// 共享闸门不挡共享打开（多开共读是正常语义），但挡独占。
func TestSharedGateStillBlocksExclusive(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := AcquireShared(db)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	if _, err := AcquireShared(db); err != nil {
		t.Fatalf("共享应可叠加: %v", err)
	}
	if _, err := AcquireExclusive(db); !errors.Is(err, ErrOccupied) {
		t.Fatalf("共享占用应挡住恢复独占: %v", err)
	}
}
