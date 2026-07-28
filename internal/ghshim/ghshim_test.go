package ghshim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/creack/pty"
	"github.com/hyj28/unring/internal/adapter"
)

func TestReadPassThroughPreservesStdinStreamsAndExitCode(t *testing.T) {
	fake := writeFakeGH(t, `
IFS= read -r input
printf 'stdout:%s:%s\001\002\n' "$*" "$input"
printf 'stderr:%s:%s\003\004\n' "$*" "$input" >&2
exit 23
`)
	session := startTestSession(t, fake, func(context.Context, ApprovalRequest) (bool, error) {
		t.Fatal("confident read unexpectedly requested approval")
		return false, nil
	})
	setClientEnvironment(t, session)

	arguments := []string{"issue", "list", "--repo", "acme/widget", "--limit", "2"}
	input := "stdin-through-shim\n"
	var directOut, directErr bytes.Buffer
	direct := exec.Command(fake, arguments...)
	direct.Stdin = strings.NewReader(input)
	direct.Stdout = &directOut
	direct.Stderr = &directErr
	directCode := commandExitCode(direct.Run())

	var shimOut, shimErr bytes.Buffer
	shimCode := RunClient(arguments, strings.NewReader(input), &shimOut, &shimErr)
	if shimCode != directCode || shimOut.String() != directOut.String() ||
		shimErr.String() != directErr.String() {
		t.Fatalf(
			"shim result differs from real gh:\ncode %d vs %d\nstdout %q vs %q\nstderr %q vs %q",
			shimCode, directCode, shimOut.String(), directOut.String(),
			shimErr.String(), directErr.String(),
		)
	}
	if session.Summary().HasReviewableActivity() {
		t.Fatal("confident read made a read-only session reviewable")
	}
}

func TestDeclinedMutationIsNonzeroAndWritesOnlyStderr(t *testing.T) {
	runLog := filepath.Join(t.TempDir(), "runs")
	fake := writeFakeGH(t, "printf 'ran\\n' >> \""+runLog+"\"\n")
	var approval ApprovalRequest
	session := startTestSession(t, fake,
		func(_ context.Context, request ApprovalRequest) (bool, error) {
			approval = request
			return false, nil
		})
	setClientEnvironment(t, session)

	var stdout, stderr bytes.Buffer
	code := RunClient(
		[]string{"api", "repos/acme/widget/hooks/1", "--method", "DELETE"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Fatal("declined mutation returned success")
	}
	if stdout.Len() != 0 {
		t.Fatalf("declined mutation wrote stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invocation not run") ||
		!strings.Contains(stderr.String(), "gh api uses DELETE") {
		t.Fatalf("declined mutation stderr = %q", stderr.String())
	}
	if _, err := os.Stat(runLog); !os.IsNotExist(err) {
		t.Fatalf("declined mutation invoked real gh: %v", err)
	}
	if !strings.Contains(approval.Reason, "gh api uses DELETE") {
		t.Fatalf("declined mutation reason = %#v", approval)
	}
}

func TestAPIGetPassesThroughButFieldImpliedPostDoesNot(t *testing.T) {
	runLog := filepath.Join(t.TempDir(), "runs")
	fake := writeFakeGH(t, "printf '%s\\n' \"$*\" >> \""+runLog+"\"\n")
	approvals := 0
	session := startTestSession(t, fake,
		func(_ context.Context, request ApprovalRequest) (bool, error) {
			approvals++
			return false, nil
		})
	setClientEnvironment(t, session)

	if code := RunClient(
		[]string{"api", "repos/acme/widget", "--method", "GET"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{},
	); code != 0 {
		t.Fatalf("explicit API GET exit code = %d", code)
	}
	var declinedStderr bytes.Buffer
	if code := RunClient(
		[]string{"api", "repos/acme/widget/hooks", "-f", "name=deploy"},
		strings.NewReader(""), &bytes.Buffer{}, &declinedStderr,
	); code == 0 {
		t.Fatal("field-implied API POST returned success")
	}
	data, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("read fake gh log: %v", err)
	}
	if got := string(data); got != "api repos/acme/widget --method GET\n" {
		t.Fatalf("real gh invocations = %q", got)
	}
	if approvals != 1 || !strings.Contains(declinedStderr.String(), "-f supplies request data") {
		t.Fatalf("field-implied POST approvals=%d stderr=%q", approvals, declinedStderr.String())
	}
}

func TestReadPassThroughPreservesTTY(t *testing.T) {
	fake := writeFakeGH(t, `
if [ ! -t 0 ] || [ ! -t 1 ] || [ ! -t 2 ]; then
  printf 'real gh did not inherit a TTY\n' >&2
  exit 90
fi
IFS= read -r input
printf 'tty-stdin:%s\n' "$input"
exit 37
`)
	session := startTestSession(t, fake, nil)
	command := exec.Command(os.Args[0], "-test.run=^TestRunClientTTYHelper$")
	command.Env = append(
		session.Environment(os.Environ()),
		"UNRING_GH_TTY_HELPER=1",
	)
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start gh shim helper under PTY: %v", err)
	}
	if _, err := terminal.Write([]byte("tty-input\n")); err != nil {
		_ = terminal.Close()
		t.Fatalf("write PTY input: %v", err)
	}
	output, readErr := io.ReadAll(terminal)
	_ = terminal.Close()
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		t.Fatalf("read PTY output: %v", readErr)
	}
	err = command.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 37 {
		t.Fatalf("TTY pass-through exit = %v, output=%q", err, output)
	}
	if !bytes.Contains(output, []byte("tty-stdin:tty-input")) {
		t.Fatalf("TTY pass-through output = %q", output)
	}
}

func TestRunClientTTYHelper(t *testing.T) {
	if os.Getenv("UNRING_GH_TTY_HELPER") != "1" {
		return
	}
	os.Exit(RunClient(
		[]string{"issue", "list"},
		os.Stdin, os.Stdout, os.Stderr,
	))
}

func TestInstalledGHReadsPassThroughIdentically(t *testing.T) {
	realGH, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh is not installed: %v", err)
	}
	sources, err := adapter.BuiltinSources()
	if err != nil {
		t.Fatalf("load built-in adapter sources: %v", err)
	}
	adapters, err := adapter.Load(sources...)
	if err != nil {
		t.Fatalf("load built-in adapters: %v", err)
	}
	session, err := Start(Options{Adapters: adapters, Executable: os.Args[0]})
	if err != nil {
		t.Fatalf("start gh shim: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	setClientEnvironment(t, session)

	for _, arguments := range [][]string{
		{"--version"},
		{"--help"},
		{"issue", "list", "--help"},
	} {
		var directOut, directErr bytes.Buffer
		direct := exec.Command(realGH, arguments...)
		direct.Stdout = &directOut
		direct.Stderr = &directErr
		directCode := commandExitCode(direct.Run())

		var shimOut, shimErr bytes.Buffer
		shimCode := RunClient(arguments, strings.NewReader(""), &shimOut, &shimErr)
		if shimCode != directCode || shimOut.String() != directOut.String() ||
			shimErr.String() != directErr.String() {
			t.Fatalf(
				"installed gh %q differs through shim:\ncode %d vs %d\nstdout %q vs %q\nstderr %q vs %q",
				arguments, shimCode, directCode, shimOut.String(), directOut.String(),
				shimErr.String(), directErr.String(),
			)
		}
	}
}

func TestIssueCreateNeedsApprovalAndReturnsOnlyRealResult(t *testing.T) {
	for _, test := range []struct {
		name       string
		approved   bool
		wantCode   int
		wantStdout string
		wantRuns   string
	}{
		{name: "declined", approved: false, wantCode: 1},
		{
			name: "approved", approved: true, wantCode: 0,
			wantStdout: "https://github.com/acme/widget/issues/123\n",
			wantRuns:   "create\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runLog := filepath.Join(t.TempDir(), "runs")
			fake := writeFakeGH(t,
				"printf 'create\\n' >> \""+runLog+"\"\n"+
					"printf 'https://github.com/acme/widget/issues/123\\n'\n")
			session := startTestSession(t, fake,
				func(context.Context, ApprovalRequest) (bool, error) {
					return test.approved, nil
				})
			setClientEnvironment(t, session)
			var shimOut bytes.Buffer
			var shimErr bytes.Buffer
			code := RunClient(
				[]string{"issue", "create", "--repo", "acme/widget", "--title", "shim issue"},
				strings.NewReader(""), &shimOut, &shimErr,
			)
			if code != test.wantCode || shimOut.String() != test.wantStdout {
				t.Fatalf("create result: code=%d stdout=%q stderr=%q",
					code, shimOut.String(), shimErr.String())
			}
			if !test.approved && (!strings.Contains(shimErr.String(), "cannot be staged honestly") ||
				shimOut.Len() != 0) {
				t.Fatalf("declined create contract: stdout=%q stderr=%q",
					shimOut.String(), shimErr.String())
			}
			if err := session.Finalize(context.Background(), true); err != nil {
				t.Fatalf("finalize gh session: %v", err)
			}
			data, err := os.ReadFile(runLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read fake gh run log: %v", err)
			}
			if string(data) != test.wantRuns {
				t.Fatalf("fake gh runs = %q, want %q", data, test.wantRuns)
			}
			records := session.Summary().Records
			if len(records) != 1 || records[0].Decision != test.name ||
				(test.approved && records[0].ResourceURL != strings.TrimSpace(test.wantStdout)) {
				t.Fatalf("gh summary = %#v", records)
			}
		})
	}
}

func TestApprovedIssueCreateIsClosedOnDiscard(t *testing.T) {
	runLog := filepath.Join(t.TempDir(), "runs")
	fake := writeFakeGH(t, `
if [ "$1 $2" = "issue close" ]; then
  printf 'close:%s\n' "$3" >> "`+runLog+`"
  exit 0
fi
printf 'create\n' >> "`+runLog+`"
printf 'https://github.com/acme/widget/issues/123\n'
`)
	session := startTestSession(t, fake,
		func(context.Context, ApprovalRequest) (bool, error) { return true, nil })
	setClientEnvironment(t, session)
	var stdout bytes.Buffer
	if code := RunClient(
		[]string{"issue", "create", "--repo", "acme/widget", "--title", "undo me"},
		strings.NewReader(""), &stdout, &bytes.Buffer{},
	); code != 0 {
		t.Fatalf("approved create exit code = %d", code)
	}
	if err := session.Finalize(context.Background(), false); err != nil {
		t.Fatalf("discard compensation: %v", err)
	}
	data, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("read fake gh log: %v", err)
	}
	want := "create\nclose:https://github.com/acme/widget/issues/123\n"
	if string(data) != want {
		t.Fatalf("gh create/compensation log = %q, want %q", data, want)
	}
	record := session.Summary().Records[0]
	if record.UndoState != "succeeded" {
		t.Fatalf("gh compensation record = %#v", record)
	}
}

func TestUnknownSubcommandAndFlagNeedApprovalWithReason(t *testing.T) {
	for _, arguments := range [][]string{
		{"mystery", "do-it"},
		{"issue", "list", "--mystery"},
	} {
		var approval ApprovalRequest
		fake := writeFakeGH(t, "exit 99\n")
		session := startTestSession(t, fake,
			func(_ context.Context, request ApprovalRequest) (bool, error) {
				approval = request
				return false, nil
			})
		setClientEnvironment(t, session)
		if code := RunClient(arguments, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
			t.Fatalf("unknown invocation %v passed through", arguments)
		}
		if !strings.Contains(approval.Reason, "unrecognized") ||
			!strings.Contains(approval.Reason, "will not guess") {
			t.Fatalf("unknown invocation reason = %q", approval.Reason)
		}
	}
}

func TestRealGHCannotReenterShim(t *testing.T) {
	fake := writeFakeGH(t, `
if [ -n "$UNRING_GH_SHIM" ]; then
  printf 'shim marker leaked\n' >&2
  exit 91
fi
case "$PATH" in
  *unring-gh-*) printf 'shim path leaked\n' >&2; exit 92 ;;
esac
printf 'real-gh-once\n'
`)
	session := startTestSession(t, fake, nil)
	setClientEnvironment(t, session)
	var output bytes.Buffer
	if code := RunClient(
		[]string{"issue", "list", "--repo", "acme/widget"},
		strings.NewReader(""), &output, &bytes.Buffer{},
	); code != 0 {
		t.Fatalf("read exit code = %d", code)
	}
	if output.String() != "real-gh-once\n" {
		t.Fatalf("real gh output = %q", output.String())
	}
}

func startTestSession(
	t *testing.T,
	realGH string,
	approve func(context.Context, ApprovalRequest) (bool, error),
) *Session {
	t.Helper()
	t.Setenv("PATH", filepath.Dir(realGH))
	sources, err := adapter.BuiltinSources()
	if err != nil {
		t.Fatalf("load built-in adapter sources: %v", err)
	}
	adapters, err := adapter.Load(sources...)
	if err != nil {
		t.Fatalf("load built-in adapters: %v", err)
	}
	session, err := Start(Options{Adapters: adapters, Approve: approve, Executable: os.Args[0]})
	if err != nil {
		t.Fatalf("start gh shim: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func setClientEnvironment(t *testing.T, session *Session) {
	t.Helper()
	t.Setenv(envMarker, "1")
	t.Setenv(envSocket, session.socket)
	t.Setenv(envToken, session.token)
	t.Setenv(envRealGH, session.realGH)
	t.Setenv(envOriginalPath, session.originalPath)
	t.Setenv("PATH", session.directory+string(os.PathListSeparator)+session.originalPath)
}

func writeFakeGH(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return path
}
