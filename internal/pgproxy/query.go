package pgproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

func (p *Proxy) relayQuery(client *clientState, sql string) {
	p.acquireClient(client)
	defer p.releaseClientIfIdle(client)
	if err := p.clearUnnamedObjectsLocked(client); err != nil {
		p.markFatal(fmt.Errorf("clear unnamed protocol objects before simple query: %w", err))
		return
	}

	statements, err := analyzeClientSQL(sql)
	if err != nil {
		p.sendStatementError(client, "42601", err.Error(), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		p.finishSimpleQuery(client)
		p.recordQuery(QueryRecord{SQL: sql, Failed: true})
		return
	}
	if len(statements) == 0 {
		statements = []clientStatement{{SQL: sql}}
	}
	batchSavepoint := ""
	if client.transactionSavepoint == "" && len(statements) > 1 && allRegularStatements(statements) {
		batchSavepoint = client.nextObjectName(p.savepointPrefix, "b")
		if _, err := p.internalQueryLocked("SAVEPOINT " + batchSavepoint); err != nil {
			p.markFatal(fmt.Errorf("create simple-query batch savepoint: %w", err))
			return
		}
	}

	record := QueryRecord{SQL: sql}
	for _, statement := range statements {
		tags, failed := p.executeStatementLocked(client, statement)
		record.CommandTags = append(record.CommandTags, tags...)
		if failed {
			record.Failed = true
			break
		}
	}
	if batchSavepoint != "" {
		command := "RELEASE SAVEPOINT " + batchSavepoint
		if record.Failed {
			command = "ROLLBACK TO SAVEPOINT " + batchSavepoint +
				"; RELEASE SAVEPOINT " + batchSavepoint
		}
		if _, err := p.internalQueryLocked(command); err != nil {
			p.markFatal(fmt.Errorf("finish simple-query batch: %w", err))
			return
		}
	}
	p.recordQuery(record)
	p.finishSimpleQuery(client)
}

func allRegularStatements(statements []clientStatement) bool {
	for _, statement := range statements {
		if statement.Kind != statementRegular {
			return false
		}
	}
	return true
}

func (p *Proxy) clearUnnamedObjectsLocked(client *clientState) error {
	if portal := client.portals[""]; portal != nil {
		if !portal.statement.synthetic {
			if err := p.closeBackendObjectLocked('P', portal.backendName); err != nil {
				return err
			}
		}
		delete(client.portals, "")
	}
	if prepared := client.prepared[""]; prepared != nil {
		if !prepared.synthetic {
			if err := p.closeBackendObjectLocked('S', prepared.backendName); err != nil {
				return err
			}
		}
		delete(client.prepared, "")
		for name, portal := range client.portals {
			if portal.statement == prepared {
				delete(client.portals, name)
			}
		}
	}
	return nil
}

func (p *Proxy) finishSimpleQuery(client *clientState) {
	client.backend.Send(&pgproto3.ReadyForQuery{TxStatus: client.transactionStatus()})
	_ = client.backend.Flush()
}

func (p *Proxy) executeStatementLocked(
	client *clientState,
	statement clientStatement,
) ([]string, bool) {
	if statement.Kind == statementForbidden {
		p.sendStatementError(client, "0A000",
			"unring cannot allow prepared-transaction control in the shared transaction", true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	if client.transactionFailed && statement.Kind != statementRollback &&
		statement.Kind != statementRollbackTo && statement.Kind != statementCommit {
		p.sendStatementError(client, "25P02",
			"current transaction is aborted, commands ignored until end of transaction block", false)
		return nil, true
	}
	if statement.Kind != statementRegular {
		return p.executeTransactionControlLocked(client, statement)
	}
	if statement.Irreversible != "" {
		return p.executeIrreversibleLocked(client, statement)
	}
	return p.executeRegularLocked(client, statement)
}

func (p *Proxy) executeTransactionControlLocked(
	client *clientState,
	statement clientStatement,
) ([]string, bool) {
	switch statement.Kind {
	case statementBegin:
		if statement.Options {
			p.sendStatementError(client, "0A000",
				"unring cannot emulate transaction characteristics on its shared transaction", true)
			if client.transactionSavepoint != "" {
				client.transactionFailed = true
			}
			return nil, true
		}
		if client.transactionSavepoint != "" {
			client.backend.Send(&pgproto3.NoticeResponse{
				Severity: "WARNING", Code: "25001",
				Message: "there is already a transaction in progress",
			})
			client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
			return []string{"BEGIN"}, false
		}
		if err := p.beginClientTransactionLocked(client); err != nil {
			p.markFatal(err)
			return nil, true
		}
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
		return []string{"BEGIN"}, false

	case statementCommit:
		if client.transactionSavepoint == "" {
			p.noTransactionNotice(client)
			client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")})
			return []string{"COMMIT"}, false
		}
		tag := "COMMIT"
		command := "RELEASE SAVEPOINT " + client.transactionSavepoint
		if client.transactionFailed {
			tag = "ROLLBACK"
			command = "ROLLBACK TO SAVEPOINT " + client.transactionSavepoint +
				"; RELEASE SAVEPOINT " + client.transactionSavepoint
		}
		if _, err := p.internalQueryLocked(command); err != nil {
			p.markFatal(fmt.Errorf("finish client transaction: %w", err))
			return nil, true
		}
		p.clearClientTransaction(client)
		if statement.Chain {
			if err := p.beginClientTransactionLocked(client); err != nil {
				p.markFatal(err)
				return nil, true
			}
		}
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
		return []string{tag}, false

	case statementRollback:
		if client.transactionSavepoint == "" {
			p.noTransactionNotice(client)
			client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
			return []string{"ROLLBACK"}, false
		}
		if _, err := p.internalQueryLocked("ROLLBACK TO SAVEPOINT " + client.transactionSavepoint +
			"; RELEASE SAVEPOINT " + client.transactionSavepoint); err != nil {
			p.markFatal(fmt.Errorf("roll back client transaction: %w", err))
			return nil, true
		}
		p.clearClientTransaction(client)
		if statement.Chain {
			if err := p.beginClientTransactionLocked(client); err != nil {
				p.markFatal(err)
				return nil, true
			}
		}
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
		return []string{"ROLLBACK"}, false

	case statementSavepoint:
		if !p.requireClientTransaction(client) {
			return nil, true
		}
		backendName := client.nextObjectName(p.savepointPrefix, "u")
		if _, err := p.internalQueryLocked("SAVEPOINT " + backendName); err != nil {
			p.markFatal(fmt.Errorf("create client savepoint: %w", err))
			return nil, true
		}
		client.savepoints = append(client.savepoints, clientSavepoint{
			clientName: statement.Savepoint, backendName: backendName,
		})
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SAVEPOINT")})
		return []string{"SAVEPOINT"}, false

	case statementRollbackTo:
		if client.transactionSavepoint == "" {
			p.sendStatementError(client, "25P01",
				"ROLLBACK TO SAVEPOINT can only be used in transaction blocks", false)
			return nil, true
		}
		index := clientSavepointIndex(client, statement.Savepoint)
		if index < 0 {
			client.transactionFailed = true
			p.sendStatementError(client, "3B001",
				fmt.Sprintf("savepoint %q does not exist", statement.Savepoint), false)
			return nil, true
		}
		if _, err := p.internalQueryLocked("ROLLBACK TO SAVEPOINT " +
			client.savepoints[index].backendName); err != nil {
			p.markFatal(fmt.Errorf("roll back to client savepoint: %w", err))
			return nil, true
		}
		client.savepoints = client.savepoints[:index+1]
		client.transactionFailed = false
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
		return []string{"ROLLBACK"}, false

	case statementRelease:
		if !p.requireClientTransaction(client) {
			return nil, true
		}
		index := clientSavepointIndex(client, statement.Savepoint)
		if index < 0 {
			client.transactionFailed = true
			p.sendStatementError(client, "3B001",
				fmt.Sprintf("savepoint %q does not exist", statement.Savepoint), false)
			return nil, true
		}
		if _, err := p.internalQueryLocked("RELEASE SAVEPOINT " +
			client.savepoints[index].backendName); err != nil {
			p.markFatal(fmt.Errorf("release client savepoint: %w", err))
			return nil, true
		}
		client.savepoints = client.savepoints[:index]
		client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("RELEASE")})
		return []string{"RELEASE"}, false
	default:
		p.sendStatementError(client, "XX000", "unring encountered an unknown statement class", true)
		return nil, true
	}
}

func (p *Proxy) beginClientTransactionLocked(client *clientState) error {
	name := client.nextObjectName(p.savepointPrefix, "t")
	if _, err := p.internalQueryLocked("SAVEPOINT " + name); err != nil {
		return fmt.Errorf("begin client transaction: %w", err)
	}
	client.transactionSavepoint = name
	client.transactionFailed = false
	client.savepoints = nil
	return nil
}

func (p *Proxy) clearClientTransaction(client *clientState) {
	client.transactionSavepoint = ""
	client.transactionFailed = false
	client.savepoints = nil
}

func clientSavepointIndex(client *clientState, name string) int {
	for index := len(client.savepoints) - 1; index >= 0; index-- {
		if client.savepoints[index].clientName == name {
			return index
		}
	}
	return -1
}

func (p *Proxy) requireClientTransaction(client *clientState) bool {
	if client.transactionSavepoint == "" {
		p.sendStatementError(client, "25P01",
			"SAVEPOINT can only be used in transaction blocks", false)
		return false
	}
	if client.transactionFailed {
		p.sendStatementError(client, "25P02",
			"current transaction is aborted, commands ignored until end of transaction block", false)
		return false
	}
	return true
}

func (p *Proxy) noTransactionNotice(client *clientState) {
	client.backend.Send(&pgproto3.NoticeResponse{
		Severity: "WARNING", Code: "25P01",
		Message: "there is no transaction in progress",
	})
}

func (p *Proxy) executeRegularLocked(
	client *clientState,
	statement clientStatement,
) ([]string, bool) {
	savepoint := client.nextObjectName(p.savepointPrefix, "q")
	if _, err := p.internalQueryLocked("SAVEPOINT " + savepoint); err != nil {
		p.markFatal(fmt.Errorf("create query savepoint: %w", err))
		return nil, true
	}

	p.frontend.Send(&pgproto3.Query{String: statement.SQL})
	if err := p.frontend.Flush(); err != nil {
		p.markFatal(fmt.Errorf("send query to real postgres: %w", err))
		return nil, true
	}

	var tags []string
	var backendFailed bool
	var clientErr error
	var transactionBlockError *pgproto3.ErrorResponse
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			p.markFatal(fmt.Errorf("receive query result from real postgres: %w", err))
			return tags, true
		}
		switch message := message.(type) {
		case *pgproto3.ReadyForQuery:
			if message.TxStatus == 'I' {
				p.loseTransaction(client, "client query")
				return tags, true
			}
			if message.TxStatus != 'T' && message.TxStatus != 'E' {
				p.markFatal(fmt.Errorf("postgres backend reported unknown transaction status %q", message.TxStatus))
				return tags, true
			}
			backendFailed = backendFailed || message.TxStatus == 'E'
			goto complete
		case *pgproto3.ErrorResponse:
			backendFailed = true
			if message.Code == "25001" {
				transactionBlockError = message
				continue
			}
		case *pgproto3.CommandComplete:
			tags = append(tags, string(message.CommandTag))
		case *pgproto3.ParameterStatus:
			p.updateParameter(message)
		case *pgproto3.CopyInResponse:
			if clientErr == nil {
				client.backend.Send(message)
				clientErr = client.backend.Flush()
			}
			if err := p.relayCopyIn(client.backend); err != nil {
				clientErr = err
			}
			continue
		case *pgproto3.CopyBothResponse:
			p.sendStatementError(client, "0A000",
				"unring does not support PostgreSQL copy-both mode", true)
			p.markFatal(errors.New("real postgres entered unsupported copy-both mode"))
			return tags, true
		}
		if clientErr == nil {
			client.backend.Send(message)
			clientErr = client.backend.Flush()
		}
	}

complete:
	recoverySQL := "RELEASE SAVEPOINT " + savepoint
	if backendFailed {
		recoverySQL = "ROLLBACK TO SAVEPOINT " + savepoint + "; RELEASE SAVEPOINT " + savepoint
	}
	if _, err := p.internalQueryLocked(recoverySQL); err != nil {
		p.markFatal(fmt.Errorf("restore shared transaction after client query: %w", err))
		return tags, true
	}
	if transactionBlockError != nil {
		statement.Irreversible = transactionBlockError.Message
		return p.executeIrreversibleLocked(client, statement)
	}
	if backendFailed && client.transactionSavepoint != "" {
		client.transactionFailed = true
	}
	return tags, backendFailed
}

func (p *Proxy) executeIrreversibleLocked(
	client *clientState,
	statement clientStatement,
) ([]string, bool) {
	approved := false
	var approvalErr error
	if p.approve != nil {
		approved, approvalErr = p.approve(context.Background(), ApprovalRequest{
			SQL: statement.SQL, Reason: statement.Irreversible,
		})
	}
	if approvalErr != nil {
		p.sendStatementError(client, "58000",
			fmt.Sprintf("unring could not obtain approval: %v", approvalErr), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	if !approved {
		p.sendStatementError(client, "57014",
			"unring declined this irreversible PostgreSQL statement; it was not run", true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}

	connection, err := pgconn.ConnectConfig(context.Background(), p.config.Copy())
	if err != nil {
		p.sendStatementError(client, "08001",
			fmt.Sprintf("unring could not open the non-transactional connection: %v", err), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	p.summaryMu.Lock()
	p.irreversibleActions = append(p.irreversibleActions, IrreversibleAction{SQL: statement.SQL})
	p.summaryMu.Unlock()
	results, execErr := connection.Exec(context.Background(), statement.SQL).ReadAll()
	closeErr := connection.Close(context.Background())
	if execErr != nil {
		p.sendPgError(client, execErr)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	if closeErr != nil {
		p.sendStatementError(client, "08006",
			fmt.Sprintf("the irreversible statement ran but its connection did not close cleanly: %v", closeErr), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}

	var tags []string
	for _, result := range results {
		p.sendExternalResult(client, result)
		tag := result.CommandTag.String()
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags, false
}

func (p *Proxy) sendExternalResult(client *clientState, result *pgconn.Result) {
	if len(result.FieldDescriptions) > 0 {
		fields := make([]pgproto3.FieldDescription, len(result.FieldDescriptions))
		for index, field := range result.FieldDescriptions {
			fields[index] = pgproto3.FieldDescription{
				Name: []byte(field.Name), TableOID: field.TableOID,
				TableAttributeNumber: field.TableAttributeNumber,
				DataTypeOID:          field.DataTypeOID, DataTypeSize: field.DataTypeSize,
				TypeModifier: field.TypeModifier, Format: field.Format,
			}
		}
		client.backend.Send(&pgproto3.RowDescription{Fields: fields})
	}
	for _, row := range result.Rows {
		client.backend.Send(&pgproto3.DataRow{Values: row})
	}
	client.backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(result.CommandTag.String())})
}

func (p *Proxy) sendPgError(client *clientState, err error) {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		client.backend.Send(&pgproto3.ErrorResponse{
			Severity: postgresError.Severity, Code: postgresError.Code,
			Message: postgresError.Message, Detail: postgresError.Detail,
			Hint: postgresError.Hint,
		})
		return
	}
	p.sendStatementError(client, "58000", err.Error(), true)
}

func (p *Proxy) sendStatementError(
	client *clientState,
	code, message string,
	includePrefix bool,
) {
	if includePrefix && !strings.HasPrefix(message, "unring") {
		message = "unring: " + message
	}
	client.backend.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR", Code: code, Message: message,
	})
}

func (p *Proxy) loseTransaction(client *clientState, operation string) {
	client.backend.Send(&pgproto3.ErrorResponse{
		Severity: "FATAL", Code: "XX000", Message: ErrTransactionLost.Error(),
		Detail: "The real PostgreSQL backend reported idle state after a " + operation +
			"; unring can no longer promise rollback.",
	})
	_ = client.backend.Flush()
	p.markFatal(ErrTransactionLost)
}

func (p *Proxy) updateParameter(message *pgproto3.ParameterStatus) {
	p.paramsMu.Lock()
	p.params[message.Name] = message.Value
	p.paramsMu.Unlock()
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
		case *pgproto3.ParameterStatus:
			p.updateParameter(message)
		case *pgproto3.NoticeResponse, *pgproto3.NotificationResponse:
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
