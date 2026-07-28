package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/hyj28/unring/internal/httpsproxy"
	"github.com/hyj28/unring/internal/pgproxy"
)

type reviewItem struct {
	section  string
	title    string
	SQL      string
	affected string
	err      string
	detail   string
}

type reviewModel struct {
	summary  pgproxy.Summary
	https    httpsproxy.Summary
	items    []reviewItem
	expanded map[int]bool
	cursor   int
	offset   int
	height   int
	decision pgproxy.Decision
	decided  bool
}

func newReviewModel(summary pgproxy.Summary) reviewModel {
	return newReviewModelWithHTTPS(summary, httpsproxy.Summary{Sealed: true})
}

func newReviewModelWithHTTPS(summary pgproxy.Summary, httpsSummary httpsproxy.Summary) reviewModel {
	model := reviewModel{
		summary: summary, https: httpsSummary, expanded: make(map[int]bool), height: 24,
		decision: pgproxy.DecisionRollback,
	}
	for _, item := range summary.Unintercepted {
		title := item.Detail
		if item.Statement != "" {
			title = compactSQL(item.Statement)
		}
		model.items = append(model.items, reviewItem{
			section: "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!",
			title:   title, SQL: item.Statement, detail: item.Detail,
		})
	}
	for _, item := range httpsSummary.Unintercepted {
		model.items = append(model.items, reviewItem{
			section: "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!",
			title:   item.Host,
			detail:  item.Detail,
		})
	}
	for _, effect := range summary.NonTransactional {
		model.items = append(model.items, reviewItem{
			section: "NON-TRANSACTIONAL EFFECTS — DISCARD CANNOT UNDO THESE",
			title:   effect.Detail, detail: effect.Detail,
		})
	}
	for _, query := range summary.Queries {
		status := "ok"
		if query.Failed {
			status = "error"
		}
		model.items = append(model.items, reviewItem{
			section: "STATEMENTS", title: fmt.Sprintf("[%s] %s", status, compactSQL(query.SQL)),
			SQL: query.SQL, affected: affectedRows(query.CommandTags), err: query.Error,
		})
	}
	for _, action := range summary.IrreversibleActions {
		status := "approved and ran"
		detail := "This action ran outside the shared transaction; discard cannot undo it."
		if action.Failed {
			status = "approved; execution failed"
			detail = "This irreversible action was approved; its recorded execution attempt failed."
		}
		model.items = append(model.items, reviewItem{
			section: "APPROVED IRREVERSIBLE ACTIONS",
			title:   fmt.Sprintf("[%s] %s", status, compactSQL(action.SQL)),
			SQL:     action.SQL, affected: affectedRows(action.CommandTags), err: action.Error,
			detail: detail,
		})
	}
	for _, request := range httpsSummary.Requests {
		status := "forwarded"
		if request.StatusCode != 0 {
			status = fmt.Sprintf("forwarded: HTTP %d", request.StatusCode)
		}
		if request.Error != "" {
			status = "forwarding failed"
		}
		model.items = append(model.items, reviewItem{
			section: "HTTPS REQUESTS — ALREADY FORWARDED",
			title:   fmt.Sprintf("[%s] %s %s", status, request.Method, request.URL),
			err:     request.Error,
			detail: "This request was intercepted and forwarded. " +
				"The final commit/discard decision cannot undo an external effect.",
		})
	}
	return model
}

func (model reviewModel) Init() tea.Cmd { return nil }

func (model reviewModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		model.height = size.Height
		model.adjustOffset()
		return model, nil
	}
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return model, nil
	}
	switch key.String() {
	case "up", "k":
		if model.cursor > 0 {
			model.cursor--
		}
	case "down", "j":
		if model.cursor+1 < len(model.items) {
			model.cursor++
		}
	case "enter", " ":
		if len(model.items) > 0 {
			model.expanded[model.cursor] = !model.expanded[model.cursor]
		}
	case "c":
		model.decision = pgproxy.DecisionCommit
		model.decided = true
		return model, tea.Quit
	case "d", "q", "esc", "ctrl+c":
		model.decision = pgproxy.DecisionRollback
		model.decided = true
		return model, tea.Quit
	}
	model.adjustOffset()
	return model, nil
}

func (model *reviewModel) adjustOffset() {
	pageSize := model.pageSize()
	if model.cursor < model.offset {
		model.offset = model.cursor
	}
	if model.cursor >= model.offset+pageSize {
		model.offset = model.cursor - pageSize + 1
	}
	if model.offset < 0 {
		model.offset = 0
	}
}

func (model reviewModel) pageSize() int {
	size := model.height - 14
	if size < 4 {
		return 4
	}
	if size > 20 {
		return 20
	}
	return size
}

func (model reviewModel) View() string {
	var output strings.Builder
	output.WriteString("UNRING SESSION REVIEW\n")
	output.WriteString("One decision applies to the whole session; partial commit is not available.\n")
	if !model.summary.FullyReversible || len(model.https.Requests) > 0 {
		output.WriteString("\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
		output.WriteString("WARNING: THIS SESSION IS NOT FULLY REVERSIBLE\n")
		output.WriteString("Unring cannot guarantee every recorded effect can be undone by discarding.\n")
		output.WriteString("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
	}
	uninterceptedCount := len(model.summary.Unintercepted) + len(model.https.Unintercepted)
	if uninterceptedCount > 0 {
		output.WriteString("\n================================================================\n")
		fmt.Fprintf(&output, "!!! INTERCEPTION/COVERAGE WARNING: %d UNCLASSIFIED ITEM(S) !!!\n",
			uninterceptedCount)
		output.WriteString("Coverage is incomplete. Review these items before making any decision.\n")
		output.WriteString("================================================================\n")
	}
	writeChangeSummary(&output, model.summary)

	start := model.offset
	if start > len(model.items) {
		start = len(model.items)
	}
	end := start + model.pageSize()
	if end > len(model.items) {
		end = len(model.items)
	}
	if start > 0 {
		fmt.Fprintf(&output, "\n... %d review items above ...\n", start)
	}
	lastSection := ""
	for index := start; index < end; index++ {
		item := model.items[index]
		if item.section != lastSection {
			output.WriteString("\n" + item.section + "\n")
			if strings.HasPrefix(item.section, "!!!") {
				output.WriteString("Coverage is incomplete. Treat these items separately from intercepted statements.\n")
			}
			lastSection = item.section
		}
		cursor := "  "
		if index == model.cursor {
			cursor = "> "
		}
		indicator := "+"
		if model.expanded[index] {
			indicator = "-"
		}
		fmt.Fprintf(&output, "%s[%s] %s\n", cursor, indicator, item.title)
		if model.expanded[index] {
			if item.SQL != "" {
				output.WriteString("      Statement:\n")
				for _, line := range strings.Split(item.SQL, "\n") {
					fmt.Fprintf(&output, "        %s\n", line)
				}
			}
			if item.affected != "" {
				fmt.Fprintf(&output, "      Rows affected: %s\n", item.affected)
			}
			if item.err != "" {
				fmt.Fprintf(&output, "      Error: %s\n", item.err)
			}
			if item.detail != "" {
				fmt.Fprintf(&output, "      Detail: %s\n", item.detail)
			}
		}
	}
	if end < len(model.items) {
		fmt.Fprintf(&output, "... %d review items below ...\n", len(model.items)-end)
	}
	if model.decided {
		fmt.Fprintf(&output, "\nDecision: %s\n", model.decision)
		return output.String()
	}
	output.WriteString("\nUp/down: select  Enter/space: expand  c: commit  d: discard\n")
	return output.String()
}

func reviewDecisionWithSignal(
	input io.Reader,
	output io.Writer,
	signals <-chan os.Signal,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
) (pgproxy.Decision, bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
			interrupted <- struct{}{}
			cancel()
		case <-done:
		}
	}()

	program := tea.NewProgram(
		newReviewModelWithHTTPS(summary, httpsSummary),
		tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output),
	)
	final, err := program.Run()
	close(done)
	select {
	case <-interrupted:
		return pgproxy.DecisionRollback, true, nil
	default:
	}
	if err != nil {
		return pgproxy.DecisionRollback, false, fmt.Errorf("run interactive review: %w", err)
	}
	model, ok := final.(reviewModel)
	if !ok || !model.decided {
		return pgproxy.DecisionRollback, false, nil
	}
	return model.decision, false, nil
}

func shouldUseTUI(input io.Reader, output io.Writer) bool {
	return isTerminal(input) && isTerminalWriter(output) && os.Getenv("TERM") != "dumb"
}

func isTerminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
