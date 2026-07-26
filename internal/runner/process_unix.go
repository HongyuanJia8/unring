//go:build darwin || linux

package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/term"
)

func (control processGroupControl) handleApproval(
	childPID int,
	decide func() (bool, error),
) ApprovalResult {
	if control.terminal == nil {
		approved, err := decide()
		return ApprovalResult{Approved: approved, Err: err}
	}
	if err := signalProcessGroup(childPID, syscall.SIGSTOP); err != nil {
		return ApprovalResult{Err: fmt.Errorf("pause child for approval: %w", err)}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stopped, err := childStopped(childPID)
		if err == nil && stopped {
			break
		}
		if err != nil && !errors.Is(err, syscall.ECHILD) {
			_ = signalProcessGroup(childPID, syscall.SIGCONT)
			return ApprovalResult{Err: fmt.Errorf("wait for child to pause: %w", err)}
		}
		if time.Now().After(deadline) {
			_ = signalProcessGroup(childPID, syscall.SIGCONT)
			return ApprovalResult{Err: errors.New("timed out pausing child for approval")}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := control.restoreForeground(); err != nil {
		_ = signalProcessGroup(childPID, syscall.SIGCONT)
		return ApprovalResult{Err: fmt.Errorf("reclaim terminal for approval: %w", err)}
	}
	approved, decisionErr := decide()
	foregroundErr := control.setForeground(int32(childPID))
	continueErr := signalProcessGroup(childPID, syscall.SIGCONT)
	return ApprovalResult{
		Approved: approved,
		Err: errors.Join(
			decisionErr,
			wrapApprovalError("return terminal to child", foregroundErr),
			wrapApprovalError("resume child after approval", continueErr),
		),
	}
}

func wrapApprovalError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

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

func (control processGroupControl) reclaimStoppedChild(childPID int) error {
	if control.terminal == nil {
		return nil
	}
	foreground, err := control.foreground()
	if err != nil {
		return err
	}
	if foreground == int32(childPID) {
		return control.restoreForeground()
	}
	return nil
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}
