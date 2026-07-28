// Package cli implements the unring command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hyj28/unring/internal/adapter"
	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/childenv"
	"github.com/hyj28/unring/internal/httpsproxy"
	"github.com/hyj28/unring/internal/pgproxy"
	"github.com/hyj28/unring/internal/runner"
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
	case "log":
		return logCommand(args[1:], stdout, stderr)
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

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int) {
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

	auditStore, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open audit log: %v\n", err)
		return internalErrorExitCode
	}
	auditSession, err := auditStore.Begin(command, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "unring: begin audit log: %v\n", err)
		return internalErrorExitCode
	}

	var proxy *pgproxy.Proxy
	var httpsProxy *httpsproxy.Proxy
	var finalized bool
	var auditError string
	requestedDecision := "discard"
	defer func() {
		recovered := recover()
		outcome := ""
		if proxy != nil && !finalized {
			closeErr := proxy.Close()
			if closeErr == nil {
				outcome = "discarded"
			} else {
				outcome = "unknown"
				auditError = joinErrorText(auditError, closeErr)
			}
		}
		if httpsProxy != nil {
			if closeErr := httpsProxy.Close(); closeErr != nil {
				auditError = joinErrorText(auditError, closeErr)
			}
		}
		if recovered != nil {
			auditError = fmt.Sprintf("panic: %v", recovered)
			if exitCode == 0 {
				exitCode = internalErrorExitCode
			}
		}
		saveErr := auditSession.Update(func(record *audit.Record) {
			record.EndedAt = time.Now().UTC()
			record.ExitCode = exitCode
			record.Error = strings.TrimPrefix(auditError, "\n")
			record.Decision = requestedDecision
			if proxy != nil {
				record.Postgres = proxy.Summary()
			}
			if httpsProxy != nil {
				updateHTTPSAudit(record, httpsProxy.Summary())
			}
			if outcome != "" {
				record.Outcome = outcome
			} else if record.Outcome == "pending" {
				record.Outcome = "not_started"
			}
		})
		if saveErr != nil {
			fmt.Fprintf(stderr, "unring: write audit log: %v\n", saveErr)
			if exitCode == 0 {
				exitCode = internalErrorExitCode
			}
		}
		if recovered != nil {
			panic(recovered)
		}
	}()

	backendConfig, err := parseBackendConfig()
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}

	adapterSet, err := loadAdapters(os.Getenv("UNRING_ADAPTERS"))
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: load HTTPS adapters: %v\n", err)
		return internalErrorExitCode
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	approvalRequests := make(chan runner.ApprovalRequest)
	proxy, err = pgproxy.StartWithOptions(ctx, backendConfig, pgproxy.Options{
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
				decision := "declined"
				if result.Approved {
					decision = "approved"
				}
				approvalError := ""
				if result.Err != nil {
					decision = "error"
					approvalError = result.Err.Error()
				}
				if err := auditSession.Update(func(record *audit.Record) {
					record.Approvals = append(record.Approvals, audit.Approval{
						Kind: "postgres", Statement: request.SQL, Reason: request.Reason,
						Decision: decision, Error: approvalError, Time: time.Now().UTC(),
					})
				}); err != nil {
					return false, fmt.Errorf("record irreversible-action decision: %w", err)
				}
				return result.Approved, result.Err
			case <-approvalContext.Done():
				return false, approvalContext.Err()
			}
		},
	})
	cancel()
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: start postgres session: %v\n", err)
		return internalErrorExitCode
	}

	authority, err := httpsproxy.EnsureAuthority(auditStore.StateDir())
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: initialize per-user HTTPS CA: %v\n", err)
		return internalErrorExitCode
	}
	httpsProxy, err = httpsproxy.Start(authority, httpsproxy.Options{
		PassthroughHost: configuredPassthroughHosts(os.Getenv("UNRING_HTTPS_PASSTHROUGH")),
		Adapters:        adapterSet,
		Approve: func(approvalContext context.Context, request httpsproxy.ApprovalRequest) (bool, error) {
			reply := make(chan runner.ApprovalResult, 1)
			work := runner.ApprovalRequest{
				Decide: func() (bool, error) {
					return promptHTTPSApproval(stdin, stdout, request), nil
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
				decision := "declined"
				if result.Approved {
					decision = "approved"
				}
				approvalError := ""
				if result.Err != nil {
					decision = "error"
					approvalError = result.Err.Error()
				}
				if err := auditSession.Update(func(record *audit.Record) {
					record.Approvals = append(record.Approvals, audit.Approval{
						Kind: "https", Statement: request.Method + " " + request.URL,
						Reason: request.Reason, Decision: decision,
						Error: approvalError, Time: time.Now().UTC(),
					})
				}); err != nil {
					return false, fmt.Errorf("record HTTPS approval decision: %w", err)
				}
				return result.Approved, result.Err
			case <-approvalContext.Done():
				return false, approvalContext.Err()
			}
		},
	})
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: start HTTPS proxy: %v\n", err)
		return internalErrorExitCode
	}

	childEnvironment, err := childenv.Postgres(os.Environ(), proxy.Address(), backendConfig)
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: build child environment: %v\n", err)
		return internalErrorExitCode
	}
	childEnvironment, err = childenv.HTTPS(
		childEnvironment, httpsProxy.Address(), authority.CertificatePath,
	)
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: build child HTTPS environment: %v\n", err)
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
	if result.Err != nil {
		auditError = joinErrorText(auditError, result.Err)
	}

	httpsSealContext, httpsSealCancel := context.WithTimeout(context.Background(), 10*time.Second)
	httpsSealErr := httpsProxy.Seal(httpsSealContext)
	httpsSealCancel()
	httpsSummary := httpsProxy.Summary()

	sealContext, sealCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sealErr := proxy.Seal(sealContext)
	sealCancel()
	summary := proxy.Summary()

	postgresInterceptionErr := sealErr
	if fatalErr := proxy.Err(); postgresInterceptionErr == nil && fatalErr != nil {
		postgresInterceptionErr = fatalErr
	}
	if postgresInterceptionErr != nil {
		auditError = joinErrorText(auditError, postgresInterceptionErr)
		fmt.Fprintf(
			stderr,
			"unring: INTERCEPTION LOST: the real Postgres outcome is unknown; "+
				"unring will not claim this session was safely discarded: %v\n",
			postgresInterceptionErr,
		)
	}
	if httpsSealErr != nil {
		auditError = joinErrorText(auditError, httpsSealErr)
		fmt.Fprintf(stderr,
			"unring: HTTPS INTERCEPTION LOST: the HTTPS audit may be incomplete; "+
				"the database session will be discarded: %v\n", httpsSealErr)
	}
	interceptionErr := errors.Join(postgresInterceptionErr, httpsSealErr)

	if interceptionErr == nil && !summary.HasReviewableActivity() &&
		!httpsSummary.HasReviewableActivity() {
		if result.Err != nil {
			fmt.Fprintf(stderr, "unring: %v\n", result.Err)
		}
		finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		httpsFinalizeErr := httpsProxy.Finalize(finalizeContext, false)
		finalizeErr := errors.Join(
			httpsFinalizeErr,
			proxy.Finalize(finalizeContext, pgproxy.DecisionRollback),
		)
		finalizeCancel()
		if finalizeErr != nil {
			finalized = true
			auditError = finalizeErr.Error()
			_ = auditSession.Update(func(record *audit.Record) {
				record.Postgres = summary
				updateHTTPSAudit(record, httpsProxy.Summary())
				record.Decision = "discard"
				record.Outcome = "unknown"
			})
			fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
			return internalErrorExitCode
		}
		finalized = true
		if err := auditSession.Update(func(record *audit.Record) {
			record.Postgres = summary
			updateHTTPSAudit(record, httpsProxy.Summary())
			record.Decision = "discard"
			record.Outcome = "discarded"
		}); err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: write audit log: %v\n", err)
			return internalErrorExitCode
		}
		return result.ExitCode
	}

	useTUI := interceptionErr == nil && !interrupted && result.Err == nil &&
		summary.Changes.Complete && !*forceCommit && !*forceDiscard && shouldUseTUI(stdin, stdout)
	if !useTUI {
		printSummaryWithHTTPS(stdout, summary, httpsSummary)
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
				stdin, stdout, signalChannel, summary, httpsSummary,
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

	if err := auditSession.Update(func(record *audit.Record) {
		record.Postgres = summary
		updateHTTPSAudit(record, httpsSummary)
		record.Decision = auditDecision(decision)
	}); err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: write audit log before final decision: %v\n", err)
		decision = pgproxy.DecisionRollback
	}
	requestedDecision = auditDecision(decision)
	commitHTTPS := decision == pgproxy.DecisionCommit
	httpsFinalizeContext, httpsFinalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	httpsFinalizeErr := httpsProxy.Finalize(httpsFinalizeContext, commitHTTPS)
	httpsFinalizeCancel()
	postgresDecision := decision
	if httpsFinalizeErr != nil {
		// A staged HTTP replay may have partially succeeded. Keep the database
		// reversible instead of compounding that uncertainty with a commit.
		postgresDecision = pgproxy.DecisionRollback
		auditError = joinErrorText(auditError, httpsFinalizeErr)
	}
	finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	postgresFinalizeErr := proxy.Finalize(finalizeContext, postgresDecision)
	finalizeCancel()
	finalizeErr := errors.Join(httpsFinalizeErr, postgresFinalizeErr)
	finalized = true
	if finalizeErr != nil {
		auditError = joinErrorText(auditError, postgresFinalizeErr)
		_ = auditSession.Update(func(record *audit.Record) {
			record.Postgres = proxy.Summary()
			updateHTTPSAudit(record, httpsProxy.Summary())
			record.Outcome = "unknown"
		})
		fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
		return internalErrorExitCode
	}
	if err := auditSession.Update(func(record *audit.Record) {
		record.Postgres = proxy.Summary()
		updateHTTPSAudit(record, httpsProxy.Summary())
		record.Outcome = pastTense(decision)
	}); err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: write final audit log: %v\n", err)
		return internalErrorExitCode
	}
	fmt.Fprintf(stdout, "Session %s.\n", pastTense(decision))

	if result.Err != nil {
		return result.ExitCode
	}
	return result.ExitCode
}

func joinErrorText(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func configuredPassthroughHosts(value string) func(string) bool {
	hosts := make(map[string]struct{})
	for _, host := range strings.Split(value, ",") {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hosts[host] = struct{}{}
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return func(host string) bool {
		host = strings.ToLower(host)
		hostname := host
		if splitHost, _, err := net.SplitHostPort(host); err == nil {
			hostname = splitHost
		}
		_, exact := hosts[host]
		_, withoutPort := hosts[hostname]
		return exact || withoutPort
	}
}

func loadAdapters(value string) (*adapter.Set, error) {
	var filenames []string
	for _, filename := range strings.Split(value, string(os.PathListSeparator)) {
		if filename = strings.TrimSpace(filename); filename != "" {
			filenames = append(filenames, filename)
		}
	}
	userSources, err := adapter.ReadFiles(filenames)
	if err != nil {
		return nil, err
	}
	builtinSources, err := adapter.BuiltinSources()
	if err != nil {
		return nil, err
	}
	// Both categories deliberately enter the exact same loader call. Load has
	// no knowledge of names, origin, or built-in status.
	sources := append(userSources, builtinSources...)
	return adapter.Load(sources...)
}

func updateHTTPSAudit(record *audit.Record, summary httpsproxy.Summary) {
	record.HTTPS = summary
	unintercepted := make([]audit.Unintercepted, 0,
		len(record.Postgres.Unintercepted)+len(summary.Unintercepted))
	for _, item := range record.Postgres.Unintercepted {
		unintercepted = append(unintercepted, audit.Unintercepted{
			Kind: "postgres", Statement: item.Statement,
			Detail: item.Detail, Time: time.Now().UTC(),
		})
	}
	for _, item := range summary.Unintercepted {
		unintercepted = append(unintercepted, audit.Unintercepted{
			Kind: "https", Host: item.Host, Detail: item.Detail, Time: item.Time,
		})
	}
	record.Unintercepted = unintercepted
}

func auditDecision(decision pgproxy.Decision) string {
	if decision == pgproxy.DecisionCommit {
		return "commit"
	}
	return "discard"
}

func logCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unring log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print structured JSON")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring log [--json] [session-id]")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return usageExitCode
	}
	store, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open audit log: %v\n", err)
		return internalErrorExitCode
	}
	if flags.NArg() == 0 {
		records, err := store.List()
		if err != nil {
			if records == nil {
				fmt.Fprintf(stderr, "unring: %v\n", err)
				return internalErrorExitCode
			}
			fmt.Fprintf(stderr,
				"unring: warning: some audit records could not be read and were skipped: %v\n",
				err,
			)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, records)
		}
		if len(records) == 0 {
			fmt.Fprintln(stdout, "No unring sessions have been recorded.")
			return 0
		}
		fmt.Fprintln(stdout, "SESSION ID                                  STARTED               OUTCOME      COMMAND")
		for _, record := range records {
			fmt.Fprintf(stdout, "%-43s %-21s %-12s %s\n",
				record.ID, record.StartedAt.Local().Format("2006-01-02 15:04:05"),
				record.Outcome, strings.Join(record.Command, " "))
		}
		return 0
	}
	record, err := store.Load(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}
	if *asJSON {
		return writeJSON(stdout, stderr, record)
	}
	printAuditRecord(stdout, record)
	return 0
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "unring: encode audit log: %v\n", err)
		return internalErrorExitCode
	}
	return 0
}

func printAuditRecord(output io.Writer, record audit.Record) {
	fmt.Fprintf(output, "UNRING SESSION %s\n", record.ID)
	fmt.Fprintf(output, "Started:  %s\n", record.StartedAt.Local().Format(time.RFC3339))
	if !record.EndedAt.IsZero() {
		fmt.Fprintf(output, "Ended:    %s\n", record.EndedAt.Local().Format(time.RFC3339))
	}
	fmt.Fprintf(output, "Command:  %s\n", strings.Join(record.Command, " "))
	fmt.Fprintf(output, "Decision: %s\n", record.Decision)
	fmt.Fprintf(output, "Outcome:  %s\n", record.Outcome)
	fmt.Fprintf(output, "Exit code: %d\n", record.ExitCode)
	if record.Error != "" {
		fmt.Fprintf(output, "Error: %s\n", record.Error)
	}
	printSummaryWithHTTPS(output, record.Postgres, record.HTTPS)
	if len(record.Approvals) > 0 {
		fmt.Fprintln(output, "\nIRREVERSIBLE ACTION DECISIONS")
		for _, approval := range record.Approvals {
			fmt.Fprintf(output, "  - [%s] %s\n", approval.Decision, compactSQL(approval.Statement))
			fmt.Fprintf(output, "    Reason: %s\n", approval.Reason)
			if approval.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", approval.Error)
			}
		}
	}
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

func promptHTTPSApproval(
	input io.Reader,
	output io.Writer,
	request httpsproxy.ApprovalRequest,
) bool {
	fmt.Fprintln(output, "\nHTTPS action needs approval")
	fmt.Fprintf(output, "  Request: %s %s\n", request.Method, request.URL)
	if request.Adapter != "" {
		fmt.Fprintf(output, "  Adapter rule: %s / %s\n", request.Adapter, request.Rule)
	}
	fmt.Fprintf(output, "  Reason: %s.\n", request.Reason)
	fmt.Fprintln(output,
		"  Approving sends this request to the real service now; declining guarantees it is not sent.")
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "  No interactive terminal; declining the action.")
		return false
	}
	fmt.Fprint(output, "Send this HTTPS request? [y/N] ")
	answer, err := readOnePromptLine(input)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(output, "\nCould not read approval (%v); declining.\n", err)
		return false
	}
	approved := strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !approved {
		fmt.Fprintln(output, "Action declined; it was not sent.")
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
	printSummaryWithHTTPS(output, summary, httpsproxy.Summary{Sealed: true})
}

func printSummaryWithHTTPS(
	output io.Writer,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
) {
	failed := 0
	for _, query := range summary.Queries {
		if query.Failed {
			failed++
		}
	}

	fmt.Fprintln(output, "\nUNRING SESSION REVIEW")
	if !summary.FullyReversible || len(httpsSummary.Requests) > 0 {
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
	if len(httpsSummary.Staged) > 0 {
		pending := false
		for _, request := range httpsSummary.Staged {
			if request.State == "" || request.State == "pending" {
				pending = true
				break
			}
		}
		if pending {
			fmt.Fprintln(output, "\nPENDING HTTPS — WILL BE SENT IF YOU COMMIT")
			fmt.Fprintln(output,
				"  These requests have not reached their origins. Discard drops them without sending.")
		} else {
			fmt.Fprintln(output, "\nSTAGED HTTPS CALLS — FINAL OUTCOME")
		}
		for _, request := range httpsSummary.Staged {
			fmt.Fprintf(output, "  - [%s] %s %s\n", request.State, request.Method, request.URL)
			fmt.Fprintf(output, "    Idempotency key: %s\n", request.IdempotencyKey)
			if request.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", request.Error)
			}
		}
	}
	declinedApprovals := 0
	for _, approval := range httpsSummary.Approvals {
		if approval.Decision != "approved" {
			declinedApprovals++
		}
	}
	if declinedApprovals > 0 {
		fmt.Fprintln(output, "\nHTTPS APPROVALS — NOT SENT")
		fmt.Fprintln(output, "  These needs-approval requests did not reach their origins.")
		for _, approval := range httpsSummary.Approvals {
			if approval.Decision == "approved" {
				continue
			}
			fmt.Fprintf(output, "  - [%s] %s %s\n",
				approval.Decision, approval.Method, approval.URL)
			if approval.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", approval.Error)
			}
		}
	}
	if len(httpsSummary.Requests) > 0 {
		fmt.Fprintln(output, "\nHTTPS REQUESTS — INTERCEPTED AND ALREADY FORWARDED")
		fmt.Fprintln(output,
			"  These requests reached their destinations; commit/discard cannot undo external effects.")
		for _, request := range httpsSummary.Requests {
			status := "forwarded"
			if request.StatusCode != 0 {
				status = fmt.Sprintf("HTTP %d", request.StatusCode)
			}
			fmt.Fprintf(output, "  - [%s] %s %s\n", status, request.Method, request.URL)
			if request.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", request.Error)
			}
		}
	}
	if len(summary.Unintercepted) > 0 || len(httpsSummary.Unintercepted) > 0 {
		fmt.Fprintln(output, "\n================================================================")
		fmt.Fprintln(output, "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!")
		fmt.Fprintln(output, "Coverage is incomplete. These items are not part of the normal statement list.")
		for _, item := range summary.Unintercepted {
			if item.Statement != "" {
				fmt.Fprintf(output, "  Statement: %s\n", item.Statement)
			}
			fmt.Fprintf(output, "  Detail: %s\n", item.Detail)
		}
		for _, item := range httpsSummary.Unintercepted {
			if item.Host != "" {
				fmt.Fprintf(output, "  Host: %s\n", item.Host)
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
	fmt.Fprintln(output, "  unring log [--json] [session-id]")
	fmt.Fprintln(output, "  unring <command-on-PATH> [--] [args...]")
	fmt.Fprintln(output, "  unring claude|codex|opencode [--] [args...]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "The child inherits PostgreSQL connection settings that point only it at")
	fmt.Fprintln(output, "unring's loopback proxy. Without a terminal, the safe default is discard.")
}
