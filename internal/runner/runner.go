// Package runner starts and supervises the command wrapped by unring.
package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Options controls one child process.
type Options struct {
	Command []string
	Env     []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Signals <-chan os.Signal
	Abort   <-chan struct{}
}

// Result describes how the child ended.
type Result struct {
	ExitCode    int
	Interrupted bool
	SessionLost bool
	Err         error
}

// Run starts the child, forwards signals to its process group, and waits.
func Run(options Options) Result {
	if len(options.Command) == 0 {
		return Result{ExitCode: 127, Err: errors.New("run child: empty command")}
	}

	command := exec.Command(options.Command[0], options.Command[1:]...)
	command.Env = options.Env
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	processGroup := configureProcessGroup(command, options.Stdin)

	if err := command.Start(); err != nil {
		restoreErr := processGroup.restoreForeground()
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore terminal foreground: %w", restoreErr))
		}
		return Result{
			ExitCode: 127,
			Err:      fmt.Errorf("start child %q: %w", options.Command[0], err),
		}
	}

	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()

	signals := options.Signals
	abort := options.Abort
	var forceKill <-chan time.Time
	var interrupted bool
	var sessionLost bool
	var supervisionErr error

	for {
		select {
		case err := <-waited:
			exitCode := processExitCode(command.ProcessState, err)
			if restoreErr := processGroup.restoreForeground(); restoreErr != nil {
				supervisionErr = errors.Join(
					supervisionErr,
					fmt.Errorf("restore terminal foreground: %w", restoreErr),
				)
				if exitCode == 0 {
					exitCode = 1
				}
			}
			return Result{
				ExitCode:    exitCode,
				Interrupted: interrupted,
				SessionLost: sessionLost,
				Err:         errors.Join(childWaitError(err), supervisionErr),
			}
		case signal, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			interrupted = true
			if err := signalProcessGroup(command.Process.Pid, signal); err != nil &&
				!errors.Is(err, os.ErrProcessDone) &&
				!errors.Is(err, syscall.ESRCH) {
				supervisionErr = errors.Join(
					supervisionErr,
					fmt.Errorf("forward signal to child: %w", err),
				)
				signals = nil
				abort = nil
				_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
			}
		case <-abort:
			sessionLost = true
			abort = nil
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
			forceKill = time.After(2 * time.Second)
		case <-forceKill:
			forceKill = nil
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
		}
	}
}

func childWaitError(err error) error {
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil
	}
	return fmt.Errorf("wait for child: %w", err)
}

func processExitCode(state *os.ProcessState, waitErr error) int {
	if state == nil {
		if waitErr == nil {
			return 0
		}
		return 1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
