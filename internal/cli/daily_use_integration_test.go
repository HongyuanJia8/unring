package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWithoutDatabaseRunsChildPropagatesExitAndRecordsCoverage(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--",
		"/bin/sh",
		"-c",
		"printf 'child-ran-without-database\\n'; exit 23",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
		t.Fatalf("database-free run exit = %v, want 23\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"child-ran-without-database",
		"UNRING SESSION REVIEW",
		"NOT INTERCEPTED — no database traffic was intercepted",
		"not evidence that the child did not access a database",
		"git push over SSH",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free review missing %q:\n%s", want, text)
		}
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	for _, want := range []string{
		`"interception_status": "not_configured"`,
		`"exit_code": 23`,
		`"structural_blind_spots"`,
		`git push over SSH`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("database-free JSON audit missing %q:\n%s", want, logText)
		}
	}
}

func TestRunWithoutDatabaseStillUsesGHShim(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	fakeDirectory := t.TempDir()
	fakeGH := filepath.Join(fakeDirectory, "gh")
	if err := os.WriteFile(fakeGH, []byte(
		"#!/bin/sh\nprintf 'database-free-gh-shim:%s\\n' \"$*\"\n",
	), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--", "gh", "--version")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("database-free gh run failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "database-free-gh-shim:--version") ||
		!strings.Contains(text, "NOT INTERCEPTED — no database traffic was intercepted") {
		t.Fatalf("database-free run did not retain gh interception and coverage review:\n%s", text)
	}
}

func TestGitPushOnlyRunGetsStructuralBlindSpotDisclosure(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	binary := buildTestBinary(t)
	fakeDirectory := t.TempDir()
	fakeGit := filepath.Join(fakeDirectory, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	command := exec.Command(binary, "run", "--", "git", "push")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git push-only run failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
		"direct-to-IP or raw-socket connections",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("git push-only run missing %q:\n%s", want, text)
		}
	}
}

func TestConfiguredSessionReviewAlwaysDisclosesStructuralBlindSpots(t *testing.T) {
	connectionString, backendDone := startReviewTestBackend(t, true)
	t.Setenv("DATABASE_URL", connectionString)
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--discard", "--", "true")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("configured review run failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
		"unshimmed Go CLIs such as aws, docker, terraform, and kubectl on macOS",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("configured review missing %q:\n%s", want, text)
		}
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestBuiltBinaryHelpLeadsWithBoundedTaskWorkflow(t *testing.T) {
	binary := buildTestBinary(t)
	command := exec.Command(binary, "--help")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("unring --help failed: %v\n%s", err, output)
	}
	text := string(output)
	primary := strings.Index(text, "Primary workflow (one bounded agent task that exits)")
	interactiveAlias := strings.Index(text, "unring claude|codex|opencode")
	if primary < 0 || interactiveAlias < 0 || primary > interactiveAlias {
		t.Fatalf("help did not present bounded work before interactive aliases:\n%s", text)
	}
	for _, want := range []string{
		"the shared transaction remains open for the",
		"whole child lifetime, holding locks and delaying cleanup",
		"DATABASE_URL may be unset",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help missing %q:\n%s", want, text)
		}
	}
}
