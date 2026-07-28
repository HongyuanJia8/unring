// Package pgproxy implements the PostgreSQL proxy used by one unring session.
package pgproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Decision is the one final decision for a session.
type Decision string

const (
	DecisionCommit   Decision = "commit"
	DecisionRollback Decision = "rollback"
)

// ErrTransactionLost means the real backend is no longer inside unring's
// shared transaction. The session outcome cannot be claimed as reversible.
var ErrTransactionLost = errors.New(
	"postgres backend left unring's shared transaction; interception was lost",
)

const (
	minimumPostgresMajor   = 14
	minimumPostgresVersion = minimumPostgresMajor * 10000
)

// QueryRecord is a compact account of one client query batch.
type QueryRecord struct {
	SQL         string   `json:"sql"`
	CommandTags []string `json:"command_tags,omitempty"`
	Failed      bool     `json:"failed"`
	Error       string   `json:"error,omitempty"`
}

// IrreversibleAction records a statement dispatched on a separate autocommit
// connection. Failed records retain the execution error because a dispatched
// statement may have produced external or partially committed effects. A
// decline or pre-execution refusal never creates this record.
type IrreversibleAction struct {
	SQL         string   `json:"sql"`
	CommandTags []string `json:"command_tags,omitempty"`
	Failed      bool     `json:"failed"`
	Error       string   `json:"error,omitempty"`
}

// RowChange reports physical tuple operations still staged by the shared
// transaction. PostgreSQL supplies per-relation counters, including writes
// performed by triggers; unring maintains the savepoint-aware staged view.
type RowChange struct {
	Table    string `json:"table"`
	Inserted int64  `json:"inserted"`
	Updated  int64  `json:"updated"`
	Deleted  int64  `json:"deleted"`
}

// SchemaChange is one catalog change staged in the shared transaction.
type SchemaChange struct {
	Action string `json:"action"`
	Kind   string `json:"kind"`
	Object string `json:"object"`
}

// ChangeSummary is frozen by Seal after all client traffic has stopped.
// Complete is false when PostgreSQL could not provide an authoritative view;
// callers must surface Error rather than treating an empty summary as no work.
type ChangeSummary struct {
	Rows     []RowChange    `json:"rows"`
	Schema   []SchemaChange `json:"schema"`
	Complete bool           `json:"complete"`
	Error    string         `json:"error,omitempty"`
}

// NonTransactionalEffect records backend-observed state that PostgreSQL does
// not roll back with the shared transaction (currently sequence advancement).
type NonTransactionalEffect struct {
	Detail string `json:"detail"`
}

// UninterceptedItem describes traffic unring could not classify or intercept.
// It is deliberately separate from QueryRecord so review UIs cannot blend it
// into the ordinary statement list.
type UninterceptedItem struct {
	Statement string `json:"statement,omitempty"`
	Detail    string `json:"detail"`
}

// ApprovalRequest describes an action that cannot be included in unring's
// shared transaction.
type ApprovalRequest struct {
	SQL    string
	Reason string
}

// ApprovalFunc asks whether an irreversible statement may run. Returning
// false is a normal decline; errors are reported to the client.
type ApprovalFunc func(context.Context, ApprovalRequest) (bool, error)

// Options configures behavior that requires the embedding application.
type Options struct {
	Approve ApprovalFunc
}

// Summary is a point-in-time copy of the session activity.
type Summary struct {
	Connections int           `json:"connections"`
	Queries     []QueryRecord `json:"queries"`
	// FullyReversible is false exactly when IrreversibleActions is non-empty.
	// Other coverage and accounting warnings are represented separately.
	FullyReversible     bool                     `json:"fully_reversible"`
	IrreversibleActions []IrreversibleAction     `json:"irreversible_actions"`
	Changes             ChangeSummary            `json:"changes"`
	Unintercepted       []UninterceptedItem      `json:"unintercepted"`
	NonTransactional    []NonTransactionalEffect `json:"non_transactional_effects"`
	Sealed              bool                     `json:"sealed"`
}

// HasReviewableActivity reports whether the sealed session needs a decision.
// An incomplete or unclassified session is never silently treated as read-only.
func (summary Summary) HasReviewableActivity() bool {
	if !summary.Sealed || !summary.Changes.Complete ||
		len(summary.IrreversibleActions) > 0 || len(summary.Unintercepted) > 0 ||
		len(summary.NonTransactional) > 0 {
		return true
	}
	for _, change := range summary.Changes.Rows {
		if change.Inserted != 0 || change.Updated != 0 || change.Deleted != 0 {
			return true
		}
	}
	return len(summary.Changes.Schema) > 0
}

// Proxy owns one real PostgreSQL backend connection and transaction.
type Proxy struct {
	listener net.Listener
	address  string

	upstream          net.Conn
	frontend          *pgproto3.Frontend
	queryMu           sync.Mutex
	activeTransaction uint64
	escapeClient      uint64
	config            *pgconn.Config
	approve           ApprovalFunc
	runCtx            context.Context
	cancel            context.CancelFunc

	paramsMu sync.RWMutex
	params   map[string]string
	clientID atomic.Uint64

	savepointPrefix string

	clientsMu sync.Mutex
	clients   map[net.Conn]struct{}
	clientWG  sync.WaitGroup
	acceptWG  sync.WaitGroup

	summaryMu            sync.Mutex
	connections          int
	queries              []QueryRecord
	irreversibleActions  []IrreversibleAction
	unintercepted        []UninterceptedItem
	changes              ChangeSummary
	sealedSummary        bool
	catalogInitial       catalogSnapshot
	rowStats             rowStatsSnapshot
	rowLedger            rowLedgerSnapshot
	rowLedgerErr         error
	uncertainEffects     []string
	sequenceEffects      map[string]struct{}
	sequenceSuppressions map[string]sequenceSuppression
	serverVersion        int

	finishMu sync.Mutex
	finished bool

	sealOnce sync.Once
	sealErr  error

	fatalOnce sync.Once
	fatalMu   sync.Mutex
	fatalErr  error
	fatalDone chan struct{}

	backendKey atomic.Uint32
}

// Start connects to the real database, starts the shared transaction, and
// begins listening on an ephemeral loopback port.
func Start(ctx context.Context, config *pgconn.Config) (*Proxy, error) {
	return StartWithOptions(ctx, config, Options{})
}

// StartWithOptions is Start with an approval hook for irreversible actions.
func StartWithOptions(ctx context.Context, config *pgconn.Config, options Options) (*Proxy, error) {
	if config == nil {
		return nil, errors.New("start postgres proxy: nil backend config")
	}

	savepointPrefix, err := randomSavepointPrefix()
	if err != nil {
		return nil, fmt.Errorf("create private savepoint namespace: %w", err)
	}

	pgConn, err := pgconn.ConnectConfig(ctx, config.Copy())
	if err != nil {
		return nil, fmt.Errorf("connect to real postgres: %w", err)
	}

	hijacked, err := pgConn.Hijack()
	if err != nil {
		_ = pgConn.Close(ctx)
		return nil, fmt.Errorf("take ownership of postgres connection: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		upstream:        hijacked.Conn,
		frontend:        hijacked.Frontend,
		params:          cloneMap(hijacked.ParameterStatuses),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: savepointPrefix,
		config:          config.Copy(),
		approve:         options.Approve,
		runCtx:          runCtx,
		cancel:          cancel,
	}

	// READ COMMITTED is deliberate. The baseline is captured explicitly below,
	// while later catalog queries must see approved DDL committed by the escape
	// connection. A transaction-wide snapshot would hide that DDL and pin xmin,
	// preventing CREATE INDEX CONCURRENTLY from completing.
	if _, err := p.internalQueryLocked("BEGIN"); err != nil {
		_ = p.upstream.Close()
		return nil, fmt.Errorf("begin shared postgres transaction: %w", err)
	}
	p.serverVersion, err = p.captureServerVersionLocked()
	if err != nil {
		_, _ = p.internalQueryLocked("ROLLBACK")
		_ = p.upstream.Close()
		return nil, fmt.Errorf("determine PostgreSQL server version: %w", err)
	}
	if err := validatePostgresVersion(p.serverVersion, p.params["server_version"]); err != nil {
		_, _ = p.internalQueryLocked("ROLLBACK")
		_ = p.upstream.Close()
		return nil, err
	}
	p.catalogInitial, err = p.captureCatalogLocked()
	if err != nil {
		_, _ = p.internalQueryLocked("ROLLBACK")
		_ = p.upstream.Close()
		return nil, fmt.Errorf("capture initial postgres catalog: %w", err)
	}
	p.rowStats, err = p.captureRowStatsLocked()
	if err != nil {
		_, _ = p.internalQueryLocked("ROLLBACK")
		_ = p.upstream.Close()
		return nil, fmt.Errorf("capture initial postgres row counters: %w", err)
	}
	p.rowLedger = make(rowLedgerSnapshot)
	p.sequenceEffects = make(map[string]struct{})
	p.sequenceSuppressions = make(map[string]sequenceSuppression)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_, _ = p.internalQueryLocked("ROLLBACK")
		_ = p.upstream.Close()
		return nil, fmt.Errorf("listen for postgres clients: %w", err)
	}
	p.listener = listener
	p.address = listener.Addr().String()

	p.acceptWG.Add(1)
	go p.acceptLoop()

	return p, nil
}

func validatePostgresVersion(versionNumber int, versionLabel string) error {
	if versionNumber >= minimumPostgresVersion {
		return nil
	}
	if versionLabel == "" {
		versionLabel = strconv.Itoa(versionNumber)
	}
	return fmt.Errorf(
		"unring requires PostgreSQL %d or newer; connected server is PostgreSQL %s",
		minimumPostgresMajor, versionLabel,
	)
}

// Address returns the loopback host and ephemeral port used by the proxy.
func (p *Proxy) Address() string {
	return p.address
}

// Done is closed if the real backend connection becomes unusable.
func (p *Proxy) Done() <-chan struct{} {
	return p.fatalDone
}

// Err reports why the backend connection became unusable, if it did.
func (p *Proxy) Err() error {
	p.fatalMu.Lock()
	defer p.fatalMu.Unlock()
	return p.fatalErr
}

// Summary returns a copy safe for use while clients are active.
func (p *Proxy) Summary() Summary {
	p.summaryMu.Lock()
	defer p.summaryMu.Unlock()

	queries := make([]QueryRecord, len(p.queries))
	for i, record := range p.queries {
		queries[i] = QueryRecord{
			SQL:         record.SQL,
			CommandTags: append([]string(nil), record.CommandTags...),
			Failed:      record.Failed,
			Error:       record.Error,
		}
	}
	actions := make([]IrreversibleAction, len(p.irreversibleActions))
	for i, action := range p.irreversibleActions {
		actions[i] = action
		actions[i].CommandTags = append([]string(nil), action.CommandTags...)
	}
	rows := append([]RowChange(nil), p.changes.Rows...)
	schema := append([]SchemaChange(nil), p.changes.Schema...)
	unintercepted := append([]UninterceptedItem(nil), p.unintercepted...)
	nonTransactional := make([]NonTransactionalEffect, 0, len(p.sequenceEffects))
	for name := range p.sequenceEffects {
		nonTransactional = append(nonTransactional, NonTransactionalEffect{
			Detail: "PostgreSQL sequence " + name + " advanced; sequence values do not roll back.",
		})
	}
	sort.Slice(nonTransactional, func(i, j int) bool {
		return nonTransactional[i].Detail < nonTransactional[j].Detail
	})
	// This flag has one deliberately narrow meaning: at least one statement
	// was dispatched on the non-transactional escape connection. Coverage
	// warnings, incomplete staged summaries, sequence effects, declines, and
	// local refusals have their own fields and must not counterfeit this stamp.
	fullyReversible := len(actions) == 0
	return Summary{
		Connections:         p.connections,
		Queries:             queries,
		FullyReversible:     fullyReversible,
		IrreversibleActions: actions,
		Changes: ChangeSummary{
			Rows: rows, Schema: schema, Complete: p.changes.Complete, Error: p.changes.Error,
		},
		Unintercepted:    unintercepted,
		NonTransactional: nonTransactional,
		Sealed:           p.sealedSummary,
	}
}

// Seal stops accepting clients, disconnects existing clients, and waits for
// any in-flight query to finish. A summary taken after Seal is the exact set of
// queries eligible for the final decision.
func (p *Proxy) Seal(ctx context.Context) error {
	p.sealOnce.Do(func() {
		if deadline, ok := ctx.Deadline(); ok {
			_ = p.upstream.SetDeadline(deadline)
			defer p.upstream.SetDeadline(time.Time{})
		}

		p.stopClients()

		// stopClients waits for handlers, but taking the lock also documents and
		// enforces the invariant that no backend exchange remains in flight.
		p.queryMu.Lock()
		if p.Err() == nil {
			p.freezeChangeSummaryLocked()
		}
		p.queryMu.Unlock()

		if fatalErr := p.Err(); fatalErr != nil {
			p.sealErr = fmt.Errorf("seal postgres session after interception failure: %w", fatalErr)
		}
	})
	return p.sealErr
}

// Finalize stops client traffic and applies the session's single decision.
func (p *Proxy) Finalize(ctx context.Context, decision Decision) error {
	if decision != DecisionCommit && decision != DecisionRollback {
		return fmt.Errorf("finalize postgres session: invalid decision %q", decision)
	}

	p.finishMu.Lock()
	defer p.finishMu.Unlock()
	if p.finished {
		return errors.New("finalize postgres session: session already finalized")
	}
	p.finished = true

	if err := p.Seal(ctx); err != nil {
		_ = p.upstream.Close()
		return fmt.Errorf(
			"cannot confirm postgres session was %s because its backend state is unknown: %w",
			pastDecision(decision),
			err,
		)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = p.upstream.SetDeadline(deadline)
		defer p.upstream.SetDeadline(time.Time{})
	}

	p.queryMu.Lock()
	defer p.queryMu.Unlock()

	if fatalErr := p.Err(); fatalErr != nil {
		_ = p.upstream.Close()
		return fmt.Errorf(
			"cannot confirm postgres session was %s because its backend state is unknown: %w",
			pastDecision(decision),
			fatalErr,
		)
	}

	command := "ROLLBACK"
	var forcedRollback error
	if decision == DecisionCommit {
		if p.Summary().Changes.Complete {
			command = "COMMIT"
		} else {
			forcedRollback = errors.New(
				"unring refused to commit an incomplete change summary and rolled the session back",
			)
		}
	}
	if _, err := p.internalQueryLocked(command); err != nil {
		_ = p.upstream.Close()
		return fmt.Errorf("%s postgres session: %w", strings.ToLower(command), err)
	}
	if err := p.upstream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close postgres backend: %w", err)
	}
	if forcedRollback != nil {
		return forcedRollback
	}
	return nil
}

func pastDecision(decision Decision) string {
	if decision == DecisionCommit {
		return "committed"
	}
	return "rolled back"
}

// Close rolls back an undecided session. It is safe to defer immediately after
// Start; a successful Finalize makes it a no-op.
func (p *Proxy) Close() error {
	p.finishMu.Lock()
	if p.finished {
		p.finishMu.Unlock()
		return nil
	}
	p.finishMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Finalize(ctx, DecisionRollback)
}

func (p *Proxy) acceptLoop() {
	defer p.acceptWG.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.markFatal(fmt.Errorf("accept postgres client: %w", err))
			return
		}

		p.clientsMu.Lock()
		p.clients[conn] = struct{}{}
		p.clientsMu.Unlock()

		p.summaryMu.Lock()
		p.connections++
		p.summaryMu.Unlock()

		p.clientWG.Add(1)
		go p.handleClient(conn)
	}
}

func (p *Proxy) handleClient(conn net.Conn) {
	defer p.clientWG.Done()
	defer func() {
		p.clientsMu.Lock()
		delete(p.clients, conn)
		p.clientsMu.Unlock()
		_ = conn.Close()
	}()

	backend := pgproto3.NewBackend(conn, conn)
	if err := p.handshake(conn, backend); err != nil {
		p.recordUnintercepted("", "PostgreSQL startup traffic was not intercepted: "+err.Error())
		p.sendFatal(backend, fmt.Sprintf("unring postgres handshake failed: %v", err))
		return
	}

	clientContext, cancelClient := context.WithCancel(p.context())
	client := newClientState(p.clientID.Add(1), backend)
	client.ctx = clientContext
	client.cancel = cancelClient
	client.incoming = make(chan clientInput)
	go p.receiveClient(client)
	defer cancelClient()
	defer p.cleanupClient(client)

	for {
		input, ok := <-client.incoming
		if !ok {
			return
		}
		message, err := input.message, input.err
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
				errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			p.sendFatal(backend, fmt.Sprintf("unring could not decode client message: %v", err))
			return
		}

		switch message := message.(type) {
		case *pgproto3.Query:
			p.relayQuery(client, message.String)
			if p.Err() != nil {
				return
			}
		case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe,
			*pgproto3.Execute, *pgproto3.Close, *pgproto3.Flush,
			*pgproto3.Sync:
			p.relayExtended(client, message)
			if p.Err() != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		default:
			p.recordUnintercepted("", fmt.Sprintf(
				"unsupported PostgreSQL frontend message %T", message,
			))
			p.sendFatal(backend, fmt.Sprintf(
				"unring does not support PostgreSQL frontend message %T",
				message,
			))
			return
		}
	}
}

func (p *Proxy) receiveClient(client *clientState) {
	defer close(client.incoming)
	for {
		message, err := client.backend.Receive()
		if err != nil {
			client.cancel()
		}
		input := clientInput{message: message, err: err}
		select {
		case client.incoming <- input:
		case <-client.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *Proxy) handshake(conn net.Conn, backend *pgproto3.Backend) error {
	for {
		message, err := backend.ReceiveStartupMessage()
		if err != nil {
			return fmt.Errorf("receive startup message: %w", err)
		}
		switch message.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return fmt.Errorf("decline transport encryption: %w", err)
			}
			continue
		case *pgproto3.StartupMessage:
			backend.Send(&pgproto3.AuthenticationOk{})

			p.paramsMu.RLock()
			names := make([]string, 0, len(p.params))
			for name := range p.params {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				backend.Send(&pgproto3.ParameterStatus{Name: name, Value: p.params[name]})
			}
			p.paramsMu.RUnlock()

			key := p.backendKey.Add(1)
			backend.Send(&pgproto3.BackendKeyData{
				ProcessID: uint32(time.Now().UnixNano()),
				SecretKey: key,
			})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := backend.Flush(); err != nil {
				return fmt.Errorf("send startup response: %w", err)
			}
			return nil
		case *pgproto3.CancelRequest:
			return errors.New("query cancellation is not supported")
		default:
			return fmt.Errorf("unsupported startup message %T", message)
		}
	}
}

func (p *Proxy) stopClients() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.acceptWG.Wait()

	p.clientsMu.Lock()
	for conn := range p.clients {
		_ = conn.Close()
	}
	p.clientsMu.Unlock()
	p.clientWG.Wait()
}

func (p *Proxy) context() context.Context {
	if p.runCtx != nil {
		return p.runCtx
	}
	return context.Background()
}

func (p *Proxy) markFatal(err error) {
	p.fatalOnce.Do(func() {
		p.recordUnintercepted("", "PostgreSQL interception failed: "+err.Error())
		p.fatalMu.Lock()
		p.fatalErr = err
		p.fatalMu.Unlock()
		_ = p.upstream.Close()
		close(p.fatalDone)
	})
}

func (p *Proxy) sendFatal(backend *pgproto3.Backend, message string) {
	backend.Send(&pgproto3.ErrorResponse{
		Severity: "FATAL",
		Code:     "0A000",
		Message:  message,
	})
	_ = backend.Flush()
}

func cloneMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func randomSavepointPrefix() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return "unring_" + hex.EncodeToString(entropy[:]), nil
}
