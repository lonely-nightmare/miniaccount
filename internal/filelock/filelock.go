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
)

// ErrOccupied 表示目标已被其它进程占用（TryLock 语义，非阻塞）。
var ErrOccupied = errors.New("账套正被其它进程使用")

// ErrNoIdentity 表示平台/文件系统给不出稳定文件身份。
// 按文档要求此时拒绝打开，不回退到按路径锁。
var ErrNoIdentity = errors.New("无法确定账套文件身份")

// Lock 是一把已持有的文件锁。进程崩溃由 OS 释放；
// 正常路径必须 Release。
type Lock struct {
	f *os.File
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

// IdentityOf 解析路径并取文件身份。文件必须已存在。
func IdentityOf(path string) (Identity, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Identity{}, err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return Identity{}, err
	}
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

// PathIdentity 给尚不存在的文件一个锁身份：父目录 EvalSymlinks
// + 文件名。仅用于「新建账套」在文件落位前先占位。
func PathIdentity(path string) (Identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Identity{}, err
	}
	dir := filepath.Dir(abs)
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Identity{}, err
	}
	raw := "path:" + filepath.Join(rd, filepath.Base(abs))
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
func acquire(path string, exclusive bool) (*Lock, error) {
	for i := 0; i < maxRetries; i++ {
		id, err := IdentityOf(path)
		if err != nil {
			return nil, err
		}
		lk, err := AcquireIdentity(id, exclusive)
		if err != nil {
			return nil, err
		}
		id2, err := IdentityOf(path)
		if err != nil {
			// 复核时文件被删/换掉：释放锁，按错误处理
			lk.Release()
			return nil, err
		}
		if id2.Key == id.Key {
			return lk, nil
		}
		lk.Release()
	}
	return nil, fmt.Errorf("账套文件身份在取锁间隙反复变化：%s", path)
}

// Release 放锁并关闭锁文件句柄。文件不删除。
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
