package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/HongyuanJia8/unring/internal/audit"
	"github.com/HongyuanJia8/unring/internal/httpsproxy"
	"github.com/HongyuanJia8/unring/internal/pgproxy"
)

func TestMainHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Main([]string{"help"}, strings.NewReader(""), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Main(help) exit code = %d, want 0; stderr: %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unring run") {
		t.Fatalf("help output did not mention run: %s", stdout.String())
	}
}

func TestReviewModelExpandsStatementDetails(t *testing.T) {
	t.Parallel()

	model := newReviewModel(pgproxy.Summary{
		Sealed:          true,
		FullyReversible: true,
		Changes:         pgproxy.ChangeSummary{Complete: true},
		Queries: []pgproxy.QueryRecord{{
			SQL: "UPDATE example\nSET value = 'changed'", CommandTags: []string{"UPDATE 2"},
			Failed: true, Error: "constraint failed (SQLSTATE 23514)",
		}},
	})
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	view := updated.(reviewModel).View()
	for _, want := range []string{
		"Statement:", "UPDATE example", "SET value = 'changed'",
		"Rows affected: update 2", "Error: constraint failed (SQLSTATE 23514)",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded review missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "\x1b") {
		t.Fatalf("model emitted styling despite using plain terminal capabilities: %q", view)
	}
}

func TestReviewModelSeparatesUninterceptedTraffic(t *testing.T) {
	t.Parallel()

	view := newReviewModel(pgproxy.Summary{
		Sealed:          true,
		FullyReversible: true,
		Changes:         pgproxy.ChangeSummary{Complete: true},
		Queries:         []pgproxy.QueryRecord{{SQL: "SELECT 1"}},
		Unintercepted: []pgproxy.UninterceptedItem{{
			Statement: "mystery", Detail: "could not classify this traffic",
		}},
	}).View()
	statementSection := strings.Index(view, "STATEMENTS")
	uninterceptedSection := strings.Index(view, "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!")
	warning := strings.Index(view, "INTERCEPTION/COVERAGE WARNING")
	if statementSection < 0 || uninterceptedSection < 0 || warning < 0 ||
		warning > statementSection || !strings.Contains(view, "mystery") {
		t.Fatalf("unintercepted traffic was not rendered in its own section:\n%s", view)
	}
}

func TestReviewModelKeepsUninterceptedWarningVisibleWhenSectionIsOffscreen(t *testing.T) {
	t.Parallel()

	queries := make([]pgproxy.QueryRecord, 40)
	for index := range queries {
		queries[index] = pgproxy.QueryRecord{SQL: fmt.Sprintf("SELECT %d", index)}
	}
	model := newReviewModel(pgproxy.Summary{
		Sealed: true, FullyReversible: true,
		Changes: pgproxy.ChangeSummary{Complete: true}, Queries: queries,
		Unintercepted: []pgproxy.UninterceptedItem{{Detail: "could not classify one batch"}},
	})
	model.offset = 20
	view := model.View()
	if !strings.Contains(view, "INTERCEPTION/COVERAGE WARNING") ||
		!strings.Contains(view, "1 UNCLASSIFIED ITEM") {
		t.Fatalf("off-screen unclassified traffic lost its persistent warning:\n%s", view)
	}
}

func TestReviewReportsForwardedAndUninterceptedHTTPSSeparately(t *testing.T) {
	t.Parallel()
	model := newReviewModelWithHTTPS(pgproxy.Summary{
		Sealed: true, FullyReversible: true,
		Changes: pgproxy.ChangeSummary{Complete: true},
	}, httpsproxy.Summary{
		Sealed: true,
		Requests: []httpsproxy.RequestRecord{{
			Method: "POST", URL: "https://api.example.test/messages", StatusCode: 201,
		}},
		Unintercepted: []httpsproxy.UninterceptedItem{{
			Host:   "api.passthrough.test:443",
			Detail: "CONNECT tunnel was passed through without TLS interception",
		}},
	})
	view := model.View()
	for _, want := range []string{
		"WARNING: THIS SESSION IS NOT FULLY REVERSIBLE",
		"HTTPS REQUESTS — ALREADY FORWARDED",
		"POST https://api.example.test/messages",
		"INTERCEPTION/COVERAGE WARNING",
		"api.passthrough.test:443",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("HTTPS review missing %q:\n%s", want, view)
		}
	}
}

func TestLogCommandListsAndShowsStructuredSession(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatalf("OpenStore() error: %v", err)
	}
	session, err := store.Begin([]string{"agent", "--fix"}, time.Now())
	if err != nil {
		t.Fatalf("Begin() error: %v", err)
	}
	err = session.Update(func(record *audit.Record) {
		record.EndedAt = time.Now().UTC()
		record.Decision = "discard"
		record.Outcome = "discarded"
		record.Postgres = pgproxy.Summary{
			Sealed: true, FullyReversible: true,
			Changes: pgproxy.ChangeSummary{
				Complete: true,
				Rows:     []pgproxy.RowChange{{Table: "public.items", Updated: 3}},
			},
		}
		record.HTTPS = httpsproxy.Summary{
			Sealed: true,
			Requests: []httpsproxy.RequestRecord{{
				Method: "POST", URL: "https://api.example.test/events", StatusCode: 202,
			}},
			Unintercepted: []httpsproxy.UninterceptedItem{{
				Host:   "go-client.example.test:443",
				Detail: "TLS handshake failed; the client may not trust unring's per-process CA",
			}},
		}
	})
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	id := session.Snapshot().ID

	var list bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := Main([]string{"log"}, strings.NewReader(""), &list, &stderr); exitCode != 0 {
		t.Fatalf("log list exit = %d; stderr: %s", exitCode, stderr.String())
	}
	if !strings.Contains(list.String(), id) || !strings.Contains(list.String(), "agent --fix") {
		t.Fatalf("log list omitted session:\n%s", list.String())
	}

	var detail bytes.Buffer
	if exitCode := Main(
		[]string{"log", id[:20]}, strings.NewReader(""), &detail, &stderr,
	); exitCode != 0 {
		t.Fatalf("log detail exit = %d; stderr: %s", exitCode, stderr.String())
	}
	if !strings.Contains(detail.String(), "public.items: 0 inserted, 3 updated") ||
		!strings.Contains(detail.String(), "Outcome:  discarded") ||
		!strings.Contains(detail.String(), "POST https://api.example.test/events") ||
		!strings.Contains(detail.String(), "go-client.example.test:443") {
		t.Fatalf("log detail omitted structured changes:\n%s", detail.String())
	}
}

func TestLogCommandListsGoodSessionsAlongsideCorruptRecordWarning(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatalf("OpenStore() error: %v", err)
	}
	session, err := store.Begin([]string{"agent", "--recover-history"}, time.Now())
	if err != nil {
		t.Fatalf("Begin() error: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(stateDir, "logs", "copied-from-the-future.json"),
		[]byte(`{"version":999,"id":"future"}`),
		0o600,
	); err != nil {
		t.Fatalf("write incompatible audit record: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Main([]string{"log"}, strings.NewReader(""), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("log list exit = %d, want 0; stderr: %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), session.Snapshot().ID) {
		t.Fatalf("log list lost readable history:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning") ||
		!strings.Contains(stderr.String(), "copied-from-the-future.json") {
		t.Fatalf("log list did not report skipped record:\n%s", stderr.String())
	}
}

func TestPromptDefaultsToRollbackWithoutTerminal(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	decision := promptDecision(strings.NewReader("commit\n"), &output)
	if decision != pgproxy.DecisionRollback {
		t.Fatalf("promptDecision() = %q, want rollback", decision)
	}
	if !strings.Contains(output.String(), "--commit") {
		t.Fatalf("non-interactive guidance missing: %s", output.String())
	}
}

func TestSummaryWarnsWhenSessionIsNotFullyReversible(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	printSummary(&output, pgproxy.Summary{
		FullyReversible: false,
		IrreversibleActions: []pgproxy.IrreversibleAction{
			{SQL: "VACUUM"},
		},
	})
	text := output.String()
	if !strings.Contains(text, "NOT FULLY REVERSIBLE") ||
		!strings.Contains(text, "VACUUM") ||
		!strings.Contains(text, "discard cannot undo") {
		t.Fatalf("irreversible summary warning missing:\n%s", text)
	}
}

func TestIrreversibleApprovalDoesNotReadAheadPastItsLine(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("yes\nchild-input\n")
	line, err := readOnePromptLine(input)
	if err != nil || line != "yes\n" {
		t.Fatalf("approval line = %q, %v", line, err)
	}
	remaining, err := io.ReadAll(input)
	if err != nil {
		t.Fatalf("read remaining prompt input: %v", err)
	}
	if string(remaining) != "child-input\n" {
		t.Fatalf("approval swallowed later terminal input: %q", remaining)
	}
}

func TestIrreversibleApprovalDefaultsToDeclineWithoutTerminal(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	approved := promptIrreversibleApproval(strings.NewReader("yes\n"), &output,
		pgproxy.ApprovalRequest{SQL: "VACUUM", Reason: "outside a transaction"})
	if approved {
		t.Fatal("non-interactive irreversible approval unexpectedly succeeded")
	}
	if !strings.Contains(output.String(), "cannot be undone by discard") ||
		!strings.Contains(output.String(), "declining") {
		t.Fatalf("approval warning missing:\n%s", output.String())
	}
}
