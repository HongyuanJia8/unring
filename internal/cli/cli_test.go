package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
