// Package runner starts and supervises the command wrapped by unring.
package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Options controls one child process.
type Options struct {
	Command   []string
	Env       []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	Signals   <-chan os.Signal
	Abort     <-chan struct{}
	Approvals <-chan ApprovalRequest
}

// ApprovalRequest lets the supervisor temporarily reclaim an interactive
// terminal while a child is blocked waiting for an unring decision.
type ApprovalRequest struct {
	Decide func() (bool, error)
	Reply  chan<- ApprovalResult
}

// ApprovalResult is returned after the child has regained its terminal.
type ApprovalResult struct {
	Approved bool
	Err      error
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

	commandPath, err := lookPathInEnvironment(options.Command[0], options.Env)
	if err != nil {
		return Result{
			ExitCode: 127,
			Err:      fmt.Errorf("start child %q: %w", options.Command[0], err),
		}
	}
	command := exec.Command(commandPath, options.Command[1:]...)
	command.Args[0] = options.Command[0]
	command.Env = options.Env
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	processGroup := configureProcessGroup(command, options.Stdin)
	childChanges, stopWatchingChild := processGroup.watchChildChanges()
	defer stopWatchingChild()

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
	approvals := options.Approvals
	allApprovals := approvals
	var forceKill <-chan time.Time
	var interrupted bool
	var sessionLost bool
	var supervisionErr error
	type completedApproval struct {
		id     uint64
		result ApprovalResult
	}
	approvalResults := make(chan completedApproval, 1)
	var approvalID uint64
	var activeApproval *ApprovalRequest

	for {
		select {
		case err := <-waited:
			if activeApproval != nil {
				activeApproval.Reply <- ApprovalResult{
					Err: errors.New("child exited while irreversible-statement approval was pending"),
				}
				activeApproval = nil
			}
			exitCode := processExitCode(command.ProcessState, err)
			interrupted = interrupted || processWasSignaled(command.ProcessState)
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
		case <-childChanges:
			// handleApproval deliberately stops the child before reclaiming the
			// terminal. Do not let the job-control watcher consume that stop
			// notification and mistake it for a user-initiated Ctrl-Z.
			if activeApproval != nil {
				continue
			}
			stopped, err := childStopped(command.Process.Pid)
			if err != nil && !errors.Is(err, syscall.ECHILD) {
				supervisionErr = errors.Join(
					supervisionErr,
					fmt.Errorf("inspect child job-control state: %w", err),
				)
				continue
			}
			if stopped {
				interrupted = true
				if err := processGroup.reclaimStoppedChild(command.Process.Pid); err != nil {
					supervisionErr = errors.Join(
						supervisionErr,
						fmt.Errorf("reclaim terminal from stopped child: %w", err),
					)
				}
				// A stopped process cannot perform graceful cleanup, and
				// continuing it in the background can immediately stop it again
				// on SIGTTIN. Kill the stopped group after reclaiming the TTY;
				// the CLI will discard the database transaction.
				if err := signalProcessGroup(command.Process.Pid, syscall.SIGKILL); err != nil &&
					!errors.Is(err, syscall.ESRCH) {
					supervisionErr = errors.Join(
						supervisionErr,
						fmt.Errorf("kill stopped child: %w", err),
					)
				}
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
			if activeApproval != nil {
				_ = processGroup.cancelApproval(command.Process.Pid)
				activeApproval.Reply <- ApprovalResult{
					Err: fmt.Errorf("approval interrupted by %s", signal),
				}
				activeApproval = nil
				approvals = allApprovals
				approvalID++
			}
		case <-abort:
			sessionLost = true
			abort = nil
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
			forceKill = time.After(2 * time.Second)
		case request, ok := <-approvals:
			if !ok {
				approvals = nil
				continue
			}
			activeApproval = &request
			approvals = nil
			approvalID++
			currentID := approvalID
			go func() {
				approvalResults <- completedApproval{
					id:     currentID,
					result: processGroup.handleApproval(command.Process.Pid, request.Decide),
				}
			}()
		case completed := <-approvalResults:
			if activeApproval == nil || completed.id != approvalID {
				continue
			}
			activeApproval.Reply <- completed.result
			activeApproval = nil
			approvals = allApprovals
		case <-forceKill:
			forceKill = nil
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
		}
	}
}

// lookPathInEnvironment resolves the directly wrapped command through the
// child's PATH, including per-session shims. exec.Command cannot do this for
// us because it searches the parent process's PATH before Cmd.Env is assigned.
func lookPathInEnvironment(command string, environment []string) (string, error) {
	if strings.ContainsRune(command, os.PathSeparator) {
		return command, nil
	}
	path := ""
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == "PATH" {
			path = value
		}
	}
	if environment == nil {
		path = os.Getenv("PATH")
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, command)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if filepath.IsAbs(candidate) {
			return candidate, nil
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", &exec.Error{Name: command, Err: err}
		}
		return absolute, nil
	}
	return "", &exec.Error{Name: command, Err: exec.ErrNotFound}
}

func processWasSignaled(state *os.ProcessState) bool {
	if state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
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
