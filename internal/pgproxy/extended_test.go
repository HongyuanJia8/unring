package pgproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

func TestPGXDefaultParameterizedExecuteCompletes(t *testing.T) {
	t.Parallel()

	proxyUpstream, postgresSide := net.Pipe()
	clientSide, proxyClient := net.Pipe()
	proxy := &Proxy{
		upstream:        proxyUpstream,
		frontend:        pgproto3.NewFrontend(proxyUpstream, proxyUpstream),
		params:          map[string]string{"client_encoding": "UTF8", "server_version": "17.0"},
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
	}

	serverErrors := make(chan error, 1)
	go serveParameterizedFakeBackend(postgresSide, serverErrors)
	handlerDone := make(chan struct{})
	proxy.clientWG.Add(1)
	go func() {
		proxy.handleClient(proxyClient)
		close(handlerDone)
	}()

	config, err := pgx.ParseConfig("postgresql://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatalf("parse client config: %v", err)
	}
	var dialed bool
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		if dialed {
			return nil, errors.New("unexpected second client dial")
		}
		dialed = true
		return clientSide, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect protocol client: %v", err)
	}
	if _, err := connection.Exec(ctx, "CREATE TABLE example (id integer, value text)"); err != nil {
		t.Fatalf("unparameterized Exec() did not complete: %v", err)
	}
	if _, err := connection.Exec(ctx,
		"INSERT INTO example (id, value) VALUES ($1, $2)", 1, "value"); err != nil {
		t.Fatalf("parameterized Exec() did not complete: %v", err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatalf("close protocol client: %v", err)
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("proxy client handler did not stop")
	}
	_ = proxyUpstream.Close()
	_ = postgresSide.Close()
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
}

func serveParameterizedFakeBackend(connection net.Conn, errorsFound chan<- error) {
	defer close(errorsFound)
	defer connection.Close()
	backend := pgproto3.NewBackend(connection, connection)
	for {
		message, err := backend.Receive()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			errorsFound <- err
			return
		}
		switch message := message.(type) {
		case *pgproto3.Query:
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.Fields(message.String)[0])})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			if err := backend.Flush(); err != nil {
				errorsFound <- err
				return
			}
		case *pgproto3.Parse:
			backend.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			backend.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			if message.ObjectType == 'S' {
				backend.Send(&pgproto3.ParameterDescription{ParameterOIDs: []uint32{23, 25}})
			}
			backend.Send(&pgproto3.NoData{})
		case *pgproto3.Execute:
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
		case *pgproto3.Close:
			backend.Send(&pgproto3.CloseComplete{})
		case *pgproto3.Flush:
			if err := backend.Flush(); err != nil {
				errorsFound <- err
				return
			}
		case *pgproto3.Sync:
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			if err := backend.Flush(); err != nil {
				errorsFound <- err
				return
			}
		default:
			errorsFound <- fmt.Errorf("unexpected frontend message %T", message)
			return
		}
	}
}

func TestExtendedProtocolRewritesSameNamesPerClient(t *testing.T) {
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

	backendNames := make(chan string, 2)
	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		for clientNumber := 1; clientNumber <= 2; clientNumber++ {
			if err := expectInternalQuery(backend, "SAVEPOINT unring_testtoken_"); err != nil {
				serverErrors <- err
				return
			}

			message, err := receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			parse, ok := message.(*pgproto3.Parse)
			if !ok || parse.Query != "SELECT $1::text" || parse.Name == "s0" || parse.Name == "" {
				serverErrors <- fmt.Errorf("client %d Parse = %#v", clientNumber, message)
				return
			}
			backendNames <- parse.Name
			backend.Send(&pgproto3.ParseComplete{})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			bind, ok := message.(*pgproto3.Bind)
			if !ok || bind.PreparedStatement != parse.Name || bind.DestinationPortal == "p0" ||
				bind.DestinationPortal == "" {
				serverErrors <- fmt.Errorf("client %d Bind = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.BindComplete{})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			describeStatement, ok := message.(*pgproto3.Describe)
			if !ok || describeStatement.ObjectType != 'S' || describeStatement.Name != parse.Name {
				serverErrors <- fmt.Errorf("client %d statement Describe = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.ParameterDescription{ParameterOIDs: []uint32{25}})
			backend.Send(testRowDescription())
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			describePortal, ok := message.(*pgproto3.Describe)
			if !ok || describePortal.ObjectType != 'P' || describePortal.Name != bind.DestinationPortal {
				serverErrors <- fmt.Errorf("client %d portal Describe = %#v", clientNumber, message)
				return
			}
			backend.Send(testRowDescription())
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			execute, ok := message.(*pgproto3.Execute)
			if !ok || execute.Portal != bind.DestinationPortal {
				serverErrors <- fmt.Errorf("client %d Execute = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(strconvItoa(clientNumber))}})
			backend.Send(&pgproto3.PortalSuspended{})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			execute, ok = message.(*pgproto3.Execute)
			if !ok || execute.Portal != bind.DestinationPortal {
				serverErrors <- fmt.Errorf("client %d resumed Execute = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			if _, ok := message.(*pgproto3.Sync); !ok {
				serverErrors <- fmt.Errorf("client %d Sync = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}

			if err := expectInternalQuery(backend, "RELEASE SAVEPOINT unring_testtoken_"); err != nil {
				serverErrors <- err
				return
			}
			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			closeMessage, ok := message.(*pgproto3.Close)
			if !ok || closeMessage.ObjectType != 'P' || closeMessage.Name != bind.DestinationPortal {
				serverErrors <- fmt.Errorf("client %d portal cleanup = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.CloseComplete{})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}
			message, err = receiveNonFlush(backend)
			if err != nil {
				serverErrors <- err
				return
			}
			if _, ok := message.(*pgproto3.Sync); !ok {
				serverErrors <- fmt.Errorf("client %d cleanup Sync = %#v", clientNumber, message)
				return
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
			if err := backend.Flush(); err != nil {
				serverErrors <- err
				return
			}
		}
	}()

	var firstOutput, secondOutput bytes.Buffer
	clients := []*clientState{
		newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &firstOutput)),
		newClientState(2, pgproto3.NewBackend(strings.NewReader(""), &secondOutput)),
	}
	for _, client := range clients {
		proxy.relayExtended(client, &pgproto3.Parse{Name: "s0", Query: "SELECT $1::text"})
		proxy.relayExtended(client, &pgproto3.Bind{
			DestinationPortal: "p0", PreparedStatement: "s0", Parameters: [][]byte{[]byte("value")},
		})
		proxy.relayExtended(client, &pgproto3.Describe{ObjectType: 'S', Name: "s0"})
		proxy.relayExtended(client, &pgproto3.Describe{ObjectType: 'P', Name: "p0"})
		proxy.relayExtended(client, &pgproto3.Execute{Portal: "p0", MaxRows: 1})
		proxy.relayExtended(client, &pgproto3.Execute{Portal: "p0"})
		proxy.relayExtended(client, &pgproto3.Sync{})
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	close(backendNames)
	var names []string
	for name := range backendNames {
		names = append(names, name)
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("rewritten prepared names = %#v, want two unique names", names)
	}
	for index, output := range []*bytes.Buffer{&firstOutput, &secondOutput} {
		frontend := pgproto3.NewFrontend(output, io.Discard)
		var ready *pgproto3.ReadyForQuery
		for {
			message, err := frontend.Receive()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			if err != nil {
				t.Fatalf("decode client %d response: %v", index+1, err)
			}
			if response, ok := message.(*pgproto3.ReadyForQuery); ok {
				ready = response
			}
		}
		if ready == nil || ready.TxStatus != 'I' {
			t.Fatalf("client %d ReadyForQuery = %#v, want idle", index+1, ready)
		}
	}
}

func TestExtendedCopyFromRelaysClientDataWithoutDeadlock(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	defer proxySide.Close()
	defer postgresSide.Close()
	proxy := &Proxy{
		upstream:        proxySide,
		frontend:        pgproto3.NewFrontend(proxySide, proxySide),
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
	}
	var output bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &output))
	client.incoming = make(chan clientInput, 2)
	prepared := &preparedStatement{
		backendName: "copy_statement",
		query:       "COPY example FROM STDIN",
		statement:   clientStatement{SQL: "COPY example FROM STDIN"},
	}
	client.portals["copy_portal"] = &portal{
		backendName: "copy_portal_backend",
		statement:   prepared,
	}
	client.incoming <- clientInput{message: &pgproto3.CopyData{Data: []byte("1\n")}}
	client.incoming <- clientInput{message: &pgproto3.CopyDone{}}

	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		if err := expectInternalQuery(backend, "SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err := receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		execute, ok := message.(*pgproto3.Execute)
		if !ok || execute.Portal != "copy_portal_backend" {
			serverErrors <- fmt.Errorf("extended COPY Execute = %#v", message)
			return
		}
		backend.Send(&pgproto3.CopyInResponse{OverallFormat: pgproto3.TextFormat})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		data, ok := message.(*pgproto3.CopyData)
		if !ok || string(data.Data) != "1\n" {
			serverErrors <- fmt.Errorf("extended COPY data = %#v", message)
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, ok := message.(*pgproto3.CopyDone); !ok {
			serverErrors <- fmt.Errorf("extended COPY completion = %#v", message)
			return
		}
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("COPY 1")})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
		}
	}()

	done := make(chan struct{})
	go func() {
		proxy.relayExtended(client, &pgproto3.Execute{Portal: "copy_portal"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("extended COPY FROM deadlocked")
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	if proxy.Err() != nil {
		t.Fatalf("extended COPY marked proxy fatal: %v", proxy.Err())
	}
	responses := pgproto3.NewFrontend(&output, io.Discard)
	if message, err := responses.Receive(); err != nil {
		t.Fatalf("decode CopyInResponse: %v", err)
	} else if _, ok := message.(*pgproto3.CopyInResponse); !ok {
		t.Fatalf("first extended COPY response = %#v", message)
	}
	if message, err := responses.Receive(); err != nil {
		t.Fatalf("decode COPY CommandComplete: %v", err)
	} else if complete, ok := message.(*pgproto3.CommandComplete); !ok ||
		string(complete.CommandTag) != "COPY 1" {
		t.Fatalf("second extended COPY response = %#v", message)
	}
}

func TestExtendedCopyBothFailsWithoutDeadlock(t *testing.T) {
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
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), io.Discard))
	prepared := &preparedStatement{
		backendName: "both_statement",
		query:       "START_REPLICATION",
		statement:   clientStatement{SQL: "START_REPLICATION"},
	}
	client.portals["both"] = &portal{backendName: "both_backend", statement: prepared}
	go func() {
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		_ = expectInternalQuery(backend, "SAVEPOINT unring_testtoken_")
		_, _ = receiveNonFlush(backend)
		backend.Send(&pgproto3.CopyBothResponse{})
		_ = backend.Flush()
	}()
	done := make(chan struct{})
	go func() {
		proxy.relayExtended(client, &pgproto3.Execute{Portal: "both"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("extended copy-both mode deadlocked")
	}
	if proxy.Err() == nil {
		t.Fatal("extended copy-both mode did not fail closed")
	}
}

func TestSimpleQuerySafelyEndsFailedExtendedCycle(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	defer proxySide.Close()
	defer postgresSide.Close()
	proxy := &Proxy{
		upstream:        proxySide,
		frontend:        pgproto3.NewFrontend(proxySide, proxySide),
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
	}
	var output bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &output))
	client.prepared[""] = &preparedStatement{backendName: "unnamed_backend"}

	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		if err := expectInternalQuery(backend, "SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err := receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, ok := message.(*pgproto3.Sync); !ok {
			serverErrors <- fmt.Errorf("failed-cycle synchronization = %#v", message)
			return
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'E'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		if err := expectInternalQuery(backend, "ROLLBACK TO SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		closeMessage, ok := message.(*pgproto3.Close)
		if !ok || closeMessage.Name != "unnamed_backend" {
			serverErrors <- fmt.Errorf("unnamed close after Sync = %#v", message)
			return
		}
		backend.Send(&pgproto3.CloseComplete{})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		if err := expectInternalQuery(backend, "SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok || query.String != "SELECT 1" {
			serverErrors <- fmt.Errorf("simple query after failed cycle = %#v", message)
			return
		}
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		if err := expectInternalQuery(backend, "RELEASE SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
		}
	}()

	proxy.startExtended(client)
	client.extendedFailed = true
	done := make(chan struct{})
	go func() {
		proxy.relayQuery(client, "SELECT 1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("simple query hung behind failed extended cycle")
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	if proxy.Err() != nil {
		t.Fatalf("mixed protocol marked proxy fatal: %v", proxy.Err())
	}
}

func TestExtendedTransactionBlockErrorDefersApprovalUntilSync(t *testing.T) {
	t.Parallel()

	proxySide, postgresSide := net.Pipe()
	defer proxySide.Close()
	defer postgresSide.Close()
	var approvals atomic.Int32
	proxy := &Proxy{
		upstream:        proxySide,
		frontend:        pgproto3.NewFrontend(proxySide, proxySide),
		params:          make(map[string]string),
		clients:         make(map[net.Conn]struct{}),
		fatalDone:       make(chan struct{}),
		savepointPrefix: "unring_testtoken",
		approve: func(context.Context, ApprovalRequest) (bool, error) {
			approvals.Add(1)
			return false, nil
		},
	}
	var output bytes.Buffer
	client := newClientState(1, pgproto3.NewBackend(strings.NewReader(""), &output))
	prepared := &preparedStatement{
		backendName: "checkpoint_statement",
		query:       "CHECKPOINT",
		statement:   clientStatement{SQL: "CHECKPOINT"},
	}
	client.portals["checkpoint_portal"] = &portal{
		backendName: "checkpoint_portal_backend",
		statement:   prepared,
	}

	serverErrors := make(chan error, 1)
	go func() {
		defer close(serverErrors)
		backend := pgproto3.NewBackend(postgresSide, postgresSide)
		if err := expectInternalQuery(backend, "SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err := receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, ok := message.(*pgproto3.Execute); !ok {
			serverErrors <- fmt.Errorf("extended escape Execute = %#v", message)
			return
		}
		backend.Send(&pgproto3.ErrorResponse{
			Severity: "ERROR", Code: "25001",
			Message: "CHECKPOINT cannot run inside a transaction block",
		})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, ok := message.(*pgproto3.Sync); !ok {
			serverErrors <- fmt.Errorf("extended escape Sync = %#v", message)
			return
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'E'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
			return
		}
		if err := expectInternalQuery(backend, "ROLLBACK TO SAVEPOINT unring_testtoken_"); err != nil {
			serverErrors <- err
			return
		}
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		closeMessage, ok := message.(*pgproto3.Close)
		if !ok || closeMessage.ObjectType != 'P' ||
			closeMessage.Name != "checkpoint_portal_backend" {
			serverErrors <- fmt.Errorf("extended escape portal cleanup = %#v", message)
			return
		}
		backend.Send(&pgproto3.CloseComplete{})
		message, err = receiveNonFlush(backend)
		if err != nil {
			serverErrors <- err
			return
		}
		if _, ok := message.(*pgproto3.Sync); !ok {
			serverErrors <- fmt.Errorf("extended escape cleanup Sync = %#v", message)
			return
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		if err := backend.Flush(); err != nil {
			serverErrors <- err
		}
	}()

	proxy.relayExtended(client, &pgproto3.Execute{Portal: "checkpoint_portal"})
	if approvals.Load() != 0 {
		t.Fatal("extended transaction-block error requested approval before Sync")
	}
	proxy.relayExtended(client, &pgproto3.Sync{})
	if approvals.Load() != 1 {
		t.Fatalf("extended transaction-block approvals = %d, want 1", approvals.Load())
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake postgres server: %v", err)
	}
	if proxy.Err() != nil {
		t.Fatalf("extended escape marked proxy fatal: %v", proxy.Err())
	}
	responses := pgproto3.NewFrontend(&output, io.Discard)
	message, err := responses.Receive()
	if err != nil {
		t.Fatalf("decode deferred approval result: %v", err)
	}
	decline, ok := message.(*pgproto3.ErrorResponse)
	if !ok || decline.Code != "57014" {
		t.Fatalf("deferred approval result = %#v, want local decline", message)
	}
	message, err = responses.Receive()
	if err != nil {
		t.Fatalf("decode ReadyForQuery after deferred approval: %v", err)
	}
	if ready, ok := message.(*pgproto3.ReadyForQuery); !ok || ready.TxStatus != 'I' {
		t.Fatalf("deferred approval ReadyForQuery = %#v", message)
	}
}

func testRowDescription() *pgproto3.RowDescription {
	return &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
		Name: []byte("value"), DataTypeOID: 25, DataTypeSize: -1,
		TypeModifier: -1, Format: pgproto3.TextFormat,
	}}}
}

func expectInternalQuery(backend *pgproto3.Backend, prefix string) error {
	message, err := receiveNonFlush(backend)
	if err != nil {
		return err
	}
	query, ok := message.(*pgproto3.Query)
	if !ok || !strings.HasPrefix(query.String, prefix) {
		return fmt.Errorf("internal query = %#v, want prefix %q", message, prefix)
	}
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.Fields(prefix)[0])})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
	return backend.Flush()
}

func receiveNonFlush(backend *pgproto3.Backend) (pgproto3.FrontendMessage, error) {
	for {
		message, err := backend.Receive()
		if err != nil {
			return nil, err
		}
		if _, ok := message.(*pgproto3.Flush); ok {
			continue
		}
		return message, nil
	}
}

func strconvItoa(value int) string {
	return fmt.Sprintf("%d", value)
}
