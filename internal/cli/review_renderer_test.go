package cli

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := updated.(reviewModel).View()
	if lines := renderedLineCount(view); lines > 24 {
		t.Fatalf("review produced %d lines for a 24-row renderer:\n%s", lines, view)
	}
	visible := standardRendererVisible(view, 80, 24)

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
	assertDisclosureAndLegendFit(t, view, 80)
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
		NonTransactional: []pgproxy.NonTransactionalEffect{
			{Detail: "sequence advanced"},
		},
		Queries: queries,
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := updated.(reviewModel).View()
	if lines := renderedLineCount(view); lines > 24 {
		t.Fatalf("irreversible review produced %d lines for a 24-row renderer:\n%s", lines, view)
	}
	visible := standardRendererVisible(view, 80, 24)
	for _, want := range []string{
		"WARNING: THIS SESSION IS NOT FULLY REVERSIBLE",
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
		"raw-socket connections",
		"aws, docker, terraform, and kubectl",
		"Up/down: select  Enter/space: expand  c: commit  d: discard",
	} {
		if !strings.Contains(visible, want) {
			t.Fatalf("renderer-visible irreversible review missing %q:\n%s", want, visible)
		}
	}
	assertDisclosureAndLegendFit(t, view, 80)
	if strings.Contains(view, "review items below ...Up/down:") {
		t.Fatalf("review item count was fused to the key legend:\n%s", view)
	}
}

func assertDisclosureAndLegendFit(t *testing.T, view string, maximumWidth int) {
	t.Helper()
	inDisclosure := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "STRUCTURAL BLIND SPOTS") {
			inDisclosure = true
		} else if inDisclosure && line == "" {
			inDisclosure = false
		}
		if !inDisclosure && !strings.Contains(line, "Up/down:") &&
			!strings.Contains(line, "Decision:") {
			continue
		}
		if width := ansi.StringWidth(line); width > maximumWidth {
			t.Fatalf("%d-column renderer would truncate %d-column line %q",
				maximumWidth, width, line)
		}
	}
}

// Bubble Tea's standard renderer drops lines from the top when a View exceeds
// the terminal height and truncates lines at the terminal width. This mirrors
// that externally visible behavior.
func standardRendererVisible(view string, width, height int) string {
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, width, "")
	}
	return strings.Join(lines, "\n")
}
