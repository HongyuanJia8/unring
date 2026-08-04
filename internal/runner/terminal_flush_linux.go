//go:build linux

package runner

import "golang.org/x/sys/unix"

func flushTerminalInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
