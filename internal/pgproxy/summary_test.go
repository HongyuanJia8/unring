package pgproxy

import (
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func TestCatalogSnapshotCoversReviewableDDLClasses(t *testing.T) {
	t.Parallel()
	for _, version := range []int{140000, 150000, 160000, 170000} {
		if _, err := pg_query.Parse(catalogSnapshotSQL(version)); err != nil {
			t.Fatalf("PostgreSQL %d catalog snapshot SQL does not parse: %v", version, err)
		}
	}
	query := catalogSnapshotSQL(minimumPostgresVersion)
	for _, catalog := range []string{
		"pg_rewrite", "pg_statistic_ext", "pg_event_trigger", "pg_publication",
		"pg_subscription", "pg_description", "pg_cast", "pg_operator", "pg_collation",
		"pg_shdescription",
	} {
		if !strings.Contains(query, catalog) {
			t.Errorf("catalog snapshot omits %s", catalog)
		}
	}
	if strings.Contains(query, "pg_publication_namespace") {
		t.Fatal("PostgreSQL 14 catalog snapshot references a PostgreSQL 15 catalog")
	}
	if !strings.Contains(catalogSnapshotSQL(150000), "pg_publication_namespace") {
		t.Fatal("PostgreSQL 15 catalog snapshot omits schema publications")
	}
	for _, unsupportedColumn := range []string{
		"stxstattarget", "subskiplsn", "subfailover", "colliculocale", "colllocale",
	} {
		if strings.Contains(query, unsupportedColumn) {
			t.Errorf("PostgreSQL 14 catalog snapshot references version-specific column %s",
				unsupportedColumn)
		}
	}
}

func TestPostgresMinimumVersionErrorIsActionable(t *testing.T) {
	t.Parallel()
	if err := validatePostgresVersion(140000, "14.0"); err != nil {
		t.Fatalf("PostgreSQL 14 rejected: %v", err)
	}
	err := validatePostgresVersion(130016, "13.16")
	if err == nil || !strings.Contains(err.Error(), "requires PostgreSQL 14 or newer") ||
		!strings.Contains(err.Error(), "PostgreSQL 13.16") {
		t.Fatalf("old-server error is not actionable: %v", err)
	}
}

func TestSummaryReversibilityTracksEscapeActionsOnly(t *testing.T) {
	t.Parallel()
	proxy := &Proxy{
		changes: ChangeSummary{Complete: false, Error: "unknown staged effect"},
		unintercepted: []UninterceptedItem{{
			Detail: "unclassified traffic",
		}},
		sequenceEffects: map[string]struct{}{"public.ids": {}},
	}
	summary := proxy.Summary()
	if !summary.FullyReversible || len(summary.IrreversibleActions) != 0 {
		t.Fatalf("non-escape warnings counterfeited reversibility stamp: %#v", summary)
	}

	proxy.irreversibleActions = []IrreversibleAction{{SQL: "VACUUM"}}
	summary = proxy.Summary()
	if summary.FullyReversible || len(summary.IrreversibleActions) != 1 {
		t.Fatalf("recorded escape action did not change reversibility stamp: %#v", summary)
	}
}

func TestSummaryRiskRequiresAnEffectWhenPostgresReportsRowCounts(t *testing.T) {
	t.Parallel()
	statement := clientStatement{
		SummaryRisk: "unmeasurable target", RiskRequiresRows: true,
	}
	if summaryRiskApplies(statement, []string{"UPDATE 0"}) {
		t.Fatal("zero-row statement applied an explicit-unknown effect")
	}
	if !summaryRiskApplies(statement, []string{"UPDATE 1"}) {
		t.Fatal("nonzero statement did not apply its explicit-unknown effect")
	}
	statement.RiskRequiresRows = false
	if !summaryRiskApplies(statement, []string{"TRUNCATE TABLE"}) {
		t.Fatal("success-only uncountable statement lost its explicit-unknown effect")
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
