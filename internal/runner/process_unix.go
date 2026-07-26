//go:build darwin || linux

package runner

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
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

func (control processGroupControl) watchChildChanges() (<-chan time.Time, func()) {
	if control.terminal == nil {
		return nil, func() {}
	}

	ticker := time.NewTicker(25 * time.Millisecond)
	return ticker.C, func() {
		ticker.Stop()
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
	return control.setForeground(int32(control.parentProcessGroup))
}

func (control processGroupControl) setForeground(processGroup int32) error {
	wasIgnored := signal.Ignored(syscall.SIGTTOU)
	if !wasIgnored {
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
	}

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

func (control processGroupControl) foreground() (int32, error) {
	var processGroup int32
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_IOCTL,
		control.terminal.Fd(),
		uintptr(syscall.TIOCGPGRP),
		uintptr(unsafe.Pointer(&processGroup)),
	)
	if errno != 0 {
		return 0, errno
	}
	return processGroup, nil
}

func (control processGroupControl) suspendWithChild(childPID int) error {
	if control.terminal == nil {
		return nil
	}
	foreground, err := control.foreground()
	if err != nil {
		return err
	}
	if foreground == int32(childPID) {
		if err := control.restoreForeground(); err != nil {
			return err
		}
	}

	// The wrapped command is a nested foreground job. Stop unring so its
	// invoking shell can perform normal job control. Execution resumes here
	// after that shell runs fg/bg and sends SIGCONT to unring.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTSTP); err != nil {
		return err
	}
	foreground, err = control.foreground()
	if err != nil {
		return err
	}
	if foreground == int32(control.parentProcessGroup) {
		if err := control.setForeground(int32(childPID)); err != nil {
			return err
		}
	}
	return signalProcessGroup(childPID, syscall.SIGCONT)
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}
