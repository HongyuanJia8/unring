package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/httpsproxy"
	"github.com/hyj28/unring/internal/pgproxy"
)

func TestStorePersistsStructuredSessionAndLoadsByPrefix(t *testing.T) {
	store, err := OpenStoreAt(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("OpenStoreAt() error: %v", err)
	}
	start := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	session, err := store.Begin([]string{"agent", "--task"}, start)
	if err != nil {
		t.Fatalf("Begin() error: %v", err)
	}
	err = session.Update(func(record *Record) {
		record.EndedAt = start.Add(time.Minute)
		record.Decision = "commit"
		record.Outcome = "committed"
		record.Postgres = pgproxy.Summary{
			Sealed: true, FullyReversible: false,
			Changes: pgproxy.ChangeSummary{
				Complete: true,
				Rows: []pgproxy.RowChange{{
					Table: "public.accounts", Inserted: 2, Updated: 1,
				}},
				Schema: []pgproxy.SchemaChange{{
					Action: "created", Kind: "table", Object: "public.audit_events",
				}},
			},
			IrreversibleActions: []pgproxy.IrreversibleAction{{SQL: "VACUUM"}},
		}
		record.Approvals = []Approval{
			{Kind: "postgres", Statement: "VACUUM", Decision: "approved", Time: start},
			{Kind: "postgres", Statement: "CHECKPOINT", Decision: "declined", Time: start},
		}
		record.HTTPS = httpsproxy.Summary{
			Sealed: true,
			Requests: []httpsproxy.RequestRecord{{
				Method: "POST", URL: "https://api.example.test/events", StatusCode: 201,
			}},
		}
		record.Unintercepted = []Unintercepted{{
			Kind: "https", Host: "api.passthrough.test:443", Detail: "CONNECT passed through",
		}}
	})
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	snapshot := session.Snapshot()
	loaded, err := store.Load(snapshot.ID[:20])
	if err != nil {
		t.Fatalf("Load(prefix) error: %v", err)
	}
	if loaded.Outcome != "committed" ||
		len(loaded.Postgres.Changes.Rows) != 1 ||
		len(loaded.Postgres.Changes.Schema) != 1 ||
		len(loaded.Approvals) != 2 ||
		len(loaded.HTTPS.Requests) != 1 ||
		len(loaded.Unintercepted) != 1 {
		t.Fatalf("loaded record omitted structured activity: %#v", loaded)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].ID != loaded.ID {
		t.Fatalf("List() = %#v, %v", records, err)
	}

	path := filepath.Join(store.logDir, loaded.ID+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat audit record: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("audit record mode = %04o, want 0600", got)
	}
}

func TestListSkipsAndReportsUnreadableAndUnsupportedRecords(t *testing.T) {
	store, err := OpenStoreAt(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("OpenStoreAt() error: %v", err)
	}
	session, err := store.Begin([]string{"agent"}, time.Now())
	if err != nil {
		t.Fatalf("Begin() error: %v", err)
	}
	validID := session.Snapshot().ID
	malformedPath := filepath.Join(store.logDir, "malformed.json")
	if err := os.WriteFile(malformedPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write malformed record: %v", err)
	}
	unsupportedPath := filepath.Join(store.logDir, "future.json")
	if err := os.WriteFile(unsupportedPath, []byte(`{"version":99,"id":"future"}`), 0o600); err != nil {
		t.Fatalf("write unsupported record: %v", err)
	}

	records, listErr := store.List()
	if len(records) != 1 || records[0].ID != validID {
		t.Fatalf("List() readable records = %#v, want only %q", records, validID)
	}
	if listErr == nil ||
		!strings.Contains(listErr.Error(), "malformed.json") ||
		!strings.Contains(listErr.Error(), "unsupported version 99") {
		t.Fatalf("List() warning = %v, want both skipped record errors", listErr)
	}
	if _, err := store.Load("malformed"); err == nil {
		t.Fatal("Load(malformed) unexpectedly hid the exact record error")
	}
}
