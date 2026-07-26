//go:build linux

package runner

import "golang.org/x/sys/unix"

const childStoppedCode = 5 // CLD_STOPPED

func childStopped(pid int) (bool, error) {
	var info unix.Siginfo
	err := unix.Waitid(
		unix.P_PID,
		pid,
		&info,
		unix.WSTOPPED|unix.WNOHANG,
		nil,
	)
	if err != nil {
		return false, err
	}
	return info.Code == childStoppedCode, nil
}
