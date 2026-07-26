package pgproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

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

			message, err := backend.Receive()
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

			message, err = backend.Receive()
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

			message, err = backend.Receive()
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

			message, err = backend.Receive()
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

			message, err = backend.Receive()
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

			message, err = backend.Receive()
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

			message, err = backend.Receive()
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
			message, err = backend.Receive()
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
			message, err = backend.Receive()
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

func testRowDescription() *pgproto3.RowDescription {
	return &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
		Name: []byte("value"), DataTypeOID: 25, DataTypeSize: -1,
		TypeModifier: -1, Format: pgproto3.TextFormat,
	}}}
}

func expectInternalQuery(backend *pgproto3.Backend, prefix string) error {
	message, err := backend.Receive()
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

func strconvItoa(value int) string {
	return fmt.Sprintf("%d", value)
}
