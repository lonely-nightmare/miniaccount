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

// 独占获取必须全程持有路径闸门 —— 恢复在持锁期间替换文件
// （换 inode），身份锁只锁旧 inode；闸门若先放了，另一个
// 进程就能过路径闸门、解析新 inode、拿到新身份锁打开新文件，
// 「恢复中」窗口被穿透。
func TestExclusiveGateHeldUntilRelease(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	x, err := AcquireExclusive(db)
	if err != nil {
		t.Fatal(err)
	}
	// 恢复进行中：同路径打开必须被拒（闸门互斥）。
	if _, err := AcquireShared(db); !errors.Is(err, ErrOccupied) {
		t.Fatalf("独占期间打开应被拒: %v", err)
	}
	// 模拟替换完成：新 inode 落位，打开仍应被拒。
	repl := filepath.Join(dir, "repl.db")
	if err := os.WriteFile(repl, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repl, db); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireShared(db); !errors.Is(err, ErrOccupied) {
		t.Fatalf("替换完成后、释放前打开仍应被拒: %v", err)
	}
	x.Release()
	// 释放后才能按新身份打开。
	if _, err := AcquireShared(db); err != nil {
		t.Fatalf("释放后应能打开新文件: %v", err)
	}
}

// 写者锁：读写打开再占一把 w- 独占锁，第二个读写进程被拒；
// 共享占用（只读打开）不受影响。
func TestWriterKeyBlocksSecondWriter(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "book.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := IdentityOf(db)
	if err != nil {
		t.Fatal(err)
	}
	w1, err := AcquireIdentity(WriterKey(id), true)
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Release()
	if _, err := AcquireIdentity(WriterKey(id), true); !errors.Is(err, ErrOccupied) {
		t.Fatalf("第二个写者应被拒: %v", err)
	}
	// 占用锁（共享）仍可叠加 —— 只读打开共存是设计语义。
	if _, err := AcquireShared(db); err != nil {
		t.Fatalf("写者锁不该挡共享占用: %v", err)
	}
}

// CanonicalPath 必须解析「末段文件符号链接」：恢复持锁期间，
// 指向目标路径的符号链接若只按父目录算，会得出不同的闸门
// 身份而绕过互斥去开刚被替换的新文件。
func TestCanonicalPathResolvesFileSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("此平台不支持符号链接")
	}
	if got := CanonicalPath(link); got != CanonicalPath(real) {
		t.Fatalf("文件符号链接应规约到真实路径: %q vs %q", got, real)
	}
	// 目录级符号链接同样归并。
	linkDir := filepath.Join(dir, "dlink")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Skip("此平台不支持符号链接")
	}
	viaDir := filepath.Join(linkDir, "real.db")
	if got := CanonicalPath(viaDir); got != CanonicalPath(real) {
		t.Fatalf("目录符号链接应规约: %q vs %q", got, real)
	}
	// 不存在的目标：解析存在的祖先再接回文件名。
	// （EvalSymlinks 的结果是全规约路径，/var→/private/var
	// 之类系统级链接也会一并展开，不能拿字面 dir 比。）
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(linkDir, "new.db")
	if got := CanonicalPath(missing); got != filepath.Join(rd, "new.db") {
		t.Fatalf("不存在文件应接回规约父目录: %q", got)
	}
}

// 恢复锁（独占）期间，经文件符号链接打开同一目标必须被拒——
// 闸门按规约路径互斥，别名绕不进。
func TestSymlinkAliasBlockedDuringExclusive(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("此平台不支持符号链接")
	}
	x, err := AcquireExclusive(real)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Release()
	if _, err := AcquireShared(link); !errors.Is(err, ErrOccupied) {
		t.Fatalf("独占期间经符号链接打开应被拒: %v", err)
	}
}

// 硬链接别名打开：owner 记录把第二个 opener 的路径归并到
// 首个持锁者的规约路径——SQLite 的 WAL 按打开路径字符串
// 命名，不归并就是两份 WAL，只读端静默读到过期数据。
func TestOwnerRecordRewritesAliasPath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.db")
	b := filepath.Join(dir, "b.db")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Skip("此平台不支持硬链接")
	}
	s1, err := AcquireShared(a)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Release()
	if eff := s1.EffectivePath(); eff != CanonicalPath(a) {
		t.Fatalf("首开应登记自己的规约路径: %q", eff)
	}
	s2, err := AcquireShared(b)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Release()
	if eff := s2.EffectivePath(); eff != CanonicalPath(a) {
		t.Fatalf("别名打开应归并到首开端路径 %q，实际 %q",
			CanonicalPath(a), eff)
	}
}
