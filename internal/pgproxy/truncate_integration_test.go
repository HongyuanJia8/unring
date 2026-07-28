package pgproxy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	testpostgres "github.com/hyj28/unring/internal/testsupport/postgres"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTruncateCanCommitOrDiscardWithExactCountsIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)

	for _, test := range []struct {
		name     string
		decision Decision
		wantRows string
	}{
		{name: "commit", decision: DecisionCommit, wantRows: "0"},
		{name: "discard", decision: DecisionRollback, wantRows: "3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := "unring_truncate_" + test.name + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			execTest(t, ctx, direct, fmt.Sprintf(
				"CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1), (2), (3)", table, table))
			cleanupCompletenessSQL(t, config, "DROP TABLE IF EXISTS "+table)

			proxy := startTruncateTestProxy(t, ctx, config)
			client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
			execTest(t, ctx, client, "TRUNCATE "+table)
			_ = client.Close(ctx)
			if err := proxy.Seal(ctx); err != nil {
				t.Fatalf("Seal(): %v", err)
			}
			assertTruncateRows(t, proxy.Summary(), map[string]int64{"public." + table: 3})
			if err := proxy.Finalize(ctx, test.decision); err != nil {
				t.Fatalf("Finalize(%s): %v", test.decision, err)
			}
			if got := scalarTest(t, ctx, direct, "SELECT count(*) FROM "+table); got != test.wantRows {
				t.Fatalf("rows after %s = %s, want %s", test.decision, got, test.wantRows)
			}
		})
	}
}

func TestTruncateMultiCascadeAndPartitionsReportPerTableIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	a, b := "unring_truncate_a_"+suffix, "unring_truncate_b_"+suffix
	parent, child := "unring_truncate_parent_"+suffix, "unring_truncate_child_"+suffix
	partitioned := "unring_truncate_part_" + suffix
	p1, p2 := partitioned+"_one", partitioned+"_two"
	execTest(t, ctx, direct, fmt.Sprintf(`
CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1), (2);
CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1), (2), (3);
CREATE TABLE %s (id integer PRIMARY KEY); INSERT INTO %s VALUES (1), (2);
CREATE TABLE %s (id integer REFERENCES %s); INSERT INTO %s VALUES (1), (1), (2), (2);
CREATE TABLE %s (id integer) PARTITION BY RANGE (id);
CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (0) TO (10);
CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (10) TO (20);
INSERT INTO %s VALUES (1), (2), (11), (12), (13)`,
		a, a, b, b, parent, parent, child, parent, child,
		partitioned, p1, partitioned, p2, partitioned, partitioned))
	cleanupCompletenessSQL(t, config, fmt.Sprintf(
		"DROP TABLE IF EXISTS %s, %s, %s, %s, %s CASCADE", child, parent, partitioned, a, b))

	tests := []struct {
		name string
		sql  string
		want map[string]int64
	}{
		{
			name: "multiple targets", sql: fmt.Sprintf("TRUNCATE %s, %s", a, b),
			want: map[string]int64{"public." + a: 2, "public." + b: 3},
		},
		{
			name: "foreign-key cascade", sql: "TRUNCATE " + parent + " CASCADE",
			want: map[string]int64{"public." + parent: 2, "public." + child: 4},
		},
		{
			name: "partition leaves", sql: "TRUNCATE " + partitioned,
			want: map[string]int64{"public." + p1: 2, "public." + p2: 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := startTruncateTestProxy(t, ctx, config)
			client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
			execTest(t, ctx, client, test.sql)
			_ = client.Close(ctx)
			if err := proxy.Seal(ctx); err != nil {
				t.Fatalf("Seal(): %v", err)
			}
			assertTruncateRows(t, proxy.Summary(), test.want)
			if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
				t.Fatalf("discard: %v", err)
			}
		})
	}
}

func TestTruncateUnknownFallbackAndExtendedProtocolIntegration(t *testing.T) {
	connectionString := testpostgres.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	config := parseTestConfig(t, connectionString)
	direct := connectTest(t, ctx, config)
	defer direct.Close(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	unknownTable := "unring_truncate_trigger_" + suffix
	extendedTable := "unring_truncate_extended_" + suffix
	function := "unring_truncate_trigger_fn_" + suffix
	execTest(t, ctx, direct, fmt.Sprintf(`
CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1), (2);
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER count_blocker BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION %s();
CREATE TABLE %s (id integer); INSERT INTO %s VALUES (1), (2), (3), (4)`,
		unknownTable, unknownTable, function, unknownTable, function, extendedTable, extendedTable))
	cleanupCompletenessSQL(t, config, fmt.Sprintf(
		"DROP TABLE IF EXISTS %s, %s CASCADE; DROP FUNCTION IF EXISTS %s()",
		unknownTable, extendedTable, function))

	t.Run("enabled truncate trigger remains unknown", func(t *testing.T) {
		proxy := startTruncateTestProxy(t, ctx, config)
		client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
		execTest(t, ctx, client, "TRUNCATE "+unknownTable)
		_ = client.Close(ctx)
		if err := proxy.Seal(ctx); err != nil {
			t.Fatalf("Seal(): %v", err)
		}
		summary := proxy.Summary()
		if summary.Changes.Complete || !strings.Contains(summary.Changes.Error, "ON TRUNCATE trigger") {
			t.Fatalf("trigger-backed TRUNCATE summary = %#v", summary.Changes)
		}
		if err := proxy.Finalize(ctx, DecisionCommit); err == nil {
			t.Fatal("commit accepted an explicitly unknown TRUNCATE count")
		}
		if got := scalarTest(t, ctx, direct, "SELECT count(*) FROM "+unknownTable); got != "2" {
			t.Fatalf("forced discard left %s rows, want 2", got)
		}
	})

	t.Run("extended protocol uses exact path", func(t *testing.T) {
		proxy := startTruncateTestProxy(t, ctx, config)
		client := connectTest(t, ctx, proxyTestConfig(t, proxy.Address(), config))
		result := client.ExecParams(ctx, "TRUNCATE "+extendedTable, nil, nil, nil, nil).Read()
		if result.Err != nil {
			t.Fatalf("extended TRUNCATE: %v", result.Err)
		}
		_ = client.Close(ctx)
		if err := proxy.Seal(ctx); err != nil {
			t.Fatalf("Seal(): %v", err)
		}
		assertTruncateRows(t, proxy.Summary(), map[string]int64{"public." + extendedTable: 4})
		if err := proxy.Finalize(ctx, DecisionRollback); err != nil {
			t.Fatalf("discard: %v", err)
		}
	})
}

func startTruncateTestProxy(t *testing.T, ctx context.Context, config *pgconn.Config) *Proxy {
	t.Helper()
	proxy, err := Start(ctx, config)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

func assertTruncateRows(t *testing.T, summary Summary, want map[string]int64) {
	t.Helper()
	if !summary.Changes.Complete {
		t.Fatalf("TRUNCATE summary is UNKNOWN: %s", summary.Changes.Error)
	}
	got := make(map[string]int64)
	for _, change := range summary.Changes.Rows {
		if change.Deleted != 0 {
			got[change.Table] = change.Deleted
		}
	}
	if len(got) != len(want) {
		t.Fatalf("TRUNCATE changes = %#v, want %#v", summary.Changes.Rows, want)
	}
	for table, count := range want {
		if got[table] != count {
			t.Fatalf("TRUNCATE deleted %d from %s, want %d; all changes %#v",
				got[table], table, count, summary.Changes.Rows)
		}
	}
}
