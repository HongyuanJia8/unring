// Package cli implements the unring command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HongyuanJia8/unring/internal/childenv"
	"github.com/HongyuanJia8/unring/internal/pgproxy"
	"github.com/HongyuanJia8/unring/internal/runner"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/term"
)

const (
	internalErrorExitCode = 1
	usageExitCode         = 2
)

// Main runs the CLI and returns the desired process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return usageExitCode
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stdin, stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		if isNamedAlias(args[0]) || resolvesOnPath(args[0]) {
			runArgs := append([]string{"--", args[0]}, args[1:]...)
			return runCommand(runArgs, stdin, stdout, stderr)
		}
		fmt.Fprintf(stderr, "unring: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return usageExitCode
	}
}

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unring run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	forceCommit := flags.Bool("commit", false, "commit without prompting")
	forceDiscard := flags.Bool("discard", false, "discard without prompting")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring run [--commit | --discard] -- <command> [args...]")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if *forceCommit && *forceDiscard {
		fmt.Fprintln(stderr, "unring: --commit and --discard are mutually exclusive")
		return usageExitCode
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, "unring: no child command given")
		flags.Usage()
		return usageExitCode
	}

	backendConfig, err := parseBackendConfig()
	if err != nil {
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	approvalRequests := make(chan runner.ApprovalRequest)
	proxy, err := pgproxy.StartWithOptions(ctx, backendConfig, pgproxy.Options{
		Approve: func(approvalContext context.Context, request pgproxy.ApprovalRequest) (bool, error) {
			reply := make(chan runner.ApprovalResult, 1)
			work := runner.ApprovalRequest{
				Decide: func() (bool, error) {
					return promptIrreversibleApproval(stdin, stdout, request), nil
				},
				Reply: reply,
			}
			select {
			case approvalRequests <- work:
			case <-approvalContext.Done():
				return false, approvalContext.Err()
			}
			select {
			case result := <-reply:
				return result.Approved, result.Err
			case <-approvalContext.Done():
				return false, approvalContext.Err()
			}
		},
	})
	cancel()
	if err != nil {
		fmt.Fprintf(stderr, "unring: start postgres session: %v\n", err)
		return internalErrorExitCode
	}
	defer func() {
		if closeErr := proxy.Close(); closeErr != nil {
			fmt.Fprintf(stderr, "unring: safety rollback failed: %v\n", closeErr)
		}
	}()

	childEnvironment, err := childenv.Postgres(os.Environ(), proxy.Address(), backendConfig)
	if err != nil {
		fmt.Fprintf(stderr, "unring: build child environment: %v\n", err)
		return internalErrorExitCode
	}

	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChannel)

	result := runner.Run(runner.Options{
		Command:   command,
		Env:       childEnvironment,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		Signals:   signalChannel,
		Abort:     proxy.Done(),
		Approvals: approvalRequests,
	})
	interrupted := result.Interrupted || pendingSignal(signalChannel)

	sealContext, sealCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sealErr := proxy.Seal(sealContext)
	sealCancel()
	summary := proxy.Summary()

	interceptionErr := sealErr
	if fatalErr := proxy.Err(); interceptionErr == nil && fatalErr != nil {
		interceptionErr = fatalErr
	}
	if interceptionErr != nil {
		fmt.Fprintf(
			stderr,
			"unring: INTERCEPTION LOST: the real Postgres outcome is unknown; "+
				"unring will not claim this session was safely discarded: %v\n",
			interceptionErr,
		)
	}

	if interceptionErr == nil && !summary.HasReviewableActivity() {
		if result.Err != nil {
			fmt.Fprintf(stderr, "unring: %v\n", result.Err)
		}
		finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		finalizeErr := proxy.Finalize(finalizeContext, pgproxy.DecisionRollback)
		finalizeCancel()
		if finalizeErr != nil {
			fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
			return internalErrorExitCode
		}
		return result.ExitCode
	}

	useTUI := interceptionErr == nil && !interrupted && result.Err == nil &&
		summary.Changes.Complete && !*forceCommit && !*forceDiscard && shouldUseTUI(stdin, stdout)
	if !useTUI {
		printSummary(stdout, summary)
	}

	decision := pgproxy.DecisionRollback
	switch {
	case interceptionErr != nil:
		// Rollback remains the only safe request, but Finalize will report
		// that it cannot confirm the outcome.
	case !summary.Changes.Complete:
		fmt.Fprintln(stderr,
			"unring: the sealed change summary is incomplete; discarding instead of offering commit")
	case interrupted:
		fmt.Fprintln(stdout, "Signal received: discarding the session.")
	case result.Err != nil:
		fmt.Fprintf(stderr, "unring: %v\n", result.Err)
	case *forceCommit:
		decision = pgproxy.DecisionCommit
	case *forceDiscard:
		// Rollback is already the safe default.
	default:
		if useTUI {
			var reviewErr error
			decision, interrupted, reviewErr = reviewDecisionWithSignal(
				stdin, stdout, signalChannel, summary,
			)
			if reviewErr != nil {
				fmt.Fprintf(stderr, "unring: %v; defaulting to discard\n", reviewErr)
				decision = pgproxy.DecisionRollback
			}
		} else {
			var promptInterrupted bool
			decision, promptInterrupted = promptDecisionWithSignal(stdin, stdout, signalChannel)
			interrupted = interrupted || promptInterrupted
		}
	}

	if pendingSignal(signalChannel) {
		interrupted = true
	}
	if interrupted {
		decision = pgproxy.DecisionRollback
	}

	finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	finalizeErr := proxy.Finalize(finalizeContext, decision)
	finalizeCancel()
	if finalizeErr != nil {
		fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
		return internalErrorExitCode
	}
	fmt.Fprintf(stdout, "Session %s.\n", pastTense(decision))

	if result.Err != nil {
		return result.ExitCode
	}
	return result.ExitCode
}

func isNamedAlias(name string) bool {
	switch name {
	case "claude", "codex", "opencode":
		return true
	default:
		return false
	}
}

func resolvesOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func parseBackendConfig() (*pgconn.Config, error) {
	connectionString := os.Getenv("DATABASE_URL")
	config, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("read real Postgres connection settings: %w", err)
	}
	return config, nil
}

func promptDecision(input io.Reader, output io.Writer) pgproxy.Decision {
	if !isTerminal(input) {
		fmt.Fprintln(output, "No interactive terminal; defaulting to discard. Use --commit to commit.")
		return pgproxy.DecisionRollback
	}

	reader := bufio.NewReader(input)
	for {
		fmt.Fprint(output, "Commit or discard? [c/D] ")
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(output, "\nCould not read decision (%v); defaulting to discard.\n", err)
			return pgproxy.DecisionRollback
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "c", "commit":
			return pgproxy.DecisionCommit
		case "", "d", "discard":
			return pgproxy.DecisionRollback
		default:
			fmt.Fprintln(output, "Please enter commit or discard.")
		}
		if errors.Is(err, io.EOF) {
			return pgproxy.DecisionRollback
		}
	}
}

func promptIrreversibleApproval(
	input io.Reader,
	output io.Writer,
	request pgproxy.ApprovalRequest,
) bool {
	fmt.Fprintln(output, "\nIrreversible PostgreSQL action requested")
	fmt.Fprintln(output, "  SQL (exactly as requested):")
	fmt.Fprintln(output, request.SQL)
	fmt.Fprintf(output, "  Reason: %s.\n", request.Reason)
	fmt.Fprintln(output,
		"  This will run now on a separate non-transactional connection and cannot be undone by discard.")
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "  No interactive terminal; declining the action.")
		return false
	}

	fmt.Fprint(output, "Run this irreversible action? [y/N] ")
	answer, err := readOnePromptLine(input)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(output, "\nCould not read approval (%v); declining.\n", err)
		return false
	}
	approved := strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !approved {
		fmt.Fprintln(output, "Action declined; it was not run.")
	}
	return approved
}

// readOnePromptLine deliberately limits every Read call to one byte. A
// bufio.Reader may read several canonical terminal lines at once on Linux;
// discarding that reader after the approval would then swallow input intended
// for the resumed child or the final review prompt.
func readOnePromptLine(input io.Reader) (string, error) {
	var line strings.Builder
	var buffer [1]byte
	for {
		count, err := input.Read(buffer[:])
		if count == 1 {
			line.WriteByte(buffer[0])
			if buffer[0] == '\n' {
				return line.String(), nil
			}
		}
		if err != nil {
			return line.String(), err
		}
		if count == 0 {
			return line.String(), io.ErrNoProgress
		}
	}
}

func promptDecisionWithSignal(
	input io.Reader,
	output io.Writer,
	signals <-chan os.Signal,
) (pgproxy.Decision, bool) {
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "No interactive terminal; defaulting to discard. Use --commit to commit.")
		return pgproxy.DecisionRollback, false
	}

	decision := make(chan pgproxy.Decision, 1)
	go func() {
		decision <- promptDecision(input, output)
	}()
	select {
	case chosen := <-decision:
		return chosen, false
	case <-signals:
		fmt.Fprintln(output, "\nSignal received: discarding the session.")
		return pgproxy.DecisionRollback, true
	}
}

func pendingSignal(signals <-chan os.Signal) bool {
	select {
	case <-signals:
		return true
	default:
		return false
	}
}

func printSummary(output io.Writer, summary pgproxy.Summary) {
	failed := 0
	for _, query := range summary.Queries {
		if query.Failed {
			failed++
		}
	}

	fmt.Fprintln(output, "\nUNRING SESSION REVIEW")
	if !summary.FullyReversible {
		fmt.Fprintln(output, "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		fmt.Fprintln(output, "WARNING: THIS SESSION IS NOT FULLY REVERSIBLE")
		fmt.Fprintln(output, "Unring cannot guarantee every recorded effect can be undone by discarding.")
		fmt.Fprintln(output, "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
	}
	writeChangeSummary(output, summary)
	fmt.Fprintln(output, "\nSTATEMENTS")
	fmt.Fprintf(output, "  Connections: %d (one shared backend transaction)\n", summary.Connections)
	fmt.Fprintf(output, "  Query batches: %d", len(summary.Queries))
	if failed > 0 {
		fmt.Fprintf(output, " (%d failed)", failed)
	}
	fmt.Fprintln(output)

	for _, query := range summary.Queries {
		status := "ok"
		if query.Failed {
			status = "error"
		}
		fmt.Fprintf(output, "  - [%s] %s", status, compactSQL(query.SQL))
		if len(query.CommandTags) > 0 {
			fmt.Fprintf(output, " -> %s", strings.Join(query.CommandTags, ", "))
		}
		fmt.Fprintln(output)
		if query.Error != "" {
			fmt.Fprintf(output, "    Error: %s\n", query.Error)
		}
	}
	if len(summary.NonTransactional) > 0 {
		fmt.Fprintln(output, "\nNON-TRANSACTIONAL EFFECTS — DISCARD CANNOT UNDO THESE")
		for _, effect := range summary.NonTransactional {
			fmt.Fprintf(output, "  - %s\n", effect.Detail)
		}
	}
	if len(summary.IrreversibleActions) > 0 {
		fmt.Fprintln(output, "\nAPPROVED IRREVERSIBLE ACTIONS — DISCARD CANNOT UNDO THESE")
		fmt.Fprintln(output, "  Successful actions ran outside the shared transaction; discard cannot undo them.")
		for _, action := range summary.IrreversibleActions {
			fmt.Fprintf(output, "  - %s", compactSQL(action.SQL))
			if len(action.CommandTags) > 0 {
				fmt.Fprintf(output, " -> %s", strings.Join(action.CommandTags, ", "))
			}
			fmt.Fprintln(output)
			if action.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", action.Error)
			}
		}
	}
	if len(summary.Unintercepted) > 0 {
		fmt.Fprintln(output, "\n================================================================")
		fmt.Fprintln(output, "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!")
		fmt.Fprintln(output, "Coverage is incomplete. These items are not part of the normal statement list.")
		for _, item := range summary.Unintercepted {
			if item.Statement != "" {
				fmt.Fprintf(output, "  Statement: %s\n", item.Statement)
			}
			fmt.Fprintf(output, "  Detail: %s\n", item.Detail)
		}
		fmt.Fprintln(output, "================================================================")
	}
}

func writeChangeSummary(output io.Writer, summary pgproxy.Summary) {
	fmt.Fprintln(output, "\nDATA CHANGES (reported by PostgreSQL for the sealed transaction)")
	if !summary.Changes.Complete {
		fmt.Fprintf(output, "  UNKNOWN — the change summary is incomplete: %s\n", summary.Changes.Error)
	} else if len(summary.Changes.Rows) == 0 {
		fmt.Fprintln(output, "  No rows inserted, updated, or deleted.")
	} else {
		for _, change := range summary.Changes.Rows {
			fmt.Fprintf(output, "  - %s: %d inserted, %d updated, %d deleted\n",
				change.Table, change.Inserted, change.Updated, change.Deleted)
		}
	}
	fmt.Fprintln(output, "  Note: PostgreSQL sequences do not roll back; discarded sessions can leave ID gaps.")
	fmt.Fprintln(output, "\nSCHEMA CHANGES (sealed catalog comparison)")
	if !summary.Changes.Complete {
		fmt.Fprintln(output, "  UNKNOWN — catalog changes could not be determined safely.")
	} else if len(summary.Changes.Schema) == 0 {
		fmt.Fprintln(output, "  No schema changes.")
	} else {
		for _, change := range summary.Changes.Schema {
			fmt.Fprintf(output, "  - %s %s %s\n", change.Action, change.Kind, change.Object)
		}
	}
}

func affectedRows(tags []string) string {
	var affected []string
	for _, tag := range tags {
		fields := strings.Fields(tag)
		if len(fields) < 2 {
			continue
		}
		operation := strings.ToUpper(fields[0])
		if operation != "INSERT" && operation != "UPDATE" && operation != "DELETE" &&
			operation != "MERGE" && operation != "COPY" && operation != "MOVE" &&
			operation != "FETCH" && operation != "SELECT" {
			continue
		}
		countText := fields[len(fields)-1]
		if _, err := strconv.ParseInt(countText, 10, 64); err != nil {
			continue
		}
		affected = append(affected, strings.ToLower(operation)+" "+countText)
	}
	return strings.Join(affected, ", ")
}

func compactSQL(sql string) string {
	compacted := strings.Join(strings.Fields(sql), " ")
	const maximum = 160
	if len(compacted) <= maximum {
		return compacted
	}
	return compacted[:maximum-3] + "..."
}

func isTerminal(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func pastTense(decision pgproxy.Decision) string {
	if decision == pgproxy.DecisionCommit {
		return "committed"
	}
	return "discarded"
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  unring run [--commit | --discard] -- <command> [args...]")
	fmt.Fprintln(output, "  unring <command-on-PATH> [--] [args...]")
	fmt.Fprintln(output, "  unring claude|codex|opencode [--] [args...]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "The child inherits PostgreSQL connection settings that point only it at")
	fmt.Fprintln(output, "unring's loopback proxy. Without a terminal, the safe default is discard.")
}
