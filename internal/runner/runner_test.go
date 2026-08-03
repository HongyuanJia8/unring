package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestRunPropagatesExitCode(t *testing.T) {
	t.Parallel()

	result := Run(Options{
		Command: []string{"/bin/sh", "-c", "exit 37"},
		Env:     os.Environ(),
		Stdin:   nil,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if result.Err != nil {
		t.Fatalf("Run() error: %v", result.Err)
	}
	if result.ExitCode != 37 {
		t.Fatalf("Run() exit code = %d, want 37", result.ExitCode)
	}
}

func TestRunResolvesDirectCommandThroughChildEnvironment(t *testing.T) {
	parentDirectory := t.TempDir()
	childDirectory := t.TempDir()
	commandName := "unring-runner-path-test"
	parentCommand := filepath.Join(parentDirectory, commandName)
	childCommand := filepath.Join(childDirectory, commandName)
	if err := os.WriteFile(parentCommand, []byte("#!/bin/sh\nprintf parent\n"), 0o755); err != nil {
		t.Fatalf("write parent-PATH command: %v", err)
	}
	if err := os.WriteFile(childCommand, []byte("#!/bin/sh\nprintf child\n"), 0o755); err != nil {
		t.Fatalf("write child-PATH command: %v", err)
	}
	t.Setenv("PATH", parentDirectory)
	environment := append([]string(nil), os.Environ()...)
	for index, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			environment[index] = "PATH=" + childDirectory
		}
	}

	var stdout strings.Builder
	result := Run(Options{
		Command: []string{commandName},
		Env:     environment,
		Stdout:  &stdout,
		Stderr:  io.Discard,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
	if got := stdout.String(); got != "child" {
		t.Fatalf("direct command output = %q, want child-PATH executable", got)
	}
}

func TestRunSkipsPathCandidateCurrentUserCannotExecute(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can execute a file when any execute bit is set")
	}
	firstDirectory := t.TempDir()
	secondDirectory := t.TempDir()
	commandName := "unring-runner-access-test"
	firstCommand := filepath.Join(firstDirectory, commandName)
	secondCommand := filepath.Join(secondDirectory, commandName)
	if err := os.WriteFile(firstCommand, []byte("#!/bin/sh\nprintf first\n"), 0o010); err != nil {
		t.Fatalf("write inaccessible PATH command: %v", err)
	}
	if err := os.WriteFile(secondCommand, []byte("#!/bin/sh\nprintf second\n"), 0o755); err != nil {
		t.Fatalf("write executable PATH command: %v", err)
	}

	var stdout strings.Builder
	result := Run(Options{
		Command: []string{commandName},
		Env: []string{
			"PATH=" + firstDirectory + string(os.PathListSeparator) + secondDirectory,
		},
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
	if got := stdout.String(); got != "second" {
		t.Fatalf("direct command output = %q, want accessible PATH executable", got)
	}
}

func TestRunUsesParentPathWhenChildEnvironmentOmitsPath(t *testing.T) {
	parentDirectory := t.TempDir()
	commandName := "unring-runner-parent-path-fallback-test"
	parentCommand := filepath.Join(parentDirectory, commandName)
	if err := os.WriteFile(parentCommand, []byte("#!/bin/sh\nprintf parent\n"), 0o755); err != nil {
		t.Fatalf("write parent-PATH command: %v", err)
	}
	t.Setenv("PATH", parentDirectory)

	var stdout strings.Builder
	result := Run(Options{
		Command: []string{commandName},
		Env:     []string{"UNRING_RUNNER_TEST=1"},
		Stdout:  &stdout,
		Stderr:  io.Discard,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
	if got := stdout.String(); got != "parent" {
		t.Fatalf("direct command output = %q, want parent-PATH executable", got)
	}
}

func TestRunForwardsSignalToChildProcessGroup(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	result := Run(Options{
		Command: []string{"/bin/sh", "-c", "trap 'exit 42' TERM; while :; do sleep 1; done"},
		Env:     os.Environ(),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Signals: signals,
	})
	if result.Err != nil {
		t.Fatalf("Run() error: %v", result.Err)
	}
	if !result.Interrupted {
		t.Fatal("Run() did not report the forwarded signal")
	}
	if result.ExitCode != 42 {
		t.Fatalf("Run() exit code = %d, want trapped exit code 42", result.ExitCode)
	}
}

func TestRunTreatsSignaledChildAsInterrupted(t *testing.T) {
	t.Parallel()

	result := Run(Options{
		Command: []string{"/bin/sh", "-c", "kill -INT $$"},
		Env:     os.Environ(),
		Stdin:   nil,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if result.Err != nil {
		t.Fatalf("Run() error: %v", result.Err)
	}
	if !result.Interrupted {
		t.Fatal("Run() did not treat a signal-terminated child as interrupted")
	}
	if result.ExitCode != 128+int(syscall.SIGINT) {
		t.Fatalf("Run() exit code = %d, want %d",
			result.ExitCode, 128+int(syscall.SIGINT))
	}
}

func TestRunServicesApprovalWhileChildIsActive(t *testing.T) {
	t.Parallel()

	requests := make(chan ApprovalRequest)
	approvalDone := make(chan ApprovalResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		reply := make(chan ApprovalResult, 1)
		requests <- ApprovalRequest{
			Decide: func() (bool, error) { return true, nil },
			Reply:  reply,
		}
		approvalDone <- <-reply
	}()
	result := Run(Options{
		Command:   []string{"/bin/sh", "-c", "sleep 0.2"},
		Env:       os.Environ(),
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		Approvals: requests,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
	approval := <-approvalDone
	if !approval.Approved || approval.Err != nil {
		t.Fatalf("approval result = %#v", approval)
	}
}

func TestRunForwardsSignalWhileApprovalPromptIsBlocked(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	requests := make(chan ApprovalRequest)
	decisionStarted := make(chan struct{})
	approvalDone := make(chan ApprovalResult, 1)
	runDone := make(chan Result, 1)
	go func() {
		runDone <- Run(Options{
			Command: []string{"/bin/sh", "-c",
				"trap 'exit 42' TERM; while :; do sleep 1; done"},
			Env:       os.Environ(),
			Stdout:    io.Discard,
			Stderr:    io.Discard,
			Signals:   signals,
			Approvals: requests,
		})
	}()
	go func() {
		reply := make(chan ApprovalResult, 1)
		requests <- ApprovalRequest{
			Decide: func() (bool, error) {
				close(decisionStarted)
				select {}
			},
			Reply: reply,
		}
		approvalDone <- <-reply
	}()

	select {
	case <-decisionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("approval decision did not start")
	}
	signals <- syscall.SIGTERM

	select {
	case result := <-runDone:
		if !result.Interrupted ||
			(result.ExitCode != 42 && result.ExitCode != 128+int(syscall.SIGTERM)) {
			t.Fatalf("Run() after signal during approval = %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() swallowed SIGTERM while approval was blocked")
	}
	select {
	case approval := <-approvalDone:
		if approval.Err == nil || approval.Approved {
			t.Fatalf("interrupted approval = %#v, want declined error", approval)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted approval requester was not released")
	}
}

func TestChildStoppedObservesStopWithoutReapingExit(t *testing.T) {
	t.Parallel()

	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
	})
	if err := signalProcessGroup(command.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop child: %v", err)
	}

	var stopped bool
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for !stopped && time.Now().Before(deadline) {
		stopped, err = childStopped(command.Process.Pid)
		if err != nil {
			t.Fatalf("childStopped(): %v", err)
		}
		if !stopped {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !stopped {
		t.Fatal("childStopped() did not observe SIGSTOP")
	}
	if err := signalProcessGroup(command.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatalf("continue child: %v", err)
	}
	if err := signalProcessGroup(command.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("terminate child: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("signal-terminated child unexpectedly exited successfully")
	}
}

func TestRunInteractiveChildUsesForegroundTTY(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRunInteractiveChildHelper$")
	command.Env = append(os.Environ(), "UNRING_RUNNER_PTY_HELPER=1")

	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start runner helper under PTY: %v", err)
	}
	defer terminal.Close()

	if _, err := terminal.Write([]byte("hello-terminal\nparent-terminal\n")); err != nil {
		t.Fatalf("write interactive input: %v", err)
	}

	type readResult struct {
		output []byte
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		output, err := io.ReadAll(terminal)
		readDone <- readResult{output: output, err: err}
	}()

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(10 * time.Second):
		_ = terminal.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatal("interactive child timed out, likely waiting in a background process group")
	}
	output, readErr := result.output, result.err
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf("read interactive output: %v\n%s", readErr, output)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("interactive helper failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "child-read:hello-terminal") {
		t.Fatalf("interactive child did not read from its foreground TTY:\n%s", output)
	}
	if !strings.Contains(string(output), "parent-read:parent-terminal") {
		t.Fatalf("runner did not regain the TTY after its child exited:\n%s", output)
	}
}

func TestRunInteractiveChildHandlesTerminalStop(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRunInteractiveChildHelper$")
	command.Env = append(os.Environ(), "UNRING_RUNNER_PTY_HELPER=job-control")

	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start runner helper under PTY: %v", err)
	}
	defer terminal.Close()

	reader := bufio.NewReader(terminal)
	var initialOutput strings.Builder
	ready := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			initialOutput.WriteString(line)
			if strings.Contains(line, "child-ready") {
				ready <- nil
				return
			}
			if err != nil {
				ready <- err
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("wait for interactive child readiness: %v\n%s", err, initialOutput.String())
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf("interactive child did not become ready:\n%s", initialOutput.String())
	}

	if _, err := terminal.Write([]byte{0x1a}); err != nil {
		t.Fatalf("send terminal Ctrl-Z to foreground child: %v", err)
	}

	interrupted := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			initialOutput.WriteString(line)
			if strings.Contains(line, "wrapper-interrupted") {
				interrupted <- nil
				return
			}
			if err != nil {
				interrupted <- err
				return
			}
		}
	}()
	select {
	case err := <-interrupted:
		if err != nil {
			t.Fatalf("wait for stopped child interruption: %v\n%s",
				err, initialOutput.String())
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf("runner did not abort after its foreground child stopped:\n%s",
			initialOutput.String())
	}

	if _, err := terminal.Write([]byte("parent-after-stop\n")); err != nil {
		t.Fatalf("write input after stopping interactive child: %v", err)
	}

	type readResult struct {
		output string
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		remaining, err := io.ReadAll(reader)
		readDone <- readResult{output: initialOutput.String() + string(remaining), err: err}
	}()

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatal("interactive job did not abort after its foreground child stopped")
	}
	if result.err != nil && !errors.Is(result.err, syscall.EIO) {
		t.Fatalf("read stopped interactive output: %v\n%s", result.err, result.output)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("stopped interactive helper failed: %v\n%s", err, result.output)
	}
	if !strings.Contains(result.output, "wrapper-interrupted") {
		t.Fatalf("runner did not treat Ctrl-Z as an interruption:\n%s", result.output)
	}
	if !strings.Contains(result.output, "parent-read:parent-after-stop") {
		t.Fatalf("runner did not reclaim a readable foreground TTY:\n%s", result.output)
	}
}

func TestApprovalOwnsTTYAndRestoresRawChild(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRunInteractiveChildHelper$")
	command.Env = append(os.Environ(), "UNRING_RUNNER_PTY_HELPER=approval")
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start approval helper under PTY: %v", err)
	}
	defer terminal.Close()
	session := bufio.NewReader(terminal)
	var output strings.Builder
	waitFor := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(output.String(), marker) && time.Now().Before(deadline) {
			line, readErr := session.ReadString('\n')
			output.WriteString(line)
			if readErr != nil {
				t.Fatalf("wait for %q: %v\n%s", marker, readErr, output.String())
			}
		}
		if !strings.Contains(output.String(), marker) {
			t.Fatalf("timed out waiting for %q:\n%s", marker, output.String())
		}
	}
	waitFor("raw-child-ready")
	if _, err := terminal.Write([]byte("x")); err != nil {
		t.Fatalf("queue child input before approval: %v", err)
	}
	waitFor("approval-prompt")
	if _, err := terminal.Write([]byte("y\n")); err != nil {
		t.Fatalf("answer approval: %v", err)
	}
	waitFor("approval-accepted")
	if _, err := terminal.Write([]byte("z")); err != nil {
		t.Fatalf("write child input after approval: %v", err)
	}

	rest, readErr := io.ReadAll(session)
	output.Write(rest)
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		t.Fatalf("read approval helper: %v\n%s", readErr, output.String())
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("approval helper failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "raw-child-read:z") ||
		strings.Contains(output.String(), "raw-child-read:x") ||
		strings.Contains(output.String(), "raw-child-read:y") {
		t.Fatalf("approval and child did not receive isolated input:\n%s", output.String())
	}
}

func TestRunInteractiveChildHelper(t *testing.T) {
	mode := os.Getenv("UNRING_RUNNER_PTY_HELPER")
	if mode != "1" && mode != "job-control" && mode != "approval" {
		return
	}

	childCommand := []string{
		"/bin/sh",
		"-c",
		`IFS= read -r line; printf 'child-read:%s\n' "$line"`,
	}
	childEnvironment := os.Environ()
	if mode == "job-control" {
		childCommand = []string{
			os.Args[0],
			"-test.run=^TestRunForegroundChildProcess$",
		}
		childEnvironment = append(childEnvironment, "UNRING_RUNNER_FOREGROUND_CHILD=1")
	} else if mode == "approval" {
		childCommand = []string{
			os.Args[0],
			"-test.run=^TestRunRawForegroundChildProcess$",
		}
		childEnvironment = append(childEnvironment, "UNRING_RUNNER_RAW_CHILD=1")
	}
	var approvals chan ApprovalRequest
	if mode == "approval" {
		approvals = make(chan ApprovalRequest)
		go func() {
			time.Sleep(300 * time.Millisecond)
			reply := make(chan ApprovalResult, 1)
			approvals <- ApprovalRequest{
				Decide: func() (bool, error) {
					fmt.Println("approval-prompt")
					line, err := bufio.NewReader(os.Stdin).ReadString('\n')
					approved := strings.TrimSpace(line) == "y"
					if approved {
						fmt.Println("approval-accepted")
					}
					return approved, err
				},
				Reply: reply,
			}
			result := <-reply
			if !result.Approved || result.Err != nil {
				fmt.Fprintf(os.Stderr, "approval failed: %#v\n", result)
			}
		}()
	}
	result := Run(Options{
		Command:   childCommand,
		Env:       childEnvironment,
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Approvals: approvals,
	})
	if result.Err != nil {
		fmt.Fprintf(os.Stderr, "interactive runner failed: exit=%d err=%v\n",
			result.ExitCode, result.Err)
		os.Exit(1)
	}
	if mode == "job-control" {
		if !result.Interrupted || result.ExitCode != 128+int(syscall.SIGKILL) {
			fmt.Fprintf(os.Stderr,
				"stopped child result: exit=%d interrupted=%v, want exit=%d interrupted\n",
				result.ExitCode,
				result.Interrupted,
				128+int(syscall.SIGKILL),
			)
			os.Exit(1)
		}
		fmt.Println("wrapper-interrupted")
	} else if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "interactive child exit=%d, want 0\n", result.ExitCode)
		os.Exit(1)
	}
	if mode == "approval" {
		fmt.Println("approval-wrapper-done")
		return
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "parent failed to read restored TTY: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("parent-read:%s", line)
}

func TestRunRawForegroundChildProcess(t *testing.T) {
	if os.Getenv("UNRING_RUNNER_RAW_CHILD") != "1" {
		return
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "make child terminal raw: %v\n", err)
		os.Exit(1)
	}
	defer term.Restore(int(os.Stdin.Fd()), state)
	fmt.Println("raw-child-ready")
	time.Sleep(time.Second)
	var input [1]byte
	if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
		fmt.Fprintf(os.Stderr, "raw child read: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("raw-child-read:%c\n", input[0])
	os.Exit(0)
}

func TestRunForegroundChildProcess(t *testing.T) {
	if os.Getenv("UNRING_RUNNER_FOREGROUND_CHILD") != "1" {
		return
	}

	fmt.Printf("child-ready:%d\n", os.Getpid())
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "foreground child failed to read TTY: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("child-read:%s", line)
	os.Exit(0)
}
