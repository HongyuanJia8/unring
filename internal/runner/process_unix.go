//go:build darwin || linux

package runner

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

type processGroupControl struct {
	terminal           *os.File
	parentProcessGroup int
}

func configureProcessGroup(command *exec.Cmd, stdin io.Reader) processGroupControl {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	terminal, ok := stdin.(*os.File)
	if !ok || !isTerminal(terminal) {
		return processGroupControl{}
	}

	command.SysProcAttr.Foreground = true
	command.SysProcAttr.Ctty = int(terminal.Fd())
	return processGroupControl{
		terminal:           terminal,
		parentProcessGroup: syscall.Getpgrp(),
	}
}

func signalProcessGroup(pid int, signal os.Signal) error {
	unixSignal, ok := signal.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, unixSignal)
}

func (control processGroupControl) restoreForeground() error {
	if control.terminal == nil {
		return nil
	}

	wasIgnored := signal.Ignored(syscall.SIGTTOU)
	if !wasIgnored {
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
	}

	processGroup := control.parentProcessGroup
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_IOCTL,
		control.terminal.Fd(),
		uintptr(syscall.TIOCSPGRP),
		uintptr(unsafe.Pointer(&processGroup)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}
