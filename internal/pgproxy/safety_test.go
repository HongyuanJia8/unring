package pgproxy

import (
	"bytes"
	"context"
	"errors"
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
		upstream:  proxySide,
		frontend:  pgproto3.NewFrontend(proxySide, proxySide),
		params:    make(map[string]string),
		clients:   make(map[net.Conn]struct{}),
		fatalDone: make(chan struct{}),
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
			!strings.HasPrefix(query.String, "SAVEPOINT unring_internal_") {
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
	client := pgproto3.NewBackend(strings.NewReader(""), &clientOutput)
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
