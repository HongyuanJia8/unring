package cli

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hyj28/unring/internal/pgproxy"
)

func TestRendererKeepsStructuralDisclosureVisibleInPagedReview(t *testing.T) {
	queries := make([]pgproxy.QueryRecord, 40)
	for index := range queries {
		queries[index] = pgproxy.QueryRecord{SQL: fmt.Sprintf("SELECT %d", index)}
	}
	model := newReviewModel(pgproxy.Summary{
		Sealed: true, FullyReversible: true,
		Changes: pgproxy.ChangeSummary{Complete: true}, Queries: queries,
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	view := updated.(reviewModel).View()
	if lines := renderedLineCount(view); lines > 24 {
		t.Fatalf("review produced %d lines for a 24-row renderer:\n%s", lines, view)
	}
	visible := standardRendererVisibleTail(view, 24)

	for _, want := range []string{
		"UNRING SESSION REVIEW",
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
		"direct-to-IP and raw-socket connections",
		"aws, docker, terraform, and kubectl",
	} {
		if !strings.Contains(visible, want) {
			t.Fatalf("renderer-visible review missing %q:\n%s\n\nfull view:\n%s",
				want, visible, view)
		}
	}
	for _, line := range strings.Split(visible, "\n") {
		if strings.Contains(line, "git push") ||
			strings.Contains(line, "proxy or PATH") ||
			strings.Contains(line, "Unshimmed macOS") {
			if len(line) > 120 {
				t.Fatalf("120-column renderer would truncate disclosure line %q", line)
			}
		}
	}
}

func TestRendererKeepsIrreversibilityAndBlindSpotWarningsTogether(t *testing.T) {
	queries := make([]pgproxy.QueryRecord, 40)
	for index := range queries {
		queries[index] = pgproxy.QueryRecord{SQL: fmt.Sprintf("UPDATE example SET n = %d", index)}
	}
	model := newReviewModel(pgproxy.Summary{
		Sealed:          true,
		FullyReversible: false,
		Changes:         pgproxy.ChangeSummary{Complete: true},
		Queries:         queries,
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	view := updated.(reviewModel).View()
	if lines := renderedLineCount(view); lines > 24 {
		t.Fatalf("irreversible review produced %d lines for a 24-row renderer:\n%s", lines, view)
	}
	visible := standardRendererVisibleTail(view, 24)
	for _, want := range []string{
		"WARNING: THIS SESSION IS NOT FULLY REVERSIBLE",
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
	} {
		if !strings.Contains(visible, want) {
			t.Fatalf("renderer-visible irreversible review missing %q:\n%s", want, visible)
		}
	}
}

// Bubble Tea's standard renderer drops lines from the top when a View exceeds
// the terminal height. This mirrors that externally visible behavior.
func standardRendererVisibleTail(view string, height int) string {
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return strings.Join(lines, "\n")
}
