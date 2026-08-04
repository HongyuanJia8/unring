//go:build darwin

package runner

import "golang.org/x/sys/unix"

func flushTerminalInput(fd int) error {
	// Darwin's FREAD value selects the terminal input queue for TIOCFLUSH.
	const terminalInputQueue = 1
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, terminalInputQueue)
}
