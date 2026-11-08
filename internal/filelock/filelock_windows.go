//go:build windows

package filelock

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIdentity 取 (volume serial, file index) 对。
// syscall 包没有 GetFileInformationByHandle，走 x/sys/windows。
func fileIdentity(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("win:%x:%x:%x", info.VolumeSerialNumber,
		info.FileIndexHigh, info.FileIndexLow), nil
}

var (
	modkernel32       = windows.NewLazySystemDLL("kernel32.dll")
	procLockFileEx    = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx  = modkernel32.NewProc("UnlockFileEx")
	lockfileExclusive = 0x00000002
	lockfileFailImm   = 0x00000001
)

// tryLock 非阻塞取锁：LockFileEx + LOCKFILE_FAIL_IMMEDIATELY。
// x/sys/windows 的 LockFileEx 在较新版本可用，但为兼容旧版
// x/sys 直接用 syscall 发——两个 proc 是稳定 ABI。
func tryLock(f *os.File, exclusive bool) error {
	flags := uint32(lockfileFailImm)
	if exclusive {
		flags |= uint32(lockfileExclusive)
	}
	var ol syscall.Overlapped
	r, _, e := syscall.SyscallN(procLockFileEx.Addr(),
		f.Fd(), uintptr(flags), 0,
		1, 0, // 锁 1 字节即够——锁的是「有没有」，不是范围
		uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if e == windows.ERROR_LOCK_VIOLATION || e == windows.ERROR_IO_PENDING {
		return ErrOccupied
	}
	return e
}

func unlock(f *os.File) error {
	var ol syscall.Overlapped
	r, _, e := syscall.SyscallN(procUnlockFileEx.Addr(),
		f.Fd(), 0, 1, 0,
		uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return e
	}
	return nil
}
