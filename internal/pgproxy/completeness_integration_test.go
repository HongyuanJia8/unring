package pgproxy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5/pgconn"
)

func cleanupCompletenessSQL(t *testing.T, config *pgconn.Config, sql string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		connection := connectTest(t, ctx, config)
		defer connection.Close(ctx)
		execTest(t, ctx, connection, sql)
	})
}

func TestReadCommittedEscapeDDLIsVisibleInSealedSummaryIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	table, index := "unring_escape_ddl_"+suffix, "unring_escape_ddl_idx_"+suffix
	execTest(t, ctx, direct, fmt.Sprintf("CREATE TABLE %s (id integer)", table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(context.Context, ApprovalRequest) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	if got := scalarTest(t, ctx, client, "SHOW transaction_isolation"); got != "read committed" {
		t.Fatalf("shared transaction isolation = %q, want read committed", got)
	}
	execTest(t, ctx, client, fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s (id)", index, table))
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	found := false
	for _, change := range proxy.Summary().Changes.Schema {
		found = found || change.Action == "created" && change.Kind == "index" &&
			change.Object == "public."+index
	}
	if !found {
		t.Fatalf("sealed summary omitted escape-committed index: %#v", proxy.Summary())
	}
}

func TestUncountableDataEffectsNeverLookReadOnlyIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	table, view := "unring_uncountable_"+suffix, "unring_refresh_"+suffix
	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1); "+
			"CREATE MATERIALIZED VIEW %s AS SELECT * FROM %s", table, table, view, table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup, fmt.Sprintf(
			"DROP MATERIALIZED VIEW IF EXISTS %s; DROP TABLE IF EXISTS %s", view, table))
	})

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, fmt.Sprintf("INSERT INTO %s VALUES (2); TRUNCATE %s", table, table))
	execTest(t, ctx, client, fmt.Sprintf("REFRESH MATERIALIZED VIEW %s", view))
	execTest(t, ctx, client, "SELECT lo_from_bytea(0, decode('00', 'hex'))")
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if summary.Changes.Complete || !summary.HasReviewableActivity() {
		t.Fatalf("uncountable effects looked read-only: %#v", summary)
	}
	for _, want := range []string{"REFRESH MATERIALIZED VIEW", "large-object"} {
		if !strings.Contains(summary.Changes.Error, want) {
			t.Fatalf("incomplete summary omitted %q: %s", want, summary.Changes.Error)
		}
	}
	if strings.Contains(summary.Changes.Error, "TRUNCATE row count") {
		t.Fatalf("countable TRUNCATE remained unknown: %s", summary.Changes.Error)
	}
	if strings.Contains(summary.Changes.Error, "reset row counters") {
		t.Fatalf("TRUNCATE tripped the generic counter-decrease failure: %s", summary.Changes.Error)
	}
}

func TestRejectedUncountableStatementLeavesSummaryCompleteIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	if _, err := client.Exec(ctx, "TRUNCATE unring_table_that_does_not_exist").ReadAll(); err == nil {
		t.Fatal("invalid TRUNCATE unexpectedly succeeded")
	}
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if !summary.Changes.Complete || !summary.FullyReversible ||
		len(summary.IrreversibleActions) != 0 || summary.HasReviewableActivity() {
		t.Fatalf("rejected uncountable statement changed the sealed summary: %#v", summary)
	}
}

func TestSequenceAdvancementSurvivesClientRollbackInReviewIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	sequence := "unring_sequence_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	execTest(t, ctx, direct, "CREATE SEQUENCE "+sequence)
	cleanupCompletenessSQL(t, config, "DROP SEQUENCE IF EXISTS "+sequence)

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "BEGIN")
	execTest(t, ctx, client, "SELECT nextval('"+sequence+"')")
	execTest(t, ctx, client, "ROLLBACK")
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if len(summary.NonTransactional) == 0 || !summary.FullyReversible || !summary.HasReviewableActivity() {
		t.Fatalf("rolled-back sequence advancement was hidden: %#v", summary)
	}
}

func TestCatalogSummaryIncludesPreviouslyOmittedObjectClassesIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	table := "unring_catalog_" + suffix
	execTest(t, ctx, direct, fmt.Sprintf("CREATE TABLE %s (a integer, b integer)", table))
	cleanupCompletenessSQL(t, config, "DROP TABLE IF EXISTS "+table)

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	rule, statistic, publication := "rule_"+suffix, "stats_"+suffix, "pub_"+suffix
	eventFunction, eventTrigger := "event_fn_"+suffix, "event_"+suffix
	collation, enumType, castFunction := "coll_"+suffix, "enum_"+suffix, "cast_fn_"+suffix
	operatorFunction := "operator_fn_" + suffix
	execTest(t, ctx, client, fmt.Sprintf(
		"CREATE RULE %s AS ON INSERT TO %s DO NOTHING; "+
			"CREATE STATISTICS %s ON a, b FROM %s; "+
			"COMMENT ON TABLE %s IS 'review me'; "+
			"CREATE PUBLICATION %s FOR TABLE %s; "+
			"CREATE FUNCTION %s() RETURNS event_trigger LANGUAGE plpgsql AS $$BEGIN END$$; "+
			"CREATE EVENT TRIGGER %s ON ddl_command_start EXECUTE FUNCTION %s(); "+
			"CREATE COLLATION %s FROM \"C\"; "+
			"CREATE TYPE %s AS ENUM ('one'); "+
			"CREATE FUNCTION %s(%s) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT $1::text'; "+
			"CREATE CAST (%s AS text) WITH FUNCTION %s(%s) AS ASSIGNMENT; "+
			"CREATE FUNCTION %s(integer, integer) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT $1 = $2'; "+
			"CREATE OPERATOR public.=== (LEFTARG = integer, RIGHTARG = integer, FUNCTION = %s)",
		rule, table, statistic, table, table, publication, table,
		eventFunction, eventTrigger, eventFunction, collation, enumType,
		castFunction, enumType, enumType, castFunction, enumType,
		operatorFunction, operatorFunction))
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	for _, kind := range []string{
		"rule", "extended statistic", "comment", "publication", "event trigger",
		"collation", "cast", "operator",
	} {
		found := false
		for _, change := range summary.Changes.Schema {
			found = found || change.Kind == kind
		}
		if !found {
			t.Fatalf("catalog summary omitted %s: %#v", kind, summary.Changes.Schema)
		}
	}
}

func TestDeferredEffectsForceExplicitUnknownSummaryIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	parent, child := "unring_parent_"+suffix, "unring_child_"+suffix
	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE TABLE %s (id integer PRIMARY KEY); CREATE TABLE %s "+
			"(id integer PRIMARY KEY, parent_id integer REFERENCES %s(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED); "+
			"INSERT INTO %s VALUES (1); INSERT INTO %s SELECT n, 1 FROM generate_series(1, 5) n",
		parent, child, parent, parent, child))
	cleanupCompletenessSQL(t, config, fmt.Sprintf("DROP TABLE IF EXISTS %s, %s", child, parent))

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "SET CONSTRAINTS ALL DEFERRED")
	execTest(t, ctx, client, "DELETE FROM "+parent+" WHERE id = 1")
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if summary.Changes.Complete || !strings.Contains(summary.Changes.Error, "COMMIT") {
		t.Fatalf("deferred work was presented as complete: %#v", summary.Changes)
	}
}

func TestForeignTableWriteForcesExplicitUnknownSummaryIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	target, foreign, server := "unring_fdw_target_"+suffix, "unring_fdw_"+suffix, "unring_server_"+suffix
	mappingOptions := "user " + quoteSQLLiteral(config.User)
	if config.Password != "" {
		mappingOptions += ", password " + quoteSQLLiteral(config.Password)
	}
	execTest(t, ctx, direct, "CREATE EXTENSION IF NOT EXISTS postgres_fdw")
	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE TABLE %s (id integer); "+
			"CREATE SERVER %s FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host %s, port %s, dbname %s); "+
			"CREATE USER MAPPING FOR CURRENT_USER SERVER %s OPTIONS (%s); "+
			"CREATE FOREIGN TABLE %s (id integer) SERVER %s OPTIONS (schema_name 'public', table_name %s)",
		target, server, quoteSQLLiteral(config.Host), quoteSQLLiteral(strconv.Itoa(int(config.Port))),
		quoteSQLLiteral(config.Database), server, mappingOptions, foreign, server,
		quoteSQLLiteral(target)))
	cleanupCompletenessSQL(t, config, fmt.Sprintf(
		"DROP FOREIGN TABLE IF EXISTS %s; DROP SERVER IF EXISTS %s CASCADE; DROP TABLE IF EXISTS %s",
		foreign, server, target))

	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "INSERT INTO "+foreign+" VALUES (1)")
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if summary.Changes.Complete || !strings.Contains(summary.Changes.Error, "foreign table") ||
		!summary.HasReviewableActivity() {
		t.Fatalf("foreign write was silently counted as zero: %#v", summary)
	}
}

func TestServerSideCopyRequiresApprovalAndReviewIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	var approvals atomic.Int32
	proxy, err := StartWithOptions(ctx, config, Options{Approve: func(
		context.Context, ApprovalRequest,
	) (bool, error) {
		approvals.Add(1)
		return true, nil
	}})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "COPY (SELECT 1) TO PROGRAM 'cat >/dev/null'")
	_ = client.Close(ctx)
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal(): %v", err)
	}
	summary := proxy.Summary()
	if approvals.Load() != 1 || len(summary.IrreversibleActions) != 1 ||
		summary.FullyReversible || !summary.HasReviewableActivity() {
		t.Fatalf("server-side COPY escaped irreversible review: %#v", summary)
	}
}

func TestBroadMaintenanceConflictIsRefusedBeforePartialExecutionIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	table := "unring_broad_lock_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	execTest(t, ctx, direct, "CREATE TABLE "+table+" (id integer)")
	cleanupCompletenessSQL(t, config, "DROP TABLE IF EXISTS "+table)
	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(context.Context, ApprovalRequest) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "SELECT * FROM "+table)
	started := time.Now()
	_, commandErr := client.Exec(ctx, "VACUUM (FULL)").ReadAll()
	if commandErr == nil || !strings.Contains(commandErr.Error(), table) ||
		!strings.Contains(commandErr.Error(), "commit or discard") {
		t.Fatalf("broad maintenance error = %v, want actionable conflict on %s", commandErr, table)
	}
	if time.Since(started) >= time.Second {
		t.Fatalf("broad maintenance reached the timeout instead of preflight")
	}
	if summary := proxy.Summary(); !summary.FullyReversible ||
		len(summary.IrreversibleActions) != 0 {
		t.Fatalf("broad maintenance refusal changed reversibility summary: %#v", summary)
	}
}

func TestEscapeReservationPreventsLockPreflightRaceIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(context.Context, ApprovalRequest) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	proxyConfig := proxyTestConfig(t, proxy.Address(), config)
	first := connectTest(t, ctx, proxyConfig)
	defer first.Close(ctx)
	second := connectTest(t, ctx, proxyConfig)
	defer second.Close(ctx)

	done := make(chan error, 1)
	go func() {
		_, commandErr := first.Exec(ctx,
			"COPY (SELECT pg_sleep(1)) TO PROGRAM 'cat >/dev/null'").ReadAll()
		done <- commandErr
	}()
	deadline := time.Now().Add(3 * time.Second)
	reserved := false
	for time.Now().Before(deadline) {
		proxy.queryMu.Lock()
		reserved = proxy.escapeClient != 0
		proxy.queryMu.Unlock()
		if reserved {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !reserved {
		t.Fatal("approved escape statement never reserved the shared backend")
	}
	if _, err := second.Exec(ctx, "SELECT 1").ReadAll(); err == nil ||
		!strings.Contains(err.Error(), "55P03") || !strings.Contains(err.Error(), "cannot safely interleave") {
		t.Fatalf("statement raced between lock preflight and escape execution: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("slow approved COPY failed: %v", err)
	}
}
