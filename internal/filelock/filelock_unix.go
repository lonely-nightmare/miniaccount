//go:build !windows

package filelock

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 取 (device, inode) 对，符号链接已在外层解析。
func fileIdentity(abs string) (string, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrNoIdentity
	}
	return fmt.Sprintf("ino:%d:%d", st.Dev, st.Ino), nil
}

// tryLock 非阻塞取锁：LOCK_NB 失败即 ErrOccupied。
func tryLock(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK {
		return ErrOccupied
	}
	return err
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// processAlive 判断 owner 记录里的进程是否还活着。
// EPERM 也算活——我们只是没权限发信号。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
