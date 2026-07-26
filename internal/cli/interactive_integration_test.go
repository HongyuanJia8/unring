package cli

import (
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

		for _, expected := range []struct {
			sql    string
			status byte
		}{
			{sql: "BEGIN", status: 'T'},
			{sql: "ROLLBACK", status: 'I'},
		} {
			message, err := backend.Receive()
			if err != nil {
				done <- fmt.Errorf("receive %s: %w", expected.sql, err)
				return
			}
			query, ok := message.(*pgproto3.Query)
			if !ok || query.String != expected.sql {
				done <- fmt.Errorf("got backend message %#v, want query %s", message, expected.sql)
				return
			}
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(expected.sql)})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: expected.status})
			if err := backend.Flush(); err != nil {
				done <- fmt.Errorf("send %s response: %w", expected.sql, err)
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
