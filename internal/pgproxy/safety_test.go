package pgproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestRelayQueryDetectsLostTransaction(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	t.Cleanup(func() {
		_ = proxySide.Close()
		_ = postgresSide.Close()
	})

	proxy := &Proxy{
		upstream:        proxySide,
		frontend:        pgproto3.NewFrontend(proxySide, proxySide),
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
	}

	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)

		message, err := backend.Receive()
		if err != nil {
			serverErrors <- err
			return
		}
		if query, ok := message.(*pgproto3.Query); !ok ||
			!strings.HasPrefix(query.String, "SAVEPOINT unring_testtoken_") {
			serverErrors <- errors.New("first proxy query was not its internal savepoint")
			return
		}
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SAVEPOINT")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}

		message, err = backend.Receive()
		if err != nil {
			serverErrors <- err
			return
		}
		if query, ok := message.(*pgproto3.Query); !ok || query.String != "SELECT 1" {
			serverErrors <- errors.New("second proxy query was not the client query")
			return
		}
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
		}
	}()

	var clientOutput bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &clientOutput))
	proxy.relayQuery(client, "SELECT 1")

	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	if !errors.Is(proxy.Err(), ErrTransactionLost) {
		t.Fatalf("Proxy.Err() = %v, want ErrTransactionLost", proxy.Err())
	}

	summary := proxy.Summary()
	if len(summary.Queries) != 1 || !summary.Queries[0].Failed {
		t.Fatalf("lost transaction summary = %#v, want one failed query", summary)
	}

	clientMessages := pgproto3.NewFrontend(&clientOutput, io.Discard)
	if _, err := clientMessages.Receive(); err != nil {
		t.Fatalf("decode relayed CommandComplete: %v", err)
	}
	message, err := clientMessages.Receive()
	if err != nil {
		t.Fatalf("decode interception-loss error: %v", err)
	}
	errorResponse, ok := message.(*pgproto3.ErrorResponse)
	if !ok || errorResponse.Severity != "FATAL" ||
		!strings.Contains(errorResponse.Message, "interception was lost") {
		t.Fatalf("client interception-loss message = %#v", message)
	}
}

func TestInternalQueryAllowsAsynchronousMessages(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	t.Cleanup(func() {
		_ = proxySide.Close()
		_ = postgresSide.Close()
	})
	proxy := &Proxy{
		upstream: proxySide,
		frontend: pgproto3.NewFrontend(proxySide, proxySide),
		params:   make(map[string]string),
	}

	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		message, err := backend.Receive()
		if err != nil {
			serverErrors <- err
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok || query.String != "SAVEPOINT unring_test" {
			serverErrors <- fmt.Errorf("got %#v, want internal SAVEPOINT query", message)
			return
		}
		backend.Send(&pgproto3.NoticeResponse{
			Severity: "DEBUG",
			Message:  "debug notice from internal savepoint",
		})
		backend.Send(&pgproto3.NotificationResponse{
			PID:     42,
			Channel: "events",
			Payload: "payload",
		})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SAVEPOINT")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
		}
	}()

	status, err := proxy.internalQueryLocked("SAVEPOINT unring_test")
	if err != nil {
		t.Fatalf("internalQueryLocked() rejected asynchronous backend messages: %v", err)
	}
	if status != 'T' {
		t.Fatalf("internalQueryLocked() status = %q, want T", status)
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
}

func TestRandomSavepointPrefix(t *testing.T) {
	t.Parallel()

	first, err := randomSavepointPrefix()
	if err != nil {
		t.Fatalf("first randomSavepointPrefix(): %v", err)
	}
	second, err := randomSavepointPrefix()
	if err != nil {
		t.Fatalf("second randomSavepointPrefix(): %v", err)
	}
	if first == second {
		t.Fatalf("random savepoint prefixes unexpectedly match: %q", first)
	}
	if !strings.HasPrefix(first, "unring_") || len(first) != len("unring_")+32 {
		t.Fatalf("random savepoint prefix has unexpected shape: %q", first)
	}
}

func TestFinalizeRollbackDoesNotHideFatalBackendState(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	defer postgresSide.Close()
	proxy := &Proxy{
		upstream:  proxySide,
		frontend:  pgproto3.NewFrontend(proxySide, proxySide),
		params:    make(map[string]string),
		clients:   make(map[net.Conn]struct{}),
		fatalDone: make(chan struct{}),
	}
	proxy.markFatal(errors.New("backend connection lost"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := proxy.Finalize(ctx, DecisionRollback)
	if err == nil {
		t.Fatal("Finalize(rollback) succeeded after a fatal backend error")
	}
	if !strings.Contains(err.Error(), "backend state is unknown") ||
		!strings.Contains(err.Error(), "backend connection lost") {
		t.Fatalf("Finalize(rollback) error = %v, want explicit unknown outcome", err)
	}
}

func TestSealStopsNewClientsBeforeReview(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxySide, postgresSide := net.Pipe()
	defer proxySide.Close()
	defer postgresSide.Close()

	proxy := &Proxy{
		listener:  listener,
		address:   listener.Addr().String(),
		upstream:  proxySide,
		frontend:  pgproto3.NewFrontend(proxySide, proxySide),
		params:    make(map[string]string),
		clients:   make(map[net.Conn]struct{}),
		fatalDone: make(chan struct{}),
	}
	proxy.acceptWG.Add(1)
	go proxy.acceptLoop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	beforeReview := proxy.Summary()

	connection, err := net.DialTimeout("tcp", proxy.Address(), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("new client connected after the review boundary was sealed")
	}
	afterAttempt := proxy.Summary()
	if beforeReview.Connections != afterAttempt.Connections ||
		len(beforeReview.Queries) != len(afterAttempt.Queries) {
		t.Fatalf("summary changed after Seal: before %#v, after %#v", beforeReview, afterAttempt)
	}
}

func TestClientTransactionTranslationAndAbortedState(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	defer postgresSide.Close()
	proxy := &Proxy{
		upstream:        proxySide,
		frontend:        pgproto3.NewFrontend(proxySide, proxySide),
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
	}
	queries := make(chan string, 64)
	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		for {
			message, err := backend.Receive()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
				errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				serverErrors <- err
				return
			}
			query, ok := message.(*pgproto3.Query)
			if !ok {
				serverErrors <- fmt.Errorf("unexpected backend message %T", message)
				return
			}
			queries <- query.String
			if strings.Contains(query.String, "fail_table") {
				backend.Send(&pgproto3.ErrorResponse{
					Severity: "ERROR", Code: "23505", Message: "duplicate key",
				})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'E'})
			} else {
				tag := strings.ToUpper(strings.Fields(query.String)[0])
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			}
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}
		}
	}()

	var output bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &output))
	for _, sql := range []string{
		"BEGIN",
		"INSERT INTO example VALUES (1)",
		"SAVEPOINT a",
		"INSERT INTO fail_table VALUES (1)",
		"SELECT 1",
		"ROLLBACK TO SAVEPOINT a",
		"INSERT INTO example VALUES (2)",
		"COMMIT",
	} {
		proxy.relayQuery(client, sql)
	}
	if proxy.Err() != nil {
		t.Fatalf("transaction translation marked proxy fatal: %v", proxy.Err())
	}
	if client.transactionSavepoint != "" || client.transactionFailed || client.locked {
		t.Fatalf("client state after COMMIT = %#v, want idle and unlocked", client)
	}
	_ = proxySide.Close()
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	close(queries)
	var backendQueries []string
	for query := range queries {
		backendQueries = append(backendQueries, query)
		upper := strings.ToUpper(strings.TrimSpace(query))
		if upper == "COMMIT" || upper == "ROLLBACK" || upper == "BEGIN" {
			t.Fatalf("client transaction control reached outer backend: %q", query)
		}
	}
	for _, query := range backendQueries {
		if query == "SELECT 1" {
			t.Fatalf("query after error reached backend before rollback: %#v", backendQueries)
		}
	}

	frontend := pgproto3.NewFrontend(&output, io.Discard)
	var statuses []byte
	var sawAbortedError bool
	for {
		message, err := frontend.Receive()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode translated transaction responses: %v", err)
		}
		switch message := message.(type) {
		case *pgproto3.ReadyForQuery:
			statuses = append(statuses, message.TxStatus)
		case *pgproto3.ErrorResponse:
			sawAbortedError = sawAbortedError || message.Code == "25P02"
		}
	}
	if !sawAbortedError {
		t.Fatal("query after error did not receive SQLSTATE 25P02")
	}
	wantStatuses := []byte{'T', 'T', 'T', 'E', 'E', 'T', 'T', 'I'}
	if !bytes.Equal(statuses, wantStatuses) {
		t.Fatalf("ReadyForQuery statuses = %q, want %q", statuses, wantStatuses)
	}
}

func TestClientCancellationReleasesPendingIrreversibleApproval(t *testing.T) {
	t.Parallel()

	approvalStarted := make(chan struct{})
	proxy := &Proxy{
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
		approve: func(context.Context, ApprovalRequest) (bool, error) {
			close(approvalStarted)
			select {}
		},
	}

	var output bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &output))
	client.ctx, client.cancel = context.WithCancel(context.Background())
	proxy.acquireClient(client)
	done := make(chan struct{})
	go func() {
		proxy.executeIrreversibleLocked(client, clientStatement{
			SQL: "VACUUM", Irreversible: "VACUUM cannot run inside a transaction block",
		})
		proxy.releaseClientIfIdle(client)
		close(done)
	}()
	select {
	case <-approvalStarted:
	case <-time.After(time.Second):
		t.Fatal("approval hook did not start")
	}
	client.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not release pending approval")
	}
}

func TestClientDisconnectCancelsWorkWhileHandlerIsBusy(t *testing.T) {
	t.Parallel()

	proxySide, clientSide := net.Pipe()
	backend := pgproto3.NewBackend(proxySide, proxySide)
	client := newClientState(1, backend)
	client.ctx, client.cancel = context.WithCancel(context.Background())
	client.incoming = make(chan clientInput)
	proxy := &Proxy{}
	go proxy.receiveClient(client)

	_ = clientSide.Close()
	select {
	case <-client.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel in-flight work")
	}
	_ = proxySide.Close()
}
