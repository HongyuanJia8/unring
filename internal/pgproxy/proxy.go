// Package pgproxy implements the PostgreSQL simple-query proxy used by one
// unring session.
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

// QueryRecord is a compact account of one client query batch.
type QueryRecord struct {
	SQL         string
	CommandTags []string
	Failed      bool
}

// Summary is a point-in-time copy of the session activity.
type Summary struct {
	Connections int
	Queries     []QueryRecord
}

// Proxy owns one real PostgreSQL backend connection and transaction.
type Proxy struct {
	listener net.Listener
	address  string

	upstream net.Conn
	frontend *pgproto3.Frontend
	queryMu  sync.Mutex

	paramsMu sync.RWMutex
	params   map[string]string
	queryID  atomic.Uint64

	savepointPrefix string

	clientsMu sync.Mutex
	clients   map[net.Conn]struct{}
	clientWG  sync.WaitGroup
	acceptWG  sync.WaitGroup

	summaryMu   sync.Mutex
	connections int
	queries     []QueryRecord

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

	p := &Proxy{
		upstream:        hijacked.Conn,
		frontend:        hijacked.Frontend,
		params:          cloneMap(hijacked.ParameterStatuses),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: savepointPrefix,
	}

	if _, err := p.internalQueryLocked("BEGIN"); err != nil {
		_ = p.upstream.Close()
		return nil, fmt.Errorf("begin shared postgres transaction: %w", err)
	}

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
		}
	}
	return Summary{Connections: p.connections, Queries: queries}
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
	if decision == DecisionCommit {
		command = "COMMIT"
	}
	if _, err := p.internalQueryLocked(command); err != nil {
		_ = p.upstream.Close()
		return fmt.Errorf("%s postgres session: %w", strings.ToLower(command), err)
	}
	if err := p.upstream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close postgres backend: %w", err)
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
		p.sendFatal(backend, fmt.Sprintf("unring postgres handshake failed: %v", err))
		return
	}

	for {
		message, err := backend.Receive()
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
			p.relayQuery(backend, message.String)
			if p.Err() != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		default:
			p.sendFatal(backend, fmt.Sprintf(
				"unring slice 1 supports only PostgreSQL simple Query and Terminate messages; received %T",
				message,
			))
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
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			if err := backend.Flush(); err != nil {
				return fmt.Errorf("send startup response: %w", err)
			}
			return nil
		case *pgproto3.CancelRequest:
			return errors.New("query cancellation is not supported in slice 1")
		default:
			return fmt.Errorf("unsupported startup message %T", message)
		}
	}
}

func (p *Proxy) stopClients() {
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

func (p *Proxy) markFatal(err error) {
	p.fatalOnce.Do(func() {
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
