package pgproxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestExtendedProtocolORMAndPreparedStatementCollisionsIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directConfig := parseTestConfig(t, connectionString)

	proxy, err := Start(ctx, directConfig)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxy.Close()
	proxyConfig := proxyTestConfig(t, proxy.Address(), directConfig)
	table := "unring_orm_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	first := connectPGXTest(t, ctx, proxyConfig)
	defer first.Close(ctx)
	if _, err := first.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id integer PRIMARY KEY, value text)", table)); err != nil {
		t.Fatalf("extended CREATE TABLE: %v", err)
	}
	if _, err := first.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s (id, value) VALUES ($1, $2)", table), 1, "first"); err != nil {
		t.Fatalf("parameterized extended INSERT: %v", err)
	}
	var value string
	if err := first.QueryRow(ctx,
		fmt.Sprintf("SELECT value FROM %s WHERE id = $1", table), 1).Scan(&value); err != nil {
		t.Fatalf("parameterized extended SELECT: %v", err)
	}
	if value != "first" {
		t.Fatalf("parameterized value = %q, want first", value)
	}

	second := connectPGXTest(t, ctx, proxyConfig)
	defer second.Close(ctx)
	query := fmt.Sprintf("SELECT value || $1::text FROM %s WHERE id = $2", table)
	for index, connection := range []*pgx.Conn{first, second} {
		if _, err := connection.Prepare(ctx, "s0", query); err != nil {
			t.Fatalf("connection %d prepares s0: %v", index+1, err)
		}
	}
	for index, connection := range []*pgx.Conn{first, second} {
		if err := connection.QueryRow(ctx, "s0", fmt.Sprintf("-%d", index+1), 1).Scan(&value); err != nil {
			t.Fatalf("connection %d executes s0: %v", index+1, err)
		}
		want := fmt.Sprintf("first-%d", index+1)
		if value != want {
			t.Fatalf("connection %d s0 result = %q, want %q", index+1, value, want)
		}
	}

	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first ORM client: %v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatalf("close second ORM client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("wait for client handlers and protocol-object cleanup with Seal: %v", err)
	}
	proxy.queryMu.Lock()
	preparedRows, preparedErr := proxy.internalRowsLocked(
		"SELECT name, statement FROM pg_prepared_statements ORDER BY name",
	)
	proxy.queryMu.Unlock()
	if preparedErr != nil {
		t.Fatalf("inspect backend prepared statements after Seal completed cleanup: %v", preparedErr)
	}
	if len(preparedRows) != 0 {
		prepared := make([]string, 0, len(preparedRows))
		for _, row := range preparedRows {
			prepared = append(prepared, fmt.Sprintf("%q => %q", row[0], row[1]))
		}
		t.Fatalf(
			"after Seal waited for all client handlers and protocol-object cleanup, "+
				"backend still had %d prepared statements for session prefix %q: %s; want none",
			len(preparedRows), proxy.savepointPrefix, strings.Join(prepared, ", "),
		)
	}

	if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
		t.Fatalf("discard proxy: %v", err)
	}
	direct := connectTest(t, ctx, directConfig)
	defer direct.Close(ctx)
	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", table)); got != "t" {
		t.Fatalf("discard left ORM-created table behind: %s", got)
	}
}

func TestClientTransactionsAndSavepointsIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directConfig := parseTestConfig(t, connectionString)
	table := "unring_tx_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	direct := connectTest(t, ctx, directConfig)
	defer direct.Close(ctx)
	execTest(t, ctx, direct, fmt.Sprintf("CREATE TABLE %s (id integer PRIMARY KEY, value text)", table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, directConfig)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	proxy, err := Start(ctx, directConfig)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxy.Close()
	proxyConfig := proxyTestConfig(t, proxy.Address(), directConfig)
	client := connectPGXTest(t, ctx, proxyConfig)

	if _, err := client.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES ($1, $2)", table), 1, "outside"); err != nil {
		t.Fatalf("outside insert: %v", err)
	}
	transaction, err := client.Begin(ctx)
	if err != nil {
		t.Fatalf("client BEGIN: %v", err)
	}
	if _, err := transaction.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES ($1, $2)", table), 2, "rolled-back"); err != nil {
		t.Fatalf("transaction insert: %v", err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatalf("client ROLLBACK: %v", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close rollback client: %v", err)
	}

	observer := connectPGXTest(t, ctx, proxyConfig)
	var values string
	if err := observer.QueryRow(ctx,
		fmt.Sprintf("SELECT string_agg(value, ',' ORDER BY id) FROM %s", table)).Scan(&values); err != nil {
		t.Fatalf("observe rolled-back transaction: %v", err)
	}
	if values != "outside" {
		t.Fatalf("state after client ROLLBACK = %q, want outside", values)
	}

	transaction, err = observer.Begin(ctx)
	if err != nil {
		t.Fatalf("second client BEGIN: %v", err)
	}
	if _, err := transaction.Exec(ctx, "SAVEPOINT a"); err != nil {
		t.Fatalf("SAVEPOINT a: %v", err)
	}
	if _, err := transaction.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES ($1, $2)", table), 3, "savepoint-undone"); err != nil {
		t.Fatalf("nested insert before rollback: %v", err)
	}
	if _, err := transaction.Exec(ctx, "ROLLBACK TO SAVEPOINT a"); err != nil {
		t.Fatalf("ROLLBACK TO a: %v", err)
	}
	if _, err := transaction.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES ($1, $2)", table), 4, "committed-by-client"); err != nil {
		t.Fatalf("nested insert after rollback: %v", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("client COMMIT: %v", err)
	}

	if got := scalarTest(t, ctx, direct, fmt.Sprintf("SELECT count(*) FROM %s", table)); got != "0" {
		t.Fatalf("direct connection sees %s rows after client COMMIT, want 0", got)
	}

	transaction, err = observer.Begin(ctx)
	if err != nil {
		t.Fatalf("error-case BEGIN: %v", err)
	}
	if _, err := transaction.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s VALUES ($1, $2)", table), 4, "duplicate"); err == nil {
		t.Fatal("duplicate key inside client transaction unexpectedly succeeded")
	}
	if _, err := transaction.Exec(ctx, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "25P02") {
		t.Fatalf("statement after transaction error = %v, want SQLSTATE 25P02", err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatalf("recover with ROLLBACK: %v", err)
	}
	if err := observer.QueryRow(ctx, "SELECT 42").Scan(new(int)); err != nil {
		t.Fatalf("query after transaction recovery: %v", err)
	}
	if err := observer.Close(ctx); err != nil {
		t.Fatalf("close transaction observer: %v", err)
	}

	if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
		t.Fatalf("discard proxy: %v", err)
	}
	if got := scalarTest(t, ctx, direct, fmt.Sprintf("SELECT count(*) FROM %s", table)); got != "0" {
		t.Fatalf("discard left %s rows, want 0", got)
	}
}

func TestIrreversibleApprovalIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directConfig := parseTestConfig(t, connectionString)

	var requests atomic.Int32
	proxy, err := StartWithOptions(ctx, directConfig, Options{
		Approve: func(_ context.Context, request ApprovalRequest) (bool, error) {
			if request.SQL != "VACUUM" || !strings.Contains(request.Reason, "transaction block") {
				t.Errorf("approval request = %#v", request)
			}
			return requests.Add(1) == 2, nil
		},
	})
	if err != nil {
		t.Fatalf("StartWithOptions() error: %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), directConfig))

	if _, err := client.Exec(ctx, "VACUUM").ReadAll(); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined VACUUM error = %v", err)
	}
	if summary := proxy.Summary(); !summary.FullyReversible || len(summary.IrreversibleActions) != 0 {
		t.Fatalf("declined action changed reversibility summary: %#v", summary)
	}
	if _, err := client.Exec(ctx, "VACUUM").ReadAll(); err != nil {
		t.Fatalf("approved VACUUM did not run: %v", err)
	}
	summary := proxy.Summary()
	if summary.FullyReversible || len(summary.IrreversibleActions) != 1 ||
		summary.IrreversibleActions[0].SQL != "VACUUM" {
		t.Fatalf("approved action summary = %#v", summary)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close approval client: %v", err)
	}
	if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
		t.Fatalf("discard proxy with irreversible action: %v", err)
	}
}

func connectPGXTest(t *testing.T, ctx context.Context, config *pgconn.Config) *pgx.Conn {
	t.Helper()
	user := url.User(config.User)
	if config.Password != "" {
		user = url.UserPassword(config.User, config.Password)
	}
	connectionURL := &url.URL{
		Scheme: "postgresql",
		User:   user,
		Host:   net.JoinHostPort(config.Host, strconv.FormatUint(uint64(config.Port), 10)),
		Path:   "/" + config.Database,
	}
	query := connectionURL.Query()
	query.Set("sslmode", "disable")
	connectionURL.RawQuery = query.Encode()
	pgxConfig, err := pgx.ParseConfig(connectionURL.String())
	if err != nil {
		t.Fatalf("parse pgx test config: %v", err)
	}
	connection, err := pgx.ConnectConfig(ctx, pgxConfig)
	if err != nil {
		t.Fatalf("connect pgx test client: %v", err)
	}
	return connection
}
