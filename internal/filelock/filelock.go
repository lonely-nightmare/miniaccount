// Package filelock 提供跨进程的账套文件占用协议。
//
// # 为什么需要它
//
// GUI 的「恢复备份」只关自己的连接；stdio/CLI/另一个进程
// 持旧连接继续读写时，恢复替换 DB 文件、清 WAL/SHM ——
// 旧进程对着已换 inode 的句柄继续写，数据写进「幽灵账套」。
//
// # 设计（docs/删除AI模块改为MCP服务能力.md §2.4）
//
//   - 锁身份 = **文件身份**（unix: dev+inode；windows: volume+
//     file index），不是路径。符号链接、硬链接别名天然归并到
//     同一身份、同一把锁。
//   - 锁文件统一放 `~/.mini-account/locks/<身份散列>.lock`，
//     不在账套旁边：只读目录里的账套也能取锁，恢复替换账套
//     文件时锁文件不受影响。文件常驻不删除——删除会引入
//     重建竞态，进程崩溃由 OS 释放锁。
//   - 取锁时序：stat 得身份 → 取锁 → 复核路径当前身份与所锁
//     身份一致（间隙里文件被换则释放重取）。
//   - 平台/文件系统给不出稳定文件身份时**拒绝打开**，不回退
//     到按路径锁——路径锁防不住软链绕过。
//   - 已知限制：文件锁不吊销不遵守协议的进程（旧版本软件、
//     第三方工具直接改库）。
package filelock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrOccupied 表示目标已被其它进程占用（TryLock 语义，非阻塞）。
var ErrOccupied = errors.New("账套正被其它进程使用")

// ErrNoIdentity 表示平台/文件系统给不出稳定文件身份。
// 按文档要求此时拒绝打开，不回退到按路径锁。
var ErrNoIdentity = errors.New("无法确定账套文件身份")

// Lock 是一把已持有的文件锁。进程崩溃由 OS 释放；
// 正常路径必须 Release。gate 是独占获取时连带持有的
// 路径闸门锁，Release 时先放主锁再放闸门。
// effective 是经 owner 记录归并后的实际打开路径：
// 同一文件被另一进程经别的路径名持有（硬链接别名）时，
// 必须用它登记的路径打开——SQLite 的 WAL 按打开路径字符串
// 命名，经别名打开会用另一份 WAL，读到的是过期数据。
type Lock struct {
	f         *os.File
	gate      *Lock
	effective string
}

// Identity 是文件系统级别的文件身份，跨路径别名稳定。
type Identity struct {
	Key string // 已散列，可直接做锁文件名
	raw string
}

// Dir 返回锁文件目录，确保存在。
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".mini-account", "locks")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// CanonicalPath 把路径规约到「真实位置」：
//   - 文件存在：全路径 EvalSymlinks —— 连末段的文件符号链接
//     一起解析（/link/b.db → /real/b.db）。
//   - 末段是悬空的符号链接（恢复暂存原库、目标短暂不存在时
//     就是这种状态）：EvalSymlinks 解不动，但链接本身在——
//     Readlink 跳到目标再整轮重试，不跳就按别的名字算出
//     另一把闸门绕过恢复锁。
//   - 其余不存在：逐级向上找能解析的祖先，剩余段原样接回。
//
// 锁身份、SQLite 实际打开路径一律用规约结果——SQLite 的 WAL
// 文件按「打开时传入的路径字符串」命名，两个名字=两份 WAL。
func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	for i := 0; i < 40; i++ {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r
		}
		fi, err := os.Lstat(abs)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			break
		}
		tgt, err := os.Readlink(abs)
		if err != nil {
			break
		}
		if !filepath.IsAbs(tgt) {
			tgt = filepath.Join(filepath.Dir(abs), tgt)
		}
		abs = filepath.Clean(tgt)
	}
	cur := abs
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// IdentityOf 解析路径并取文件身份。文件必须已存在。
func IdentityOf(path string) (Identity, error) {
	abs := CanonicalPath(path)
	raw, err := fileIdentity(abs)
	if err != nil {
		return Identity{}, err
	}
	if raw == "" {
		return Identity{}, ErrNoIdentity
	}
	sum := sha256.Sum256([]byte(raw))
	return Identity{Key: hex.EncodeToString(sum[:16]), raw: raw}, nil
}

// PathIdentity 给尚不存在的文件一个锁身份：规约路径哈希。
// 仅用于「新建账套」在文件落位前先占位；已存在文件经
// CanonicalPath 归并后，所有符号链接别名落到同一把闸门。
func PathIdentity(path string) (Identity, error) {
	raw := "path:" + CanonicalPath(path)
	sum := sha256.Sum256([]byte(raw))
	return Identity{Key: "p-" + hex.EncodeToString(sum[:16]), raw: raw}, nil
}

const maxRetries = 3

// AcquireShared 取共享占用（打开账套）。非阻塞：已有人持独占
// → ErrOccupied（「恢复中，请稍后」语义由调用方包装）。
func AcquireShared(path string) (*Lock, error) { return acquire(path, false) }

// AcquireExclusive 取独占占用（恢复账套）。非阻塞：有人持共享
// → ErrOccupied（「正被使用，请先断开」语义由调用方包装）。
func AcquireExclusive(path string) (*Lock, error) { return acquire(path, true) }

// AcquireIdentity 按已得身份取锁（新建账套的路径锁、以及
// acquire 内部使用）。
func AcquireIdentity(id Identity, exclusive bool) (*Lock, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return LockFile(filepath.Join(dir, id.Key+".lock"), exclusive)
}

// LockFile 直接对一个指定的锁文件取锁——用于「锁的是这个目录」
// 而不是「锁的是某个账套身份」的场景（如审计日志目录）。
func LockFile(path string, exclusive bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f, exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// acquire 按「stat 身份 → 取锁 → 复核身份」时序取锁。
// 取锁间隙里账套文件被换掉（恢复完成）时，手里的锁锁的是
// 旧身份——释放并按新身份重取，有界重试防乒乓。
// acquire 是 Open/Restore 的统一入口。
//
// ★ 路径锁是「闸门」：双方必经同一把 p-锁，再在它的保护下
// 解析身份、取身份锁。这堵死了「路径锁→身份锁」的换锁窗口。
// 顺序固定 p→i，绝不反向，不会死锁。
//
// 共享获取：身份锁落手即放闸门（闸门只管「换锁」这一刻）。
// 独占获取：闸门随返回的 Lock 一直持有到 Release ——
// 恢复要替换文件，身份锁只锁旧 inode；闸门不放手，其它
// opener 就过不了路径这一道，换不掉 inode 也进不来。
// 释放顺序固定 i→p。
func acquire(path string, exclusive bool) (*Lock, error) {
	pid, err := PathIdentity(path)
	if err != nil {
		return nil, err
	}
	gate, err := AcquireIdentity(pid, exclusive)
	if err != nil {
		return nil, err
	}
	keepGate := exclusive
	if !keepGate {
		defer gate.Release()
	}

	for i := 0; i < maxRetries; i++ {
		id, err := IdentityOf(path)
		if err != nil {
			if keepGate {
				gate.Release()
			}
			return nil, err
		}
		lk, err := AcquireIdentity(id, exclusive)
		if err != nil {
			if keepGate {
				gate.Release()
			}
			return nil, err
		}
		id2, err := IdentityOf(path)
		if err != nil {
			// 复核时文件被删/换掉：释放锁，按错误处理
			lk.Release()
			if keepGate {
				gate.Release()
			}
			return nil, err
		}
		if id2.Key == id.Key {
			// 身份锁定后主路径归并：owner 记录存「首个持锁
			// 进程的规约路径」。经硬链接别名进来的 opener
			// 规约路径不同但身份相同——必须照登记路径打开，
			// 才会看到同一份 WAL（WAL 按打开路径字符串命名）。
			eff, err := claimOpenPath(id, CanonicalPath(path))
			if err != nil {
				lk.Release()
				if keepGate {
					gate.Release()
				}
				return nil, err
			}
			if keepGate {
				lk.gate = gate
			}
			lk.effective = eff
			return lk, nil
		}
		lk.Release()
	}
	if keepGate {
		gate.Release()
	}
	return nil, fmt.Errorf("账套文件身份在取锁间隙反复变化：%s", path)
}

// EffectivePath 返回经 owner 记录归并后的实际打开路径；
// 无归并（独占获取、路径锁占位）时返回空串，调用方用原路径。
func (l *Lock) EffectivePath() string { return l.effective }

// claimOpenPath 决定本次打开用哪条路径：owner 表存每个持锁
// 进程的「pid → 归并后路径」，取首个活着的登记路径；都死了
// 或表空 → 登记自己的规约路径。
//
// 一行一活人的原因是「登记进程退出」不该让表失效：写进程还
// 活着、它的登记行还在，后开者继续归并到它在用的路径——
// 归并进的是「实际打开路径」，不是「各自想要的路径」。
//
// 共享锁不互斥——两个首开进程可能同时看到「表空」，「读→判
// →写」必须另串行，所以开一把 <身份>.claim 短锁（顺序固定
// gate→i→claim，不回头，不死锁）。陈行按 pid 活性 + 「记录
// 路径仍指向同一身份」双重校验回收。
func claimOpenPath(id Identity, mine string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	// .claim 只被持有「读几行+写一个文件」这一瞬，但取不到
	// 也不能直接放弃——短退避等一拍即可。
	var claim *Lock
	for i := 0; i < 20; i++ {
		claim, err = LockFile(filepath.Join(dir, id.Key+".claim"), true)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrOccupied) {
			return "", err
		}
		time.Sleep(10 * time.Millisecond)
	}
	if claim == nil {
		return "", fmt.Errorf("等待账套打开路径登记锁超时")
	}
	defer claim.Release()

	owner := filepath.Join(dir, id.Key+".owner")
	live := readOwnerTable(owner, id)
	eff := mine
	if len(live) > 0 {
		eff = live[0].path
	}
	// 自己这行也要登记（登记的是归并后的实际打开路径）——
	// 否则前面的人一退，这张表就断了继承人。
	me := os.Getpid()
	rows := live[:0]
	for _, r := range live {
		if r.pid != me {
			rows = append(rows, r)
		}
	}
	rows = append(rows, ownerRow{pid: me, path: eff})
	tmp := owner + ".tmp"
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString(strconv.Itoa(r.pid))
		sb.WriteByte('\t')
		sb.WriteString(r.path)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, owner); err != nil {
		return "", err
	}
	return eff, nil
}

// ownerRow 是 owner 表的一行：持锁进程 → 它实际打开的路径。
type ownerRow struct {
	pid  int
	path string
}

// readOwnerTable 读 owner 表并按「pid 活 + 路径仍指向同一
// 文件身份」过滤出活行；解析不了的行直接丢。
func readOwnerTable(owner string, id Identity) []ownerRow {
	b, err := os.ReadFile(owner)
	if err != nil {
		return nil
	}
	var out []ownerRow
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		pid, p := parseOwner(ln)
		if pid <= 0 || p == "" || !processAlive(pid) {
			continue
		}
		if rid, err := IdentityOf(p); err != nil || rid.Key != id.Key {
			continue
		}
		out = append(out, ownerRow{pid: pid, path: p})
	}
	return out
}

// parseOwner 解析一行「pid\t路径」。
func parseOwner(s string) (int, string) {
	pidStr, p, ok := strings.Cut(s, "\t")
	if !ok {
		return 0, ""
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0, ""
	}
	return pid, p
}

// WriterKey 由账套身份推导「写者锁」的身份：读写打开在
// 共享占用之外再占一把独占锁，挡住第二个读写进程（硬链接/
// 符号链接别名归并到同一身份）；只读打开不取它，可与一个
// 读写进程共存——SQLite WAL 下多读单写是安全的。
func WriterKey(id Identity) Identity {
	return Identity{Key: "w-" + id.Key, raw: "writer:" + id.raw}
}

// Release 放锁并关闭锁文件句柄。文件不删除。
// 持有闸门时按取锁逆序释放：先身份锁，后路径闸门。
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlock(l.f)
	_ = l.f.Close()
	l.f = nil
	if l.gate != nil {
		l.gate.Release()
		l.gate = nil
	}
}
