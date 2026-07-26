package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
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

func TestRunInteractiveChildHelper(t *testing.T) {
	if os.Getenv("UNRING_RUNNER_PTY_HELPER") != "1" {
		return
	}

	result := Run(Options{
		Command: []string{
			"/bin/sh",
			"-c",
			`IFS= read -r line; printf 'child-read:%s\n' "$line"`,
		},
		Env:    os.Environ(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if result.Err != nil || result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "interactive runner failed: exit=%d err=%v\n",
			result.ExitCode, result.Err)
		os.Exit(1)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "parent failed to read restored TTY: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("parent-read:%s", line)
}
