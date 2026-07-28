package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSignalForcesRollback(t *testing.T) {
	connectionString := testpostgres.Start(t)
	table := "unring_signal_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	marker := t.TempDir() + "/child-ready"

	t.Setenv("DATABASE_URL", connectionString)
	t.Setenv("UNRING_SIGNAL_HELPER", "1")
	t.Setenv("UNRING_SIGNAL_TABLE", table)
	t.Setenv("UNRING_SIGNAL_MARKER", marker)
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exited := make(chan int, 1)
	go func() {
		exited <- Main([]string{
			"run", "--commit", "--",
			os.Args[0], "-test.run=^TestSignalChildHelper$",
		}, bytes.NewReader(nil), &stdout, &stderr)
	}()

	waitForFile(t, marker, 10*time.Second)

	directConfig, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		t.Fatalf("parse direct connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	direct, err := pgconn.ConnectConfig(ctx, directConfig)
	if err != nil {
		t.Fatalf("connect direct client: %v", err)
	}
	defer direct.Close(ctx)
	defer func() {
		_, _ = direct.Exec(context.Background(),
			fmt.Sprintf("DROP TABLE IF EXISTS %s", table)).ReadAll()
	}()

	if got := signalScalar(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", table)); got != "t" {
		t.Fatalf("direct connection saw in-flight signal-test table: %s", got)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal unring test process: %v", err)
	}

	select {
	case exitCode := <-exited:
		if exitCode != 128+int(syscall.SIGTERM) {
			t.Fatalf("signal exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s",
				exitCode, 128+int(syscall.SIGTERM), stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("unring did not exit after signal\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}

	if got := signalScalar(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", table)); got != "t" {
		t.Fatalf("signal path committed instead of rolling back: %s", got)
	}
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatalf("open signal audit store: %v", err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("signal audit records = %#v, %v", records, err)
	}
	if records[0].Outcome != "discarded" || records[0].EndedAt.IsZero() ||
		len(records[0].Postgres.Changes.Schema) == 0 {
		t.Fatalf("signal audit record is incomplete: %#v", records[0])
	}

	exitCode := Main([]string{
		"run", "--discard", "--", "/bin/sh", "-c", "exit 37",
	}, bytes.NewReader(nil), &stdout, &stderr)
	if exitCode != 37 {
		t.Fatalf("child exit code = %d, want 37\nstdout:\n%s\nstderr:\n%s",
			exitCode, stdout.String(), stderr.String())
	}
}

func TestSignalChildHelper(t *testing.T) {
	if os.Getenv("UNRING_SIGNAL_HELPER") != "1" {
		return
	}

	config, err := pgconn.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("helper parse proxy connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgconn.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("helper connect to proxy: %v", err)
	}
	table := os.Getenv("UNRING_SIGNAL_TABLE")
	if _, err := connection.Exec(ctx,
		fmt.Sprintf("CREATE TABLE %s (value text); INSERT INTO %s VALUES ('signal')", table, table),
	).ReadAll(); err != nil {
		t.Fatalf("helper write through proxy: %v", err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatalf("helper close proxy connection: %v", err)
	}
	if err := os.WriteFile(os.Getenv("UNRING_SIGNAL_MARKER"), []byte("ready"), 0o600); err != nil {
		t.Fatalf("helper write readiness marker: %v", err)
	}

	select {}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child readiness marker %s", path)
}

func signalScalar(
	t *testing.T,
	ctx context.Context,
	connection *pgconn.PgConn,
	sql string,
) string {
	t.Helper()
	results, err := connection.Exec(ctx, sql).ReadAll()
	if err != nil {
		t.Fatalf("execute scalar %q: %v", sql, err)
	}
	return string(results[0].Rows[0][0])
}
