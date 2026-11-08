package filelock

import (
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
