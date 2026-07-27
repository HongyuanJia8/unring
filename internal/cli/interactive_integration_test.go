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

	testpostgres "github.com/HongyuanJia8/unring/internal/testsupport/postgres"
	"github.com/creack/pty"
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
		`IFS= read -r line; printf 'child-read:%s\n' "$line"`,
	)
	command.Env = os.Environ()
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start built unring binary under PTY: %v", err)
	}
	defer terminal.Close()

	if _, err := terminal.Write([]byte("hello-terminal\nd\n")); err != nil {
		t.Fatalf("write interactive input: %v", err)
	}

	output := readInteractiveOutput(t, terminal, command)
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
	)
	command.Env = os.Environ()
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatalf("start built unring binary under PTY: %v", err)
	}
	defer terminal.Close()

	script := fmt.Sprintf(
		"CREATE TABLE %s (value text);\n"+
			"INSERT INTO %s VALUES ('interactive-value');\n"+
			"SELECT value FROM %s;\n"+
			"\\q\n"+
			"d\n",
		table, table, table,
	)
	if _, err := terminal.Write([]byte(script)); err != nil {
		t.Fatalf("write psql interaction: %v", err)
	}

	outputText := readInteractiveOutput(t, terminal, command)
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
	command := exec.Command(binary, "run", "--", psqlPath, "-X", "-P", "pager=off")
	command.Env = os.Environ()
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		t.Fatalf("start irreversible psql review under PTY: %v", err)
	}
	defer terminal.Close()

	if _, err := terminal.Write([]byte("VACUUM;\ny\n\\q\nd\n")); err != nil {
		t.Fatalf("write irreversible psql interaction: %v", err)
	}
	output := readInteractiveOutput(t, terminal, command)
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

func readInteractiveOutput(t *testing.T, terminal *os.File, command *exec.Cmd) string {
	t.Helper()

	var output synchronizedBuffer
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&output, terminal)
		readDone <- err
	}()

	var readErr error
	select {
	case readErr = <-readDone:
	case <-time.After(30 * time.Second):
		_ = terminal.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf(
			"interactive unring did not exit within 30 seconds\n"+
				"output captured before timeout:\n%s",
			output.String(),
		)
	}
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		_ = terminal.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		t.Fatalf("read interactive unring output: %v\n%s", readErr, output.String())
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("interactive unring failed: %v\n%s", err, output.String())
	}
	return output.String()
}

type synchronizedBuffer struct {
	mutex sync.Mutex
	data  bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.String()
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
			case query.String == "BEGIN ISOLATION LEVEL REPEATABLE READ":
				tag = "BEGIN"
			case query.String == "ROLLBACK":
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
			if query.String == "ROLLBACK" {
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
