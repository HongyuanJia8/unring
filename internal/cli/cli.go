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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HongyuanJia8/unring/internal/childenv"
	"github.com/HongyuanJia8/unring/internal/pgproxy"
	"github.com/HongyuanJia8/unring/internal/runner"
	"github.com/jackc/pgx/v5/pgconn"
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
	proxy, err := pgproxy.Start(ctx, backendConfig)
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
		Command: command,
		Env:     childEnvironment,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Signals: signalChannel,
		Abort:   proxy.Done(),
	})
	interrupted := result.Interrupted || pendingSignal(signalChannel)

	sealContext, sealCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sealErr := proxy.Seal(sealContext)
	sealCancel()

	printSummary(stdout, proxy.Summary())

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

	decision := pgproxy.DecisionRollback
	switch {
	case interceptionErr != nil:
		// Rollback remains the only safe request, but Finalize will report
		// that it cannot confirm the outcome.
	case interrupted:
		fmt.Fprintln(stdout, "Signal received: discarding the session.")
	case result.Err != nil:
		fmt.Fprintf(stderr, "unring: %v\n", result.Err)
	case *forceCommit:
		decision = pgproxy.DecisionCommit
	case *forceDiscard:
		// Rollback is already the safe default.
	default:
		var promptInterrupted bool
		decision, promptInterrupted = promptDecisionWithSignal(stdin, stdout, signalChannel)
		interrupted = interrupted || promptInterrupted
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

func promptDecisionWithSignal(
	input io.Reader,
	output io.Writer,
	signals <-chan os.Signal,
) (pgproxy.Decision, bool) {
	if !isTerminal(input) {
		return promptDecision(input, output), false
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

	fmt.Fprintln(output, "\nPostgres session summary")
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
	}
	fmt.Fprintln(output, "  Note: PostgreSQL sequences do not roll back; discarded sessions can leave ID gaps.")
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
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
	fmt.Fprintln(output)
	fmt.Fprintln(output, "The child inherits PostgreSQL connection settings that point only it at")
	fmt.Fprintln(output, "unring's loopback proxy. Without a terminal, the safe default is discard.")
}
