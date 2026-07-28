// Package ghshim installs and serves the per-session gh PATH shim.
package ghshim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyj28/unring/internal/adapter"
	"golang.org/x/term"
)

const (
	envMarker       = "UNRING_GH_SHIM"
	envSocket       = "UNRING_GH_SHIM_SOCKET"
	envToken        = "UNRING_GH_SHIM_TOKEN"
	envRealGH       = "UNRING_GH_REAL"
	envOriginalPath = "UNRING_GH_ORIGINAL_PATH"
)

// ApprovalRequest is a parsed gh intent that cannot safely pass through.
type ApprovalRequest struct {
	Invocation string
	Intent     string
	Reason     string
}

// Record describes a mutation or ambiguous invocation; confident reads are
// transparent and intentionally do not make an otherwise read-only review noisy.
type Record struct {
	Arguments   []string     `json:"arguments"`
	Intent      string       `json:"intent"`
	Tier        adapter.Tier `json:"tier"`
	Adapter     string       `json:"adapter,omitempty"`
	Rule        string       `json:"rule,omitempty"`
	Reason      string       `json:"reason"`
	Decision    string       `json:"decision"`
	State       string       `json:"state"`
	ExitCode    int          `json:"exit_code,omitempty"`
	Error       string       `json:"error,omitempty"`
	UndoEffect  string       `json:"undo_effect,omitempty"`
	StillExists string       `json:"still_exists,omitempty"`
	ResourceURL string       `json:"resource_url,omitempty"`
	UndoState   string       `json:"undo_state,omitempty"`
	UndoError   string       `json:"undo_error,omitempty"`
	Time        time.Time    `json:"time"`
}

// Summary is the session's structured gh activity.
type Summary struct {
	Records []Record `json:"records"`
	Sealed  bool     `json:"sealed"`
}

func (summary Summary) HasReviewableActivity() bool { return len(summary.Records) > 0 }

// Options configures the shim server.
type Options struct {
	Adapters   *adapter.Set
	Approve    func(context.Context, ApprovalRequest) (bool, error)
	Executable string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// Session owns one private shim directory and its IPC listener.
type Session struct {
	directory    string
	socket       string
	token        string
	realGH       string
	originalPath string
	listener     net.Listener
	adapters     *adapter.Set
	approve      func(context.Context, ApprovalRequest) (bool, error)
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer

	mu           sync.Mutex
	summary      Summary
	wg           sync.WaitGroup
	finalizeOnce sync.Once
	finalizeErr  error
}

type request struct {
	Token      string   `json:"token"`
	Arguments  []string `json:"arguments"`
	Repository string   `json:"repository,omitempty"`
}

type response struct {
	Execute      bool   `json:"execute"`
	PassThrough  bool   `json:"pass_through"`
	RecordIndex  int    `json:"record_index"`
	Reason       string `json:"reason,omitempty"`
	RealGH       string `json:"real_gh"`
	OriginalPath string `json:"original_path"`
}

type outcome struct {
	Token       string `json:"token"`
	RecordIndex int    `json:"record_index"`
	ExitCode    int    `json:"exit_code"`
	Error       string `json:"error,omitempty"`
	Stdout      string `json:"stdout,omitempty"`
}

// Start creates a per-session gh executable and a private Unix socket.
func Start(options Options) (*Session, error) {
	executable := options.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("find unring executable for gh shim: %w", err)
		}
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve unring executable for gh shim: %w", err)
	}
	realGH, _ := exec.LookPath("gh")
	if realGH != "" {
		realGH, _ = filepath.Abs(realGH)
	}
	directory, err := os.MkdirTemp("", "unring-gh-")
	if err != nil {
		return nil, fmt.Errorf("create per-session gh shim directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("restrict gh shim directory: %w", err)
	}
	if err := os.Symlink(executable, filepath.Join(directory, "gh")); err != nil {
		return nil, fmt.Errorf("create per-session gh shim: %w", err)
	}
	socket := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen for gh shim: %w", err)
	}
	var secret [16]byte
	if _, err := rand.Read(secret[:]); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("create gh shim token: %w", err)
	}
	session := &Session{
		directory: directory, socket: socket, token: hex.EncodeToString(secret[:]),
		realGH: realGH, originalPath: os.Getenv("PATH"), listener: listener,
		adapters: options.Adapters, approve: options.Approve,
		stdin: options.Stdin, stdout: options.Stdout, stderr: options.Stderr,
	}
	cleanup = false
	go session.serve()
	return session, nil
}

// Environment prepends only this session's directory and carries private
// routing metadata. It does not mutate the parent environment.
func (session *Session) Environment(base []string) []string {
	overrides := map[string]string{
		"PATH":    session.directory + string(os.PathListSeparator) + session.originalPath,
		envMarker: "1", envSocket: session.socket, envToken: session.token,
		envRealGH: session.realGH, envOriginalPath: session.originalPath,
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for _, key := range []string{
		"PATH", envMarker, envSocket, envToken, envRealGH, envOriginalPath,
	} {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

// Summary returns a detached snapshot.
func (session *Session) Summary() Summary {
	session.mu.Lock()
	defer session.mu.Unlock()
	return Summary{Records: append([]Record(nil), session.summary.Records...), Sealed: session.summary.Sealed}
}

// Seal stops accepting shim calls after the child exits.
func (session *Session) Seal() error {
	err := session.listener.Close()
	session.wg.Wait()
	session.mu.Lock()
	session.summary.Sealed = true
	session.mu.Unlock()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close removes the ephemeral shim directory.
func (session *Session) Close() error {
	_ = session.Seal()
	return os.RemoveAll(session.directory)
}

// Finalize compensates approved gh mutations on discard. Output-dependent gh
// mutations are never staged: they either ran after explicit approval or did
// not run and returned a non-zero status to their caller.
func (session *Session) Finalize(ctx context.Context, commit bool) error {
	session.finalizeOnce.Do(func() {
		if commit {
			return
		}
		session.mu.Lock()
		var indexes []int
		for index, record := range session.summary.Records {
			if record.State == "ran" && record.UndoEffect != "" {
				indexes = append(indexes, index)
			}
		}
		session.mu.Unlock()

		var undoErrors []error
		for _, index := range indexes {
			if err := ctx.Err(); err != nil {
				undoErrors = append(undoErrors, err)
				break
			}
			session.mu.Lock()
			record := session.summary.Records[index]
			session.mu.Unlock()
			if record.ResourceURL == "" {
				err := errors.New("real gh succeeded but its stdout did not contain a created issue URL")
				session.updateUndo(index, "unavailable", err)
				undoErrors = append(undoErrors, fmt.Errorf(
					"cannot %s; %s: %w", record.UndoEffect, record.StillExists, err))
				continue
			}
			command := exec.CommandContext(ctx, session.realGH, "issue", "close", record.ResourceURL)
			command.Env = clientEnvironment(os.Environ(), session.originalPath)
			command.Stdin = nil
			command.Stdout = session.stdout
			command.Stderr = session.stderr
			err := command.Run()
			if err == nil {
				session.updateUndo(index, "succeeded", nil)
			} else {
				session.updateUndo(index, "failed", err)
				undoErrors = append(undoErrors, fmt.Errorf(
					"%s failed; %s: %w", record.UndoEffect, record.StillExists, err))
			}
		}
		session.finalizeErr = errors.Join(undoErrors...)
	})
	return session.finalizeErr
}

func (session *Session) updateUndo(index int, state string, err error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	record := &session.summary.Records[index]
	record.UndoState = state
	if err != nil {
		record.UndoError = err.Error()
	}
}

func (session *Session) serve() {
	for {
		connection, err := session.listener.Accept()
		if err != nil {
			return
		}
		session.wg.Add(1)
		go func() {
			defer session.wg.Done()
			defer connection.Close()
			session.handle(connection)
		}()
	}
}

func (session *Session) handle(connection net.Conn) {
	decoder := json.NewDecoder(connection)
	var message struct {
		Request *request `json:"request,omitempty"`
		Outcome *outcome `json:"outcome,omitempty"`
	}
	if err := decoder.Decode(&message); err != nil {
		return
	}
	if message.Request != nil {
		reply := session.decide(*message.Request)
		_ = json.NewEncoder(connection).Encode(reply)
		return
	}
	if message.Outcome != nil && message.Outcome.Token == session.token {
		session.recordOutcome(*message.Outcome)
		_ = json.NewEncoder(connection).Encode(map[string]bool{"ok": true})
	}
}

func (session *Session) decide(call request) response {
	reply := response{
		RealGH: session.realGH, OriginalPath: session.originalPath, RecordIndex: -1,
	}
	if call.Token != session.token {
		reply.Reason = "gh shim authentication failed"
		return reply
	}
	parsed, parseErr := parse(call.Arguments, call.Repository)
	if parseErr == nil && parsed.Read {
		reply.Execute = true
		reply.PassThrough = true
		return reply
	}

	classification := adapter.Classification{Tier: adapter.TierNeedsApproval}
	reason := ""
	intentText := "ambiguous gh invocation"
	if parseErr != nil {
		reason = parseErr.Error() + "; unring will not guess what this invocation does"
	} else {
		intentText = parsed.Detail
		var matched bool
		var err error
		if session.adapters != nil {
			classification, matched, err = session.adapters.Classify(adapter.Request{
				Method: parsed.Method, URL: parsed.URL,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   parsed.Body,
			})
		}
		switch {
		case err != nil:
			reason = "adapter classification failed: " + err.Error()
			classification = adapter.Classification{Tier: adapter.TierNeedsApproval}
		case !matched:
			reason = parsed.Reason
			if reason == "" {
				reason = "no adapter recognized the parsed gh mutation"
			}
			classification = adapter.Classification{Tier: adapter.TierNeedsApproval}
		case classification.Tier == adapter.TierStageable:
			reason = fmt.Sprintf(
				"adapter %s rule %s is stageable for HTTPS, but this gh invocation cannot receive a truthful CLI result without running",
				classification.Adapter, classification.Rule)
			classification.Tier = adapter.TierNeedsApproval
		default:
			reason = fmt.Sprintf("adapter %s rule %s classifies this intent as %s",
				classification.Adapter, classification.Rule, classification.Tier)
			if classification.Tier == adapter.TierNeedsApproval {
				reason += "; the command must run to produce its real stdout, so it cannot be staged honestly"
			}
		}
	}
	approved := false
	var approvalErr error
	if session.approve != nil {
		approved, approvalErr = session.approve(context.Background(), ApprovalRequest{
			Invocation: "gh " + strings.Join(call.Arguments, " "),
			Intent:     intentText, Reason: reason,
		})
	}
	decision := "declined"
	state := "not-run"
	if approved {
		decision = "approved"
		state = "approved"
	}
	if approvalErr != nil {
		decision = "error"
		state = "not-run"
		reason += ": " + approvalErr.Error()
	}
	record := Record{
		Arguments: append([]string(nil), call.Arguments...), Intent: intentText,
		Tier: classification.Tier, Adapter: classification.Adapter, Rule: classification.Rule,
		Reason: reason, Decision: decision, State: state, Time: time.Now().UTC(),
	}
	if classification.Undo != nil {
		record.UndoEffect = classification.Undo.Effect
		record.StillExists = classification.Undo.StillExists
	}
	session.mu.Lock()
	reply.RecordIndex = len(session.summary.Records)
	session.summary.Records = append(session.summary.Records, record)
	session.mu.Unlock()
	reply.Execute = approved && approvalErr == nil
	if !reply.Execute {
		reply.Reason = reason
	}
	return reply
}

func (session *Session) recordOutcome(result outcome) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if result.RecordIndex < 0 || result.RecordIndex >= len(session.summary.Records) {
		return
	}
	record := &session.summary.Records[result.RecordIndex]
	record.ExitCode = result.ExitCode
	record.Error = result.Error
	if result.ExitCode == 0 && result.Error == "" {
		record.State = "ran"
		record.ResourceURL = createdIssueURL(result.Stdout)
		if record.UndoEffect != "" {
			if record.ResourceURL == "" {
				record.UndoState = "unavailable"
				record.UndoError = "real gh stdout did not contain a created issue URL"
			} else {
				record.UndoState = "available"
			}
		}
	} else {
		record.State = "failed"
	}
}

// IsInvocation reports whether this process was entered through the private gh
// symlink. Checking both argv[0] and the marker avoids changing normal unring
// behavior merely because it inherited a similarly named variable.
func IsInvocation(argv0 string) bool {
	return os.Getenv(envMarker) == "1" && filepath.Base(argv0) == "gh"
}

// RunClient handles one invocation of the private gh executable.
func RunClient(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	call := request{
		Token: os.Getenv(envToken), Arguments: append([]string(nil), arguments...),
		Repository: os.Getenv("GH_REPO"),
	}
	connection, err := net.Dial("unix", os.Getenv(envSocket))
	if err != nil {
		fmt.Fprintf(stderr, "unring gh shim: control connection failed; invocation was not run: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(connection).Encode(map[string]any{"request": call}); err != nil {
		_ = connection.Close()
		fmt.Fprintf(stderr, "unring gh shim: send intent: %v\n", err)
		return 1
	}
	var reply response
	err = json.NewDecoder(connection).Decode(&reply)
	_ = connection.Close()
	if err != nil {
		fmt.Fprintf(stderr, "unring gh shim: read decision: %v\n", err)
		return 1
	}
	if !reply.Execute {
		fmt.Fprintf(stderr, "unring gh shim: invocation not run: %s\n", reply.Reason)
		return 1
	}
	if reply.RealGH == "" {
		fmt.Fprintln(stderr, "unring gh shim: real gh was not found before the session PATH was injected")
		return 127
	}
	environment := clientEnvironment(os.Environ(), reply.OriginalPath)
	if reply.PassThrough {
		if input, ok := stdin.(*os.File); ok && input == os.Stdin &&
			stdout == os.Stdout && stderr == os.Stderr {
			err := syscall.Exec(reply.RealGH, append([]string{reply.RealGH}, arguments...), environment)
			fmt.Fprintf(stderr, "unring gh shim: execute real gh: %v\n", err)
			return 126
		}
	}
	command := exec.Command(reply.RealGH, arguments...)
	command.Env = environment
	command.Stdin = stdin
	var captured limitedCapture
	command.Stdout = stdout
	if reply.RecordIndex >= 0 && !terminalWriter(stdout) {
		command.Stdout = io.MultiWriter(stdout, &captured)
	}
	command.Stderr = stderr
	runErr := command.Run()
	exitCode := commandExitCode(runErr)
	report := outcome{
		Token: os.Getenv(envToken), RecordIndex: reply.RecordIndex,
		ExitCode: exitCode, Stdout: captured.String(),
	}
	if runErr != nil {
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) {
			report.Error = runErr.Error()
		}
	}
	reportOutcome(report)
	return exitCode
}

const maximumCapturedGHOutput = 1 << 20

type limitedCapture struct {
	data []byte
}

func (capture *limitedCapture) Write(data []byte) (int, error) {
	remaining := maximumCapturedGHOutput - len(capture.data)
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		capture.data = append(capture.data, data[:remaining]...)
	}
	return len(data), nil
}

func (capture *limitedCapture) String() string { return string(capture.data) }

func terminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func createdIssueURL(output string) string {
	fields := strings.Fields(output)
	for index := len(fields) - 1; index >= 0; index-- {
		candidate := strings.Trim(fields[index], "<>[](){}.,;\"'")
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 4 && parts[2] == "issues" && parts[3] != "" {
			return parsed.String()
		}
	}
	return ""
}

func reportOutcome(result outcome) {
	connection, err := net.Dial("unix", os.Getenv(envSocket))
	if err != nil {
		return
	}
	defer connection.Close()
	_ = json.NewEncoder(connection).Encode(map[string]any{"outcome": result})
	var acknowledgment map[string]bool
	_ = json.NewDecoder(connection).Decode(&acknowledgment)
}

func clientEnvironment(base []string, originalPath string) []string {
	remove := map[string]bool{
		envMarker: true, envSocket: true, envToken: true,
		envRealGH: true, envOriginalPath: true, "PATH": true,
	}
	result := make([]string, 0, len(base)+1)
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if !found || !remove[key] {
			result = append(result, entry)
		}
	}
	return append(result, "PATH="+originalPath)
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitError.ExitCode()
	}
	return 127
}
