//go:build darwin

package runner

import (
	"syscall"
	"unsafe"
)

const (
	childStoppedCode = 5 // CLD_STOPPED
	waitIDProcess    = 1 // P_PID
)

func childStopped(pid int) (bool, error) {
	// Darwin's x/sys package exposes the waitid constants and syscall number
	// but not a wrapper. siginfo_t begins with three int32 fields:
	// si_signo, si_errno, and si_code. The oversized buffer leaves room for
	// the architecture's remaining siginfo_t fields.
	var info [16]uint64
	_, _, errno := syscall.Syscall6(
		syscall.SYS_WAITID,
		waitIDProcess,
		uintptr(pid),
		uintptr(unsafe.Pointer(&info)),
		syscall.WSTOPPED|syscall.WNOHANG,
		0,
		0,
	)
	if errno != 0 {
		return false, errno
	}
	code := *(*int32)(unsafe.Add(unsafe.Pointer(&info), 8))
	return code == childStoppedCode, nil
}
