package cli

import (
	"bytes"
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

func TestRunWithoutDatabaseDirectGHMutationIsGatedByShim(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	runLog := filepath.Join(t.TempDir(), "real-gh-ran")
	fakeDirectory := t.TempDir()
	fakeGH := filepath.Join(fakeDirectory, "gh")
	if err := os.WriteFile(fakeGH, []byte(
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+runLog+"\"\nprintf 'REAL GH RAN\\n'\n",
	), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"gh",
		"issue",
		"create",
		"--title",
		"database-free shim regression",
		"--body",
		"must be gated",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("declined direct gh mutation exit = %v, want nonzero\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"gh action needs approval",
		"No interactive terminal; declining the action",
		"GH INVOCATIONS — MUTATIONS AND AMBIGUOUS COMMANDS",
		"[not-run] gh issue create",
		"NOT INTERCEPTED — no database traffic was intercepted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free direct gh review missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "REAL GH RAN") {
		t.Fatalf("declined direct gh mutation ran the parent-PATH executable:\n%s", text)
	}
	if _, err := os.Stat(runLog); !os.IsNotExist(err) {
		t.Fatalf("declined direct gh mutation invoked real gh: %v", err)
	}
	if strings.Contains(text, "Query batches: 0") {
		t.Fatalf("database-free review implied zero observed PostgreSQL traffic:\n%s", text)
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	for _, want := range []string{
		`"arguments": [`,
		`"issue"`,
		`"create"`,
		`"state": "not-run"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("direct gh JSON audit missing %q:\n%s", want, logText)
		}
	}
}

func TestRunWithoutDatabaseStillInterceptsHTTPS(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--discard",
		"--",
		curl,
		"--silent",
		"--show-error",
		"--max-time",
		"5",
		"--request",
		"POST",
		"--data",
		"must-not-be-sent",
		"https://127.0.0.1:1/unring-no-database",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("database-free HTTPS interception failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"HTTPS action needs approval",
		"No interactive terminal; declining the action",
		"HTTPS APPROVALS — NOT SENT",
		"POST https://127.0.0.1:1/unring-no-database",
		"NOT INTERCEPTED — no database traffic was intercepted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free HTTPS review missing %q:\n%s", want, text)
		}
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
		"direct-to-IP and raw-socket connections",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("git push-only run missing %q:\n%s", want, text)
		}
	}
}

func TestConfiguredQuietSessionPrintsOnlyDisclosure(t *testing.T) {
	connectionString, backendDone := startReviewTestBackend(t, false)
	t.Setenv("DATABASE_URL", connectionString)
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	binary := buildTestBinary(t)
	fakeDirectory := t.TempDir()
	fakeGit := filepath.Join(fakeDirectory, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := exec.Command(binary, "run", "--discard", "--", "git", "push")
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("configured quiet run failed: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("configured quiet run wrote stdout: %q", got)
	}
	if got, want := stderr.String(), quietSessionDisclosure+"\n"; got != want {
		t.Fatalf("configured quiet stderr = %q, want %q", got, want)
	}
	for _, unwanted := range []string{"UNRING SESSION REVIEW", "Commit or discard?", "Up/down:"} {
		if strings.Contains(stderr.String(), unwanted) {
			t.Fatalf("configured quiet disclosure included %q: %q", unwanted, stderr.String())
		}
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestDatabaseFreeStartupFailureRemainsNotStartedInAudit(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("UNRING_ADAPTERS", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--", "/bin/echo", "must-not-run")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != internalErrorExitCode {
		t.Fatalf("startup failure exit = %v, want %d\n%s", err, internalErrorExitCode, output)
	}
	if strings.Contains(string(output), "must-not-run\n") {
		t.Fatalf("startup failure launched child:\n%s", output)
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	if !strings.Contains(logText, `"outcome": "not_started"`) ||
		strings.Contains(logText, `"outcome": "discarded"`) {
		t.Fatalf("pre-child database-free failure has false audit outcome:\n%s", logText)
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
