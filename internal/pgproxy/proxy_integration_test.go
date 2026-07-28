package pgproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSharedTransactionIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	directConfig := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, directConfig)
	defer direct.Close(ctx)

	table := "unring_shared_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := "unring_created_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, directConfig)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup,
			fmt.Sprintf("DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s", table, created))
	})

	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE TABLE %s (id integer PRIMARY KEY, value text); INSERT INTO %s VALUES (1, 'before')",
		table, table,
	))

	proxy, err := Start(ctx, directConfig)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer proxy.Close()
	proxyConfig := proxyTestConfig(t, proxy.Address(), directConfig)

	first := connectTest(t, ctx, proxyConfig)
	execTest(t, ctx, first, fmt.Sprintf(
		"CREATE TABLE %s (value text); INSERT INTO %s VALUES ('visible'); "+
			"DELETE FROM %s WHERE id = 1; INSERT INTO %s VALUES (2, 'inside')",
		created, created, table, table,
	))
	if got := scalarTest(t, ctx, first, fmt.Sprintf("SELECT value FROM %s", created)); got != "visible" {
		t.Fatalf("first proxy client reads its inserted value %q, want visible", got)
	}
	if got := scalarTest(t, ctx, first, fmt.Sprintf("SELECT value FROM %s", table)); got != "inside" {
		t.Fatalf("first proxy client reads post-delete value %q, want inside", got)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first proxy client: %v", err)
	}

	second := connectTest(t, ctx, proxyConfig)
	defer second.Close(ctx)
	if got := scalarTest(t, ctx, second, fmt.Sprintf("SELECT count(*) FROM %s", created)); got != "1" {
		t.Fatalf("new proxy client sees created row count %s, want 1", got)
	}
	if got := scalarTest(t, ctx, second, fmt.Sprintf("SELECT string_agg(value, ',') FROM %s", table)); got != "inside" {
		t.Fatalf("new proxy client sees values %q, want inside", got)
	}

	if got := scalarTest(t, ctx, direct, fmt.Sprintf("SELECT count(*) FROM %s", table)); got != "1" {
		t.Fatalf("direct client sees row count %s during session, want original count 1", got)
	}
	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", created)); got != "t" {
		t.Fatalf("direct client unexpectedly sees proxy-created table: %s", got)
	}

	assertConcurrentClients(t, ctx, proxyConfig)

	if _, err := second.Exec(ctx, "VACUUM").ReadAll(); err == nil ||
		!strings.Contains(err.Error(), "declined") {
		t.Fatalf("VACUUM error = %v, want safe approval decline", err)
	}
	if got := scalarTest(t, ctx, second, "SELECT 42"); got != "42" {
		t.Fatalf("session did not recover after backend error: got %s", got)
	}
	execTest(t, ctx, second, "SET client_min_messages TO DEBUG5")
	if got := scalarTest(t, ctx, second, "SELECT 43"); got != "43" {
		t.Fatalf("session did not tolerate notices around internal queries: got %s", got)
	}
	execTest(t, ctx, second, "COMMIT")
	smuggledCommit := fmt.Sprintf(
		`UPDATE %s SET value = 'C:\'; COMMIT;`,
		table,
	)
	execTest(t, ctx, second, smuggledCommit)
	for _, sql := range map[string]string{
		"carriage-return comment": "SELECT 1; --\rCOMMIT;",
		"guessed savepoint escalation": "SELECT 1; --\rCOMMIT; BEGIN; " +
			"SAVEPOINT unring_internal_1;",
		"UTF-8 dollar tag":        "SELECT $\xc3\xa9$'$\xc3\xa9$; COMMIT;",
		"continued escape string": "SELECT E'a'\n'\\'e'; COMMIT; --'",
	} {
		execTest(t, ctx, second, sql)
	}
	if got := scalarTest(t, ctx, second,
		"SELECT $\xe4\xb8\xad$; COMMIT;$\xe4\xb8\xad$"); got != "; COMMIT;" {
		t.Fatalf("valid UTF-8 dollar-quoted literal = %q, want ; COMMIT;", got)
	}
	if got := scalarTest(t, ctx, second,
		"SELECT E'a'\n'\\'; COMMIT; --'"); got != "a'; COMMIT; --" {
		t.Fatalf("valid continued escape string = %q, want %q",
			got, "a'; COMMIT; --")
	}
	if got := scalarTest(t, ctx, second,
		fmt.Sprintf("SELECT value FROM %s", table)); got != `C:\` {
		t.Fatalf("translated smuggled COMMIT lost preceding write: got %q", got)
	}
	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT value FROM %s", table)); got != "before" {
		t.Fatalf("client COMMIT exposed the shared transaction: direct value %q", got)
	}

	if err := second.Close(ctx); err != nil {
		t.Fatalf("close second proxy client: %v", err)
	}
	if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
		t.Fatalf("rollback proxy: %v", err)
	}

	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT id || ':' || value FROM %s", table)); got != "1:before" {
		t.Fatalf("discard did not restore original table: got %q", got)
	}
	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT to_regclass('public.%s') IS NULL", created)); got != "t" {
		t.Fatalf("discard left proxy-created table behind: %s", got)
	}

	commitProxy, err := Start(ctx, directConfig)
	if err != nil {
		t.Fatalf("Start() for commit error: %v", err)
	}
	defer commitProxy.Close()
	commitClient := connectTest(t, ctx, proxyTestConfig(t, commitProxy.Address(), directConfig))
	execTest(t, ctx, commitClient,
		fmt.Sprintf("INSERT INTO %s VALUES (3, 'committed')", table))
	if err := commitClient.Close(ctx); err != nil {
		t.Fatalf("close commit client: %v", err)
	}
	if err := commitProxy.Finalize(ctx, DecisionCommit); err != nil {
		t.Fatalf("commit proxy: %v", err)
	}
	if got := scalarTest(t, ctx, direct,
		fmt.Sprintf("SELECT value FROM %s WHERE id = 3", table)); got != "committed" {
		t.Fatalf("committed value = %q, want committed", got)
	}
}

func TestSealedChangeSummaryIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	firstTable := "unring_summary_first_" + suffix
	secondTable := "unring_summary_second_" + suffix
	createdTable := "unring_summary_created_" + suffix
	createdIndex := createdTable + "_idx"
	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE TABLE %s (id integer PRIMARY KEY, value text); "+
			"CREATE TABLE %s (id integer PRIMARY KEY, value text); "+
			"INSERT INTO %s VALUES (1, 'one'), (2, 'two'); "+
			"INSERT INTO %s VALUES (1, 'one')",
		firstTable, secondTable, firstTable, secondTable,
	))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf(
			"DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s",
			createdTable, firstTable, secondTable))
	})

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, fmt.Sprintf(
		"INSERT INTO %s VALUES (3, 'three'), (4, 'four'); "+
			"UPDATE %s SET value = 'changed' WHERE id IN (1, 2); "+
			"DELETE FROM %s WHERE id = 1; "+
			"INSERT INTO %s VALUES (2, 'two'), (3, 'three'), (4, 'four'); "+
			"UPDATE %s SET value = 'changed' WHERE id = 2; "+
			"DELETE FROM %s WHERE id IN (1, 3)",
		firstTable, firstTable, firstTable, secondTable, secondTable, secondTable,
	))
	execTest(t, ctx, client, fmt.Sprintf(
		"CREATE TABLE %s (id integer); CREATE INDEX %s ON %s (id)",
		createdTable, createdIndex, createdTable))
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}

	summary := proxy.Summary()
	if !summary.Sealed || !summary.Changes.Complete || summary.Changes.Error != "" {
		t.Fatalf("sealed change summary is incomplete: %#v", summary.Changes)
	}
	wantRows := map[string]RowChange{
		"public." + firstTable:  {Table: "public." + firstTable, Inserted: 2, Updated: 2, Deleted: 1},
		"public." + secondTable: {Table: "public." + secondTable, Inserted: 3, Updated: 1, Deleted: 2},
	}
	if len(summary.Changes.Rows) != len(wantRows) {
		t.Fatalf("row changes = %#v, want %#v", summary.Changes.Rows, wantRows)
	}
	for _, change := range summary.Changes.Rows {
		if want, ok := wantRows[change.Table]; !ok || change != want {
			t.Fatalf("row change = %#v, want one of %#v", change, wantRows)
		}
	}
	wantSchema := map[string]bool{
		"created table public." + createdTable: true,
		"created index public." + createdIndex: true,
	}
	for _, change := range summary.Changes.Schema {
		delete(wantSchema, change.Action+" "+change.Kind+" "+change.Object)
	}
	if len(wantSchema) != 0 {
		t.Fatalf("schema changes %#v omitted %#v", summary.Changes.Schema, wantSchema)
	}
}

func TestZeroRowUpdateIsReadOnlyIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	table := "unring_zero_update_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	execTest(t, ctx, direct, fmt.Sprintf("CREATE TABLE %s (id integer PRIMARY KEY)", table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, fmt.Sprintf("UPDATE %s SET id = id WHERE id = -1", table))
	execTest(t, ctx, client, "SET search_path = pg_catalog")
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if len(summary.Changes.Rows) != 0 || len(summary.Changes.Schema) != 0 {
		t.Fatalf("zero-row update reported changes: %#v", summary.Changes)
	}
	if summary.HasReviewableActivity() {
		t.Fatalf("zero-row update requires review: %#v", summary)
	}
}

func TestClientCancelReachesSharedBackendIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	defer client.Close(ctx)

	queryDone := make(chan error, 1)
	go func() {
		_, queryErr := client.Exec(ctx, "SELECT pg_sleep(30)").ReadAll()
		queryDone <- queryErr
	}()

	activeDeadline := time.Now().Add(5 * time.Second)
	for {
		active := scalarTest(t, ctx, direct, `
SELECT count(*)::text
FROM pg_stat_activity
WHERE state = 'active' AND query = 'SELECT pg_sleep(30)'`)
		if active != "0" {
			break
		}
		if time.Now().After(activeDeadline) {
			t.Fatal("sleeping query did not become active")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancelContext, cancelRequest := context.WithTimeout(context.Background(), 5*time.Second)
	if err := client.CancelRequest(cancelContext); err != nil {
		cancelRequest()
		t.Fatalf("CancelRequest(): %v", err)
	}
	cancelRequest()

	select {
	case queryErr := <-queryDone:
		var postgresError *pgconn.PgError
		if !errors.As(queryErr, &postgresError) || postgresError.Code != "57014" {
			t.Fatalf("cancelled query error = %v, want SQLSTATE 57014", queryErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled query remained blocked")
	}
	if got := scalarTest(t, ctx, client, "SELECT 1"); got != "1" {
		t.Fatalf("query after cancellation = %q, want 1", got)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	if summary := proxy.Summary(); len(summary.Unintercepted) != 0 {
		t.Fatalf("handled cancellation was reported as un-intercepted: %#v", summary.Unintercepted)
	}
}

func TestRowSummaryIncludesTriggerWritesAndExcludesRolledBackSavepointIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	source := "unring_trigger_source_" + suffix
	audit := "unring_trigger_audit_" + suffix
	function := "unring_trigger_fn_" + suffix
	execTest(t, ctx, direct, fmt.Sprintf(`
CREATE TABLE %s (id integer PRIMARY KEY);
CREATE TABLE %s (source_id integer);
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO %s VALUES (NEW.id);
  RETURN NEW;
END
$$;
CREATE TRIGGER write_audit AFTER INSERT ON %s
FOR EACH ROW EXECUTE FUNCTION %s()`, source, audit, function, audit, source, function))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf(
			"DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s; DROP FUNCTION IF EXISTS %s()",
			source, audit, function))
	})

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, fmt.Sprintf("INSERT INTO %s VALUES (1)", source))
	execTest(t, ctx, client, "BEGIN")
	execTest(t, ctx, client, fmt.Sprintf("INSERT INTO %s VALUES (2)", source))
	execTest(t, ctx, client, "ROLLBACK")
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}

	want := map[string]RowChange{
		"public." + source: {Table: "public." + source, Inserted: 1},
		"public." + audit:  {Table: "public." + audit, Inserted: 1},
	}
	summary := proxy.Summary()
	if len(summary.Changes.Rows) != len(want) {
		t.Fatalf("trigger/savepoint row changes = %#v, want %#v", summary.Changes.Rows, want)
	}
	for _, change := range summary.Changes.Rows {
		if expected, ok := want[change.Table]; !ok || change != expected {
			t.Fatalf("trigger/savepoint row change = %#v, want one of %#v", change, want)
		}
	}
}

func assertConcurrentClients(t *testing.T, ctx context.Context, config *pgconn.Config) {
	t.Helper()
	const clients = 2
	const queries = 30

	var wait sync.WaitGroup
	errorsFound := make(chan error, clients)
	for clientNumber := 0; clientNumber < clients; clientNumber++ {
		clientNumber := clientNumber
		wait.Add(1)
		go func() {
			defer wait.Done()
			connection, err := pgconn.ConnectConfig(ctx, config.Copy())
			if err != nil {
				errorsFound <- fmt.Errorf("connect concurrent client %d: %w", clientNumber, err)
				return
			}
			defer connection.Close(ctx)

			for queryNumber := 0; queryNumber < queries; queryNumber++ {
				want := fmt.Sprintf("%d-%d", clientNumber, queryNumber)
				results, err := connection.Exec(ctx, fmt.Sprintf(
					"SELECT '%s'::text, pg_sleep(0.001)", want,
				)).ReadAll()
				if err != nil {
					errorsFound <- fmt.Errorf("client %d query %d: %w",
						clientNumber, queryNumber, err)
					return
				}
				got := string(results[0].Rows[0][0])
				if got != want {
					errorsFound <- fmt.Errorf("client %d query %d got %q, want %q",
						clientNumber, queryNumber, got, want)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func parseTestConfig(t *testing.T, connectionString string) *pgconn.Config {
	t.Helper()
	config, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		t.Fatalf("parse test connection string: %v", err)
	}
	return config
}

func proxyTestConfig(t *testing.T, address string, backend *pgconn.Config) *pgconn.Config {
	t.Helper()
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil {
		t.Fatalf("parse proxy port: %v", err)
	}

	connectionURL := &url.URL{
		Scheme: "postgresql",
		User:   url.User(backend.User),
		Host:   net.JoinHostPort(host, strconv.FormatUint(port, 10)),
		Path:   "/" + backend.Database,
	}
	query := connectionURL.Query()
	query.Set("sslmode", "disable")
	connectionURL.RawQuery = query.Encode()
	return parseTestConfig(t, connectionURL.String())
}

func connectTest(t *testing.T, ctx context.Context, config *pgconn.Config) *pgconn.PgConn {
	t.Helper()
	connection, err := pgconn.ConnectConfig(ctx, config.Copy())
	if err != nil {
		t.Fatalf("connect test postgres: %v", err)
	}
	return connection
}

func execTest(t *testing.T, ctx context.Context, connection *pgconn.PgConn, sql string) {
	t.Helper()
	if _, err := connection.Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("execute %q: %v", sql, err)
	}
}

func scalarTest(t *testing.T, ctx context.Context, connection *pgconn.PgConn, sql string) string {
	t.Helper()
	results, err := connection.Exec(ctx, sql).ReadAll()
	if err != nil {
		t.Fatalf("execute scalar %q: %v", sql, err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		t.Fatalf("scalar %q returned unexpected shape: %#v", sql, results)
	}
	return string(results[0].Rows[0][0])
}
