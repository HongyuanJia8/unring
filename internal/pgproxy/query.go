package pgproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

func (p *Proxy) relayQuery(client *pgproto3.Backend, sql string) {
	p.queryMu.Lock()
	defer p.queryMu.Unlock()

	p.paramsMu.RLock()
	plainStringBackslashEscapes :=
		strings.EqualFold(p.params["standard_conforming_strings"], "off")
	p.paramsMu.RUnlock()
	if reason := unsafeClientSQLMode(sql, plainStringBackslashEscapes); reason != "" {
		client.Send(&pgproto3.ErrorResponse{
			Severity: "ERROR",
			Code:     "0A000",
			Message:  reason,
		})
		client.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		_ = client.Flush()
		p.recordQuery(QueryRecord{SQL: sql, Failed: true})
		return
	}

	savepoint := fmt.Sprintf("%s_%d", p.savepointPrefix, p.queryID.Add(1))
	if _, err := p.internalQueryLocked("SAVEPOINT " + savepoint); err != nil {
		p.markFatal(fmt.Errorf("create query savepoint: %w", err))
		return
	}

	p.frontend.Send(&pgproto3.Query{String: sql})
	if err := p.frontend.Flush(); err != nil {
		p.markFatal(fmt.Errorf("send query to real postgres: %w", err))
		return
	}

	record := QueryRecord{SQL: sql}
	var clientErr error
	var backendFailed bool

	for {
		message, err := p.frontend.Receive()
		if err != nil {
			p.markFatal(fmt.Errorf("receive query result from real postgres: %w", err))
			return
		}

		switch message := message.(type) {
		case *pgproto3.ReadyForQuery:
			if message.TxStatus == 'I' {
				record.Failed = true
				p.recordQuery(record)
				if clientErr == nil {
					client.Send(&pgproto3.ErrorResponse{
						Severity: "FATAL",
						Code:     "XX000",
						Message:  ErrTransactionLost.Error(),
						Detail: "The real PostgreSQL backend reported idle state after a client query; " +
							"unring can no longer promise rollback.",
					})
					_ = client.Flush()
				}
				p.markFatal(ErrTransactionLost)
				return
			}
			if message.TxStatus != 'T' && message.TxStatus != 'E' {
				err := fmt.Errorf("postgres backend reported unknown transaction status %q", message.TxStatus)
				record.Failed = true
				p.recordQuery(record)
				if clientErr == nil {
					client.Send(&pgproto3.ErrorResponse{
						Severity: "FATAL",
						Code:     "XX000",
						Message:  err.Error(),
					})
					_ = client.Flush()
				}
				p.markFatal(err)
				return
			}
			backendFailed = backendFailed || message.TxStatus == 'E'
			goto queryComplete
		case *pgproto3.ErrorResponse:
			backendFailed = true
		case *pgproto3.CommandComplete:
			record.CommandTags = append(record.CommandTags, string(message.CommandTag))
		case *pgproto3.ParameterStatus:
			p.paramsMu.Lock()
			p.params[message.Name] = message.Value
			p.paramsMu.Unlock()
		case *pgproto3.CopyInResponse:
			if clientErr == nil {
				client.Send(message)
				clientErr = client.Flush()
			}
			if err := p.relayCopyIn(client); err != nil {
				clientErr = err
			}
			continue
		case *pgproto3.CopyBothResponse:
			if clientErr == nil {
				client.Send(&pgproto3.ErrorResponse{
					Severity: "FATAL",
					Code:     "0A000",
					Message:  "unring slice 1 does not support PostgreSQL copy-both mode",
				})
				_ = client.Flush()
			}
			p.markFatal(errors.New("real postgres entered unsupported copy-both mode"))
			return
		}

		if clientErr == nil {
			client.Send(message)
			clientErr = client.Flush()
		}
	}

queryComplete:
	record.Failed = backendFailed
	p.recordQuery(record)

	recoverySQL := "RELEASE SAVEPOINT " + savepoint
	if backendFailed {
		recoverySQL = "ROLLBACK TO SAVEPOINT " + savepoint +
			"; RELEASE SAVEPOINT " + savepoint
	}
	if _, err := p.internalQueryLocked(recoverySQL); err != nil {
		p.markFatal(fmt.Errorf("restore shared transaction after client query: %w", err))
		return
	}

	if clientErr == nil {
		client.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		_ = client.Flush()
	}
}

func (p *Proxy) relayCopyIn(client *pgproto3.Backend) error {
	for {
		message, err := client.Receive()
		if err != nil {
			p.frontend.Send(&pgproto3.CopyFail{Message: "unring client disconnected during COPY"})
			_ = p.frontend.Flush()
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("receive COPY data from client: %w", err)
		}

		switch message := message.(type) {
		case *pgproto3.CopyData:
			p.frontend.Send(message)
		case *pgproto3.CopyDone:
			p.frontend.Send(message)
			if err := p.frontend.Flush(); err != nil {
				return fmt.Errorf("finish COPY to real postgres: %w", err)
			}
			return nil
		case *pgproto3.CopyFail:
			p.frontend.Send(message)
			if err := p.frontend.Flush(); err != nil {
				return fmt.Errorf("fail COPY to real postgres: %w", err)
			}
			return nil
		default:
			p.frontend.Send(&pgproto3.CopyFail{
				Message: fmt.Sprintf("unring received unsupported message %T during COPY", message),
			})
			_ = p.frontend.Flush()
			return fmt.Errorf("unsupported client message %T during COPY", message)
		}

		if err := p.frontend.Flush(); err != nil {
			return fmt.Errorf("send COPY data to real postgres: %w", err)
		}
	}
}

func (p *Proxy) internalQueryLocked(sql string) (byte, error) {
	p.frontend.Send(&pgproto3.Query{String: sql})
	if err := p.frontend.Flush(); err != nil {
		return 0, fmt.Errorf("send %q: %w", sql, err)
	}

	var responseErr error
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return 0, fmt.Errorf("receive result for %q: %w", sql, err)
		}
		switch message := message.(type) {
		case *pgproto3.ErrorResponse:
			responseErr = fmt.Errorf("postgres %s: %s (SQLSTATE %s)",
				strings.ToLower(message.Severity), message.Message, message.Code)
		case *pgproto3.CommandComplete:
			// The internal command completed; ReadyForQuery still defines the
			// synchronization boundary.
		case *pgproto3.ParameterStatus:
			p.paramsMu.Lock()
			p.params[message.Name] = message.Value
			p.paramsMu.Unlock()
		case *pgproto3.NoticeResponse, *pgproto3.NotificationResponse:
			// These asynchronous messages are valid between any two protocol
			// messages. Internal maintenance has no client-visible result stream,
			// so there is nowhere meaningful to forward them.
		case *pgproto3.ReadyForQuery:
			if responseErr != nil {
				return message.TxStatus, responseErr
			}
			return message.TxStatus, nil
		default:
			return 0, fmt.Errorf("unexpected backend message %T while running internal query %q",
				message, sql)
		}
	}
}

func (p *Proxy) recordQuery(record QueryRecord) {
	p.summaryMu.Lock()
	p.queries = append(p.queries, record)
	p.summaryMu.Unlock()
}
