package ghshim

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyj28/unring/internal/adapter"
)

func TestIssueListPassesThroughWithIdenticalStreamsAndExitCode(t *testing.T) {
	fake := writeFakeGH(t, `
printf 'stdout:%s\n' "$*"
printf 'stderr:%s\n' "$*" >&2
exit 23
`)
	session := startTestSession(t, fake, func(context.Context, ApprovalRequest) (bool, error) {
		t.Fatal("confident read unexpectedly requested approval")
		return false, nil
	})
	setClientEnvironment(t, session)

	arguments := []string{"issue", "list", "--repo", "acme/widget", "--limit", "2"}
	var directOut, directErr bytes.Buffer
	direct := exec.Command(fake, arguments...)
	direct.Stdout = &directOut
	direct.Stderr = &directErr
	directCode := commandExitCode(direct.Run())

	var shimOut, shimErr bytes.Buffer
	shimCode := RunClient(arguments, strings.NewReader(""), &shimOut, &shimErr)
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

func TestInstalledGHIssueListPassesThroughIdentically(t *testing.T) {
	realGH, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh is not installed: %v", err)
	}
	arguments := []string{"issue", "list", "--help"}
	var directOut, directErr bytes.Buffer
	direct := exec.Command(realGH, arguments...)
	direct.Stdout = &directOut
	direct.Stderr = &directErr
	directCode := commandExitCode(direct.Run())

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
	var shimOut, shimErr bytes.Buffer
	shimCode := RunClient(arguments, strings.NewReader(""), &shimOut, &shimErr)
	if shimCode != directCode || shimOut.String() != directOut.String() ||
		shimErr.String() != directErr.String() {
		t.Fatalf(
			"installed gh differs through shim:\ncode %d vs %d\nstdout %q vs %q\nstderr %q vs %q",
			shimCode, directCode, shimOut.String(), directOut.String(),
			shimErr.String(), directErr.String(),
		)
	}
}

func TestIssueCreateIsWithheldUntilFinalDecision(t *testing.T) {
	for _, test := range []struct {
		name     string
		commit   bool
		wantRuns string
	}{
		{name: "discarded", commit: false, wantRuns: ""},
		{name: "committed", commit: true, wantRuns: "create\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runLog := filepath.Join(t.TempDir(), "runs")
			fake := writeFakeGH(t, "printf 'create\\n' >> \""+runLog+"\"\n")
			session := startTestSession(t, fake, nil)
			setClientEnvironment(t, session)
			var shimErr bytes.Buffer
			code := RunClient(
				[]string{"issue", "create", "--repo", "acme/widget", "--title", "shim issue"},
				strings.NewReader(""), &bytes.Buffer{}, &shimErr,
			)
			if code != 0 || !strings.Contains(shimErr.String(), "staged") {
				t.Fatalf("staged create result: code=%d stderr=%q", code, shimErr.String())
			}
			if _, err := os.Stat(runLog); !os.IsNotExist(err) {
				t.Fatalf("fake gh ran during the session: %v", err)
			}
			if err := session.Finalize(context.Background(), test.commit); err != nil {
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
			if len(records) != 1 || records[0].Decision != test.name {
				t.Fatalf("gh summary = %#v", records)
			}
		})
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
