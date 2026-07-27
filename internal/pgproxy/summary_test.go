package pgproxy

import (
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func TestCatalogSnapshotCoversReviewableDDLClasses(t *testing.T) {
	t.Parallel()
	if _, err := pg_query.Parse(catalogSnapshotSQL); err != nil {
		t.Fatalf("catalog snapshot SQL does not parse: %v", err)
	}
	for _, catalog := range []string{
		"pg_rewrite", "pg_statistic_ext", "pg_event_trigger", "pg_publication",
		"pg_subscription", "pg_description", "pg_cast", "pg_operator", "pg_collation",
		"pg_shdescription",
	} {
		if !strings.Contains(catalogSnapshotSQL, catalog) {
			t.Errorf("catalog snapshot omits %s", catalog)
		}
	}
}

func TestRowLedgerAttributesTriggersAndRestoresSavepoints(t *testing.T) {
	const (
		sourceOID = 101
		auditOID  = 202
	)
	proxy := &Proxy{
		rowStats: rowStatsSnapshot{
			sourceOID: {Table: "public.source"},
			auditOID:  {Table: "public.audit"},
		},
		rowLedger: make(rowLedgerSnapshot),
	}

	proxy.applyRowStatsLocked(rowStatsSnapshot{
		sourceOID: {Table: "public.source", Inserted: 1},
		auditOID:  {Table: "public.audit", Inserted: 1},
	}, true)
	beforeSavepoint := cloneRowLedger(proxy.rowLedger)

	proxy.applyRowStatsLocked(rowStatsSnapshot{
		sourceOID: {Table: "public.source", Inserted: 2},
		auditOID:  {Table: "public.audit", Inserted: 2},
	}, true)
	proxy.restoreRowLedgerLocked(beforeSavepoint)
	// PostgreSQL retains attempted row counts after a subtransaction abort.
	// Resynchronizing without applying the delta keeps them out of our ledger.
	proxy.applyRowStatsLocked(rowStatsSnapshot{
		sourceOID: {Table: "public.source", Inserted: 2},
		auditOID:  {Table: "public.audit", Inserted: 2},
	}, false)

	want := map[string]RowChange{
		"public.source": {Table: "public.source", Inserted: 1},
		"public.audit":  {Table: "public.audit", Inserted: 1},
	}
	changes := proxy.rowChangesLocked()
	if len(changes) != len(want) {
		t.Fatalf("row changes = %#v, want %#v", changes, want)
	}
	for _, change := range changes {
		if expected, ok := want[change.Table]; !ok || change != expected {
			t.Fatalf("row change = %#v, want one of %#v", change, want)
		}
	}
}

func TestRowLedgerContinuesAfterRolledBackAttempt(t *testing.T) {
	const tableOID = 303
	proxy := &Proxy{
		rowStats:  rowStatsSnapshot{tableOID: {Table: "public.items"}},
		rowLedger: make(rowLedgerSnapshot),
	}

	proxy.applyRowStatsLocked(rowStatsSnapshot{
		tableOID: {Table: "public.items", Updated: 1},
	}, true)
	checkpoint := cloneRowLedger(proxy.rowLedger)
	proxy.applyRowStatsLocked(rowStatsSnapshot{
		tableOID: {Table: "public.items", Updated: 2},
	}, true)
	proxy.restoreRowLedgerLocked(checkpoint)
	proxy.applyRowStatsLocked(rowStatsSnapshot{
		tableOID: {Table: "public.items", Updated: 2},
	}, false)
	proxy.applyRowStatsLocked(rowStatsSnapshot{
		tableOID: {Table: "public.items", Updated: 3},
	}, true)

	changes := proxy.rowChangesLocked()
	if len(changes) != 1 || changes[0].Updated != 2 {
		t.Fatalf("row changes after rollback and later write = %#v, want 2 staged updates", changes)
	}
}

func TestRowLedgerDoesNotSilentlyAcceptCounterReset(t *testing.T) {
	const tableOID = 404
	proxy := &Proxy{
		rowStats: rowStatsSnapshot{
			tableOID: {Table: "public.items", Deleted: 3},
		},
		rowLedger: make(rowLedgerSnapshot),
	}
	proxy.applyRowStatsLocked(rowStatsSnapshot{
		tableOID: {Table: "public.items"},
	}, true)
	if proxy.rowLedgerErr == nil {
		t.Fatal("counter reset did not make the change summary incomplete")
	}
}
