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
)

func TestOpenClientTransactionDoesNotPinBackendIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	defer proxy.Close()
	proxyConfig := proxyTestConfig(t, proxy.Address(), config)
	first := connectTest(t, ctx, proxyConfig)
	defer first.Close(ctx)
	second := connectTest(t, ctx, proxyConfig)
	defer second.Close(ctx)

	execTest(t, ctx, first, "BEGIN")
	queryDone := make(chan error, 1)
	go func() {
		queryContext, queryCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer queryCancel()
		results, queryErr := second.Exec(queryContext, "SELECT 42").ReadAll()
		if queryErr == nil &&
			(len(results) != 1 || len(results[0].Rows) != 1 ||
				string(results[0].Rows[0][0]) != "42") {
			queryErr = fmt.Errorf("unexpected SELECT result: %#v", results)
		}
		queryDone <- queryErr
	}()
	select {
	case queryErr := <-queryDone:
		if queryErr != nil {
			t.Fatalf("second client query while first transaction remained open: %v", queryErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("open transaction pinned the shared backend")
	}
	if _, err := second.Exec(ctx, "CREATE TEMP TABLE unsafe_interleave (id integer)").ReadAll(); err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("unsafe interleaved write error = %v, want immediate SQLSTATE 55P03", err)
	}
	execTest(t, ctx, first, "ROLLBACK")
}

func TestLockConflictingIrreversibleStatementIsExplainedImmediatelyIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	table := "unring_escape_lock_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	index := table + "_idx"
	execTest(t, ctx, direct, fmt.Sprintf("CREATE TABLE %s (id integer)", table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup,
			fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", index))
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
	execTest(t, ctx, client, fmt.Sprintf("INSERT INTO %s VALUES (1)", table))

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		commandContext, commandCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer commandCancel()
		_, commandErr := client.Exec(commandContext, fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY %s ON %s (id)", index, table)).ReadAll()
		done <- commandErr
	}()
	select {
	case commandErr := <-done:
		if commandErr == nil {
			t.Fatal("lock-conflicting concurrent index unexpectedly succeeded")
		}
		if !strings.Contains(commandErr.Error(), table) ||
			!strings.Contains(commandErr.Error(), "commit or discard") ||
			!strings.Contains(commandErr.Error(), "holds PostgreSQL lock") {
			t.Fatalf("concurrent index error = %v, want table-specific lock explanation", commandErr)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("lock conflict took %s to refuse; preflight did not avoid the timeout", elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("approved irreversible statement deadlocked")
	}
	if got := scalarTest(t, ctx, client, "SELECT 43"); got != "43" {
		t.Fatalf("proxy did not recover after bounded escape failure: %s", got)
	}
	if summary := proxy.Summary(); !summary.FullyReversible ||
		len(summary.IrreversibleActions) != 0 {
		t.Fatalf("lock-conflict refusal changed reversibility summary: %#v", summary)
	}
}

func TestEscapeConnectionMirrorsSearchPathIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	schema := "unring_escape_schema_" + suffix
	table := "target_" + suffix
	execTest(t, ctx, direct, fmt.Sprintf(
		"CREATE SCHEMA %s; "+
			"CREATE TABLE %s.%s (id integer) WITH (autovacuum_enabled = false); "+
			"CREATE TABLE public.%s (id integer) WITH (autovacuum_enabled = false); "+
			"INSERT INTO %s.%s SELECT generate_series(1, 7); "+
			"INSERT INTO public.%s SELECT generate_series(1, 11)",
		schema, schema, table, table, schema, table, table))
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup := connectTest(t, cleanupContext, config)
		defer cleanup.Close(cleanupContext)
		execTest(t, cleanupContext, cleanup,
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		execTest(t, cleanupContext, cleanup,
			fmt.Sprintf("DROP TABLE IF EXISTS public.%s", table))
	})

	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(context.Context, ApprovalRequest) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "SET search_path = "+schema)
	execTest(t, ctx, client, fmt.Sprintf("VACUUM (ANALYZE) %s", table))
	if got := scalarTest(t, ctx, direct, fmt.Sprintf(
		"SELECT reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE n.nspname = '%s' AND c.relname = '%s'", schema, table)); got != "7" {
		t.Fatalf("escape VACUUM did not analyze the search_path table; reltuples = %s", got)
	}
	if got := scalarTest(t, ctx, direct, fmt.Sprintf(
		"SELECT reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE n.nspname = 'public' AND c.relname = '%s'", table)); got != "-1" {
		t.Fatalf("escape VACUUM analyzed public table instead of mirrored search_path; reltuples = %s", got)
	}
}

func TestDiscardAllIsRefusedWithoutIrreversibilityStampIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(context.Context, ApprovalRequest) (bool, error) {
			t.Fatal("DISCARD ALL unexpectedly requested irreversible approval")
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	execTest(t, ctx, client, "SET search_path = pg_catalog")
	if _, err := client.Exec(ctx, "DISCARD ALL").ReadAll(); err == nil ||
		!strings.Contains(err.Error(), "cannot emulate DISCARD ALL") {
		t.Fatalf("DISCARD ALL error = %v, want explicit refusal", err)
	}
	if got := scalarTest(t, ctx, client, "SHOW search_path"); got != "pg_catalog" {
		t.Fatalf("DISCARD ALL changed shared session state to %q", got)
	}
	if summary := proxy.Summary(); !summary.FullyReversible ||
		len(summary.IrreversibleActions) != 0 {
		t.Fatalf("refused DISCARD ALL changed reversibility summary: %#v", summary)
	}
}

func TestExtendedTransactionBlockErrorUsesEscapeHatchIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	var approvals atomic.Int32
	proxy, err := StartWithOptions(ctx, config, Options{
		Approve: func(_ context.Context, request ApprovalRequest) (bool, error) {
			if request.SQL != "CHECKPOINT" {
				t.Errorf("approval SQL = %q, want CHECKPOINT", request.SQL)
			}
			approvals.Add(1)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("StartWithOptions(): %v", err)
	}
	defer proxy.Close()
	client := connectPGXTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
	defer client.Close(ctx)
	if _, err := client.Prepare(ctx, "checkpoint_escape", "CHECKPOINT"); err != nil {
		t.Fatalf("prepare CHECKPOINT: %v", err)
	}
	if _, err := client.Exec(ctx, "checkpoint_escape"); err != nil {
		t.Fatalf("extended CHECKPOINT escape hatch: %v", err)
	}
	if approvals.Load() != 1 {
		t.Fatalf("extended CHECKPOINT approvals = %d, want 1", approvals.Load())
	}
}
