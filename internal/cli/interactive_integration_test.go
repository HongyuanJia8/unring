package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

func TestBuiltBinaryRunsInteractiveChild(t *testing.T) {
	connectionString, backendDone := startInteractiveTestBackend(t)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--",
		"/bin/sh",
		"-c",
		`printf 'unring-test-child-ready\n'; IFS= read -r line; printf 'child-read:%s\n' "$line"`,
	)
	command.Env = os.Environ()
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start built unring binary under PTY: %v", err)
	}
	defer terminal.Close()

	session := newInteractiveSession(t, terminal, command)
	session.waitFor(
		"unring-test-child-ready",
		"the interactive child to take the foreground terminal",
	)
	session.write("hello-terminal\n", "the interactive child's input")
	session.waitFor(
		"child-read:hello-terminal",
		"the interactive child to read from the foreground terminal",
	)
	session.waitFor(interactiveReviewMarker, "unring to display its review prompt")
	session.write("d", "the review discard decision")
	output := session.finish("unring to exit after the review discard decision")
	if !strings.Contains(output, "child-read:hello-terminal") {
		t.Fatalf("interactive child did not read from the foreground TTY:\n%s", output)
	}
	if !strings.Contains(output, "Session discarded.") {
		t.Fatalf("built unring binary did not regain the TTY for its prompt:\n%s", output)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestCommitFlagCannotOverrideSignaledChild(t *testing.T) {
	connectionString, backendDone := startInteractiveTestBackend(t)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--commit",
		"--",
		"/bin/sh",
		"-c",
		"kill -INT $$",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("signal-terminated unring error = %v, want *exec.ExitError\n%s", err, output)
	}
	if got, want := exitError.ExitCode(), 128+int(syscall.SIGINT); got != want {
		t.Fatalf("signal-terminated unring exit code = %d, want %d\n%s", got, want, output)
	}
	if strings.Contains(string(output), "Session committed.") ||
		!strings.Contains(string(output), "Session discarded.") {
		t.Fatalf("--commit overrode a signal-terminated child:\n%s", output)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestReadOnlySessionExitsSilently(t *testing.T) {
	connectionString, backendDone := startReviewTestBackend(t, false)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("find true command: %v", err)
	}
	command := exec.Command(binary, "run", "--", truePath)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read-only unring run failed: %v\n%s", err, output)
	}
	if len(output) != 0 {
		t.Fatalf("read-only unring run emitted output: %q", output)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestGHCreateRunsOnlyOnCommitAgainstFakeGH(t *testing.T) {
	binary := buildTestBinary(t)
	for _, test := range []struct {
		name     string
		decision string
		wantRun  bool
	}{
		{name: "discard", decision: "--discard", wantRun: false},
		{name: "commit", decision: "--commit", wantRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connectionString, backendDone := startReviewTestBackend(t, false)
			runLog := filepath.Join(t.TempDir(), "gh-runs")
			fakeDirectory := t.TempDir()
			fakeGH := filepath.Join(fakeDirectory, "gh")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + runLog + "\"\n"
			if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake gh: %v", err)
			}
			t.Setenv("DATABASE_URL", connectionString)
			t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("UNRING_STATE_DIR", t.TempDir())
			command := exec.Command(
				binary, "run", test.decision, "--",
				"/bin/sh", "-c",
				"gh issue create --repo acme/widget --title 'shim acceptance'",
			)
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("unring gh %s failed: %v\n%s", test.name, err, output)
			}
			data, readErr := os.ReadFile(runLog)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read fake gh log: %v", readErr)
			}
			ran := strings.Contains(string(data), "issue create")
			if ran != test.wantRun {
				t.Fatalf("fake gh ran=%v, want %v; log=%q\n%s",
					ran, test.wantRun, data, output)
			}
			if !strings.Contains(string(output), "staged create GitHub issue") {
				t.Fatalf("gh mutation was not visibly staged:\n%s", output)
			}
			if err := <-backendDone; err != nil {
				t.Fatalf("fake Postgres backend: %v", err)
			}
		})
	}
}

func TestNonTerminalReviewUsesPlainTextWithoutANSI(t *testing.T) {
	connectionString, backendDone := startReviewTestBackend(t, true)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("find true command: %v", err)
	}
	command := exec.Command(binary, "run", "--", truePath)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("non-terminal unring run failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "SCHEMA CHANGES") ||
		!strings.Contains(text, "No interactive terminal; defaulting to discard") {
		t.Fatalf("plain-text fallback review missing:\n%s", text)
	}
	if strings.Contains(text, "\x1b[") || strings.Contains(text, "\x1b]") {
		t.Fatalf("plain-text fallback contained ANSI escapes: %q", text)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestNamedAliasRunsCommand(t *testing.T) {
	testCommandAlias(t, "claude")
}

func TestArbitraryPathAliasRunsCommand(t *testing.T) {
	testCommandAlias(t, "unring-path-command")
}

func testCommandAlias(t *testing.T, name string) {
	t.Helper()
	connectionString, backendDone := startReviewTestBackend(t, true)
	t.Setenv("DATABASE_URL", connectionString)
	directory := t.TempDir()
	child := filepath.Join(directory, name)
	if err := os.WriteFile(child, []byte("#!/bin/sh\nprintf 'alias:%s:%s\\n' \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatalf("write alias child: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	binary := buildTestBinary(t)
	command := exec.Command(binary, name, "--", "marker")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("unring %s failed: %v\n%s", name, err, output)
	}
	if !strings.Contains(string(output), "alias:--:marker") {
		t.Fatalf("unring %s did not run PATH command with alias arguments:\n%s", name, output)
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestBuiltBinaryDiscardsStoppedInteractiveChild(t *testing.T) {
	connectionString, backendDone := startInteractiveTestBackend(t)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--commit",
		"--",
		os.Args[0],
		"-test.run=^TestStoppedInteractiveChildProcess$",
	)
	command.Env = append(os.Environ(), "UNRING_CLI_STOP_CHILD=1")
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start built unring binary under PTY: %v", err)
	}
	defer terminal.Close()

	reader := bufio.NewReader(terminal)
	var output strings.Builder
	for {
		line, err := reader.ReadString('\n')
		output.WriteString(line)
		if strings.Contains(line, "child-ready") {
			break
		}
		if err != nil {
			t.Fatalf("wait for stopped-child readiness: %v\n%s", err, output.String())
		}
	}
	if _, err := terminal.Write([]byte{0x1a}); err != nil {
		t.Fatalf("send terminal Ctrl-Z: %v", err)
	}

	type readResult struct {
		output []byte
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		remaining, err := io.ReadAll(reader)
		readDone <- readResult{output: remaining, err: err}
	}()

	var readResultValue readResult
	select {
	case readResultValue = <-readDone:
	case <-time.After(10 * time.Second):
		_ = terminal.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf("built unring did not abort after Ctrl-Z:\n%s", output.String())
	}
	output.Write(readResultValue.output)
	if readResultValue.err != nil && !errors.Is(readResultValue.err, syscall.EIO) {
		t.Fatalf("read Ctrl-Z session output: %v\n%s",
			readResultValue.err, output.String())
	}

	err = command.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("Ctrl-Z unring error = %v, want *exec.ExitError\n%s", err, output.String())
	}
	if got, want := exitError.ExitCode(), 128+int(syscall.SIGKILL); got != want {
		t.Fatalf("Ctrl-Z unring exit code = %d, want %d\n%s", got, want, output.String())
	}
	if strings.Contains(output.String(), "Session committed.") ||
		!strings.Contains(output.String(), "Session discarded.") {
		t.Fatalf("--commit overrode a Ctrl-Z interruption:\n%s", output.String())
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestStoppedInteractiveChildProcess(t *testing.T) {
	if os.Getenv("UNRING_CLI_STOP_CHILD") != "1" {
		return
	}

	fmt.Println("child-ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestBuiltBinaryRunsInteractivePsql(t *testing.T) {
	psqlPath, err := exec.LookPath("psql")
	if err != nil {
		if os.Getenv("UNRING_REQUIRE_POSTGRES") == "1" {
			t.Fatalf("interactive integration test requires psql: %v", err)
		}
		t.Skipf("interactive integration test skipped: psql is not available: %v", err)
	}
	connectionString := testpostgres.Start(t)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	table := "unring_interactive_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	command := exec.Command(
		binary,
		"run",
		"--",
		psqlPath,
		"-X",
		"-P", "pager=off",
		"-v", "ON_ERROR_STOP=1",
		"-v", "PROMPT1="+interactivePsqlMarker,
	)
	command.Env = os.Environ()
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatalf("start built unring binary under PTY: %v", err)
	}
	defer terminal.Close()

	session := newInteractiveSession(t, terminal, command)
	session.waitFor(interactivePsqlMarker, "psql to display its initial prompt")
	session.write(
		fmt.Sprintf("CREATE TABLE %s (value text);\n", table),
		"the psql CREATE TABLE statement",
	)
	session.waitFor(interactivePsqlMarker, "psql to finish CREATE TABLE")
	session.write(
		fmt.Sprintf("INSERT INTO %s VALUES ('interactive-value');\n", table),
		"the psql INSERT statement",
	)
	session.waitFor(interactivePsqlMarker, "psql to finish INSERT")
	session.write(
		fmt.Sprintf("SELECT value FROM %s;\n", table),
		"the psql SELECT statement",
	)
	session.waitFor(interactivePsqlMarker, "psql to finish SELECT")
	session.write("\\q\n", "the psql quit command")
	session.waitFor(interactiveReviewMarker, "unring to regain the terminal and display its review prompt")
	session.write("d", "the review discard decision")
	outputText := session.finish("unring to exit after the review discard decision")
	if !strings.Contains(outputText, "interactive-value") {
		t.Fatalf("psql did not read its write through unring:\n%s", outputText)
	}
	if !strings.Contains(outputText, "Session discarded.") {
		t.Fatalf("unring did not regain the TTY and discard at its prompt:\n%s", outputText)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	directConfig, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		t.Fatalf("parse direct Postgres connection: %v", err)
	}
	direct, err := pgconn.ConnectConfig(ctx, directConfig)
	if err != nil {
		t.Fatalf("connect directly after interactive run: %v", err)
	}
	defer direct.Close(ctx)
	defer func() {
		_, _ = direct.Exec(context.Background(),
			fmt.Sprintf("DROP TABLE IF EXISTS %s", table)).ReadAll()
	}()
	if got := signalScalar(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", table)); got != "t" {
		t.Fatalf("interactive discard left table behind: %s", got)
	}
}

func TestApprovedIrreversibleActionAlwaysGetsReview(t *testing.T) {
	psqlPath, err := exec.LookPath("psql")
	if err != nil {
		if os.Getenv("UNRING_REQUIRE_POSTGRES") == "1" {
			t.Fatalf("irreversible review integration test requires psql: %v", err)
		}
		t.Skipf("irreversible review integration test skipped: psql is not available: %v", err)
	}
	connectionString := testpostgres.Start(t)
	t.Setenv("DATABASE_URL", connectionString)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--",
		psqlPath,
		"-X",
		"-P", "pager=off",
		"-v", "PROMPT1="+interactivePsqlMarker,
	)
	command.Env = os.Environ()
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		t.Fatalf("start irreversible psql review under PTY: %v", err)
	}
	defer terminal.Close()

	session := newInteractiveSession(t, terminal, command)
	session.waitFor(interactivePsqlMarker, "psql to display its initial prompt")
	session.write("VACUUM;\n", "the irreversible VACUUM statement")
	session.waitFor(
		"Run this irreversible action? [y/N] ",
		"unring to reclaim the terminal and request irreversible-action approval",
	)
	session.write("y\n", "the irreversible-action approval")
	session.waitFor(
		interactivePsqlMarker,
		"psql to regain the terminal after the irreversible-action approval",
	)
	session.write("\\q\n", "the psql quit command")
	session.waitFor(
		interactiveReviewMarker,
		"unring to regain the terminal and display the irreversible-session review",
	)
	session.write("d", "the review discard decision")
	output := session.finish("unring to exit after the irreversible-session review")
	if !strings.Contains(output, "WARNING: THIS SESSION IS NOT FULLY REVERSIBLE") ||
		!strings.Contains(output, "APPROVED IRREVERSIBLE ACTIONS") ||
		!strings.Contains(output, "Session discarded.") {
		t.Fatalf("approved irreversible action was not prominently reviewed:\n%s", output)
	}
}

func buildTestBinary(t *testing.T) string {
	t.Helper()

	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "unring")
	build := exec.Command("go", "build", "-o", binary, "./cmd/unring")
	build.Dir = repositoryRoot
	build.Env = os.Environ()
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build unring binary: %v\n%s", err, output)
	}
	return binary
}

const (
	interactivePsqlMarker   = "unring-test-psql-ready> "
	interactiveReviewMarker = "Up/down: select  Enter/space: expand  c: commit  d: discard"
	interactiveTimeout      = 30 * time.Second
)

type interactiveSession struct {
	t            *testing.T
	terminal     *os.File
	command      *exec.Cmd
	output       *synchronizedBuffer
	readDone     chan error
	deadline     time.Time
	searchOffset int
}

func newInteractiveSession(
	t *testing.T,
	terminal *os.File,
	command *exec.Cmd,
) *interactiveSession {
	t.Helper()

	session := &interactiveSession{
		t:        t,
		terminal: terminal,
		command:  command,
		output:   newSynchronizedBuffer(),
		readDone: make(chan error, 1),
		deadline: time.Now().Add(interactiveTimeout),
	}
	go func() {
		_, err := io.Copy(session.output, terminal)
		session.readDone <- err
	}()
	return session
}

func (session *interactiveSession) waitFor(marker, description string) {
	session.t.Helper()

	for {
		output, updated := session.output.snapshot()
		if index := strings.Index(output[session.searchOffset:], marker); index >= 0 {
			session.searchOffset += index + len(marker)
			return
		}

		timer := time.NewTimer(time.Until(session.deadline))
		select {
		case <-updated:
			if !timer.Stop() {
				<-timer.C
			}
		case readErr := <-session.readDone:
			if !timer.Stop() {
				<-timer.C
			}
			session.failAfterExit(description, readErr)
		case <-timer.C:
			session.failTimeout(description)
		}
	}
}

func (session *interactiveSession) write(input, description string) {
	session.t.Helper()

	if _, err := io.WriteString(session.terminal, input); err != nil {
		session.killAndWait()
		session.t.Fatalf(
			"write %s: %v\noutput captured before write failure:\n%s",
			description,
			err,
			session.output.String(),
		)
	}
}

func (session *interactiveSession) finish(description string) string {
	session.t.Helper()

	timer := time.NewTimer(time.Until(session.deadline))
	var readErr error
	select {
	case readErr = <-session.readDone:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
		session.failTimeout(description)
	}
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		session.killAndWait()
		session.t.Fatalf(
			"read interactive unring output while waiting for %s: %v\n"+
				"output captured before read failure:\n%s",
			description,
			readErr,
			session.output.String(),
		)
	}
	if err := session.command.Wait(); err != nil {
		session.t.Fatalf(
			"interactive unring exited while waiting for %s: %v\n"+
				"output captured before exit:\n%s",
			description,
			err,
			session.output.String(),
		)
	}
	return session.output.String()
}

func (session *interactiveSession) failAfterExit(description string, readErr error) {
	session.t.Helper()

	waitErr := session.command.Wait()
	session.t.Fatalf(
		"interactive unring exited before %s\n"+
			"process error: %v\nPTY read error: %v\n"+
			"output captured before exit:\n%s",
		description,
		waitErr,
		readErr,
		session.output.String(),
	)
}

func (session *interactiveSession) failTimeout(description string) {
	session.t.Helper()

	session.killAndWait()
	session.t.Fatalf(
		"interactive unring timed out after %s waiting for %s\n"+
			"output captured before timeout:\n%s",
		interactiveTimeout,
		description,
		session.output.String(),
	)
}

func (session *interactiveSession) killAndWait() {
	_ = session.terminal.Close()
	_ = syscall.Kill(-session.command.Process.Pid, syscall.SIGKILL)
	_ = session.command.Wait()
}

type synchronizedBuffer struct {
	mutex   sync.Mutex
	data    bytes.Buffer
	updated chan struct{}
}

func newSynchronizedBuffer() *synchronizedBuffer {
	return &synchronizedBuffer{updated: make(chan struct{})}
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	count, err := buffer.data.Write(data)
	close(buffer.updated)
	buffer.updated = make(chan struct{})
	return count, err
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.String()
}

func (buffer *synchronizedBuffer) snapshot() (string, <-chan struct{}) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.String(), buffer.updated
}

func startInteractiveTestBackend(t *testing.T) (string, <-chan error) {
	return startReviewTestBackend(t, true)
}

func startReviewTestBackend(t *testing.T, reportSchemaChange bool) (string, <-chan error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake Postgres backend: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			done <- fmt.Errorf("accept: %w", err)
			return
		}
		defer connection.Close()

		backend := pgproto3.NewBackend(connection, connection)
		startup, err := backend.ReceiveStartupMessage()
		if err != nil {
			done <- fmt.Errorf("receive startup: %w", err)
			return
		}
		if _, ok := startup.(*pgproto3.StartupMessage); !ok {
			done <- fmt.Errorf("unexpected startup message %T", startup)
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{
			Name: "standard_conforming_strings", Value: "on",
		})
		backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: 2})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			done <- fmt.Errorf("send startup response: %w", err)
			return
		}

		catalogQueries := 0
		for {
			message, err := backend.Receive()
			if err != nil {
				done <- fmt.Errorf("receive backend query: %w", err)
				return
			}
			query, ok := message.(*pgproto3.Query)
			if !ok {
				done <- fmt.Errorf("got backend message %T, want Query", message)
				return
			}
			status := byte('T')
			tag := query.String
			switch {
			case query.String == "BEGIN":
				tag = "BEGIN"
			case query.String == "SHOW server_version_num":
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("server_version_num")},
				}})
				backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("170000")}})
				tag = "SHOW"
			case query.String == "ROLLBACK" || query.String == "COMMIT":
				status = 'I'
			case strings.HasPrefix(query.String, "SAVEPOINT ") &&
				strings.Contains(query.String, "SET LOCAL search_path = pg_catalog"):
				tag = "SET"
			case strings.HasPrefix(query.String, "ROLLBACK TO SAVEPOINT "):
				tag = "RELEASE"
			case strings.Contains(query.String, "pg_stat_get_xact_tuples_inserted"):
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("oid")}, {Name: []byte("name")},
					{Name: []byte("inserted")}, {Name: []byte("updated")},
					{Name: []byte("deleted")},
				}})
				tag = "SELECT 0"
			case strings.Contains(query.String, "FROM pg_locks l"):
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("sequence")},
				}})
				tag = "SELECT 0"
			case strings.Contains(query.String, "SELECT c.oid::text"):
				catalogQueries++
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("oid")}, {Name: []byte("kind")},
					{Name: []byte("name")}, {Name: []byte("fingerprint")},
				}})
				if reportSchemaChange && catalogQueries == 2 {
					backend.Send(&pgproto3.DataRow{Values: [][]byte{
						[]byte("12345"), []byte("schema"), []byte("review_change"), []byte("owner"),
					}})
				}
				tag = "SELECT 0"
			default:
				done <- fmt.Errorf("unexpected backend query %q", query.String)
				return
			}
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
			if err := backend.Flush(); err != nil {
				done <- fmt.Errorf("send response for %q: %w", query.String, err)
				return
			}
			if query.String == "ROLLBACK" || query.String == "COMMIT" {
				return
			}
		}
	}()

	connectionURL := &url.URL{
		Scheme: "postgresql",
		User:   url.User("postgres"),
		Host:   listener.Addr().String(),
		Path:   "/postgres",
	}
	query := connectionURL.Query()
	query.Set("sslmode", "disable")
	connectionURL.RawQuery = query.Encode()
	return connectionURL.String(), done
}
