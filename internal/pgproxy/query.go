package pgproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

func (p *Proxy) relayQuery(client *clientState, sql string) {
	p.acquireClient(client)
	defer p.releaseClientIfIdle(client)
	if client.extended {
		if err := p.finishExtendedCycleLocked(client, false); err != nil {
			p.markFatal(fmt.Errorf("finish extended-query cycle before simple query: %w", err))
			return
		}
	}
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
		p.recordUnintercepted(sql, err.Error())
		return
	}
	if len(statements) == 0 {
		statements = []clientStatement{{SQL: sql}}
	}
	batchSavepoint := ""
	var batchRows rowLedgerSnapshot
	if client.transactionSavepoint == "" && len(statements) > 1 && allRegularStatements(statements) {
		batchSavepoint = client.nextObjectName(p.savepointPrefix, "b")
		if _, err := p.internalQueryLocked("SAVEPOINT " + batchSavepoint); err != nil {
			p.markFatal(fmt.Errorf("create simple-query batch savepoint: %w", err))
			return
		}
		batchRows = cloneRowLedger(p.rowLedger)
	}

	record := QueryRecord{SQL: sql}
	for _, statement := range statements {
		client.lastError = ""
		tags, failed := p.executeStatementLocked(client, statement)
		record.CommandTags = append(record.CommandTags, tags...)
		if failed {
			record.Failed = true
			record.Error = client.lastError
			break
		}
	}
	if p.Err() != nil {
		return
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
		if record.Failed {
			p.restoreRowLedgerLocked(batchRows)
			p.reconcileRowChangesLocked(false)
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
	if statement.Refusal != "" {
		p.sendStatementError(client, "0A000", statement.Refusal, false)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
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
	if p.sharedLeaseHeldByOther(client) {
		if !statement.ReadOnly {
			p.sendStatementError(client, "55P03",
				"another client has an open transaction; unring cannot safely interleave this statement",
				true)
			return nil, true
		}
		statement.RollbackAfter = true
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
		if p.sharedLeaseHeldByOther(client) {
			p.sendStatementError(client, "55P03",
				"another client already has an open transaction", true)
			return nil, true
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
		if client.transactionFailed {
			p.restoreRowLedgerLocked(client.transactionRows)
			p.reconcileRowChangesLocked(false)
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
		p.restoreRowLedgerLocked(client.transactionRows)
		p.reconcileRowChangesLocked(false)
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
			rows: cloneRowLedger(p.rowLedger),
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
		p.restoreRowLedgerLocked(client.savepoints[index].rows)
		p.reconcileRowChangesLocked(false)
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
	client.transactionRows = cloneRowLedger(p.rowLedger)
	p.activeTransaction = client.id
	client.transactionFailed = false
	client.savepoints = nil
	return nil
}

func (p *Proxy) clearClientTransaction(client *clientState) {
	if p.activeTransaction == client.id {
		p.activeTransaction = 0
	}
	client.transactionSavepoint = ""
	client.transactionFailed = false
	client.transactionRows = nil
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

func (p *Proxy) sharedLeaseHeldByOther(client *clientState) bool {
	return (p.activeTransaction != 0 && p.activeTransaction != client.id) ||
		(p.escapeClient != 0 && p.escapeClient != client.id)
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
				p.loseTransaction(client, statement.SQL, "client query")
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
			client.lastError = postgresErrorText(message)
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
			if err := p.relayCopyIn(client); err != nil {
				clientErr = err
			}
			continue
		case *pgproto3.CopyBothResponse:
			p.recordUnintercepted(statement.SQL, "PostgreSQL copy-both traffic is unsupported")
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
	if backendFailed || statement.RollbackAfter {
		recoverySQL = "ROLLBACK TO SAVEPOINT " + savepoint + "; RELEASE SAVEPOINT " + savepoint
	}
	if _, err := p.internalQueryLocked(recoverySQL); err != nil {
		p.markFatal(fmt.Errorf("restore shared transaction after client query: %w", err))
		return tags, true
	}
	p.reconcileRowChangesLocked(!backendFailed && !statement.RollbackAfter)
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
	// The approval prompt and the side connection must not pin the shared
	// backend. In particular, Seal must be able to cancel this client's context
	// while an external command is waiting for a lock.
	p.releaseClient(client)
	defer func() {
		p.acquireClient(client)
		if p.escapeClient == client.id {
			p.escapeClient = 0
		}
	}()

	approved := false
	var approvalErr error
	if p.approve != nil {
		approved, approvalErr = p.requestApproval(client.ctx, ApprovalRequest{
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
	actionIndex := p.beginIrreversibleAction(statement.SQL)

	p.acquireClient(client)
	sessionState, stateErr := p.escapeSessionStateLocked()
	if stateErr == nil {
		p.escapeClient = client.id
	}
	p.releaseClient(client)
	if stateErr != nil {
		p.finishIrreversibleAction(actionIndex, nil, stateErr)
		p.sendStatementError(client, "58000",
			fmt.Sprintf("unring could not capture session state for the irreversible statement: %v",
				stateErr), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	if sessionState.hasUncommittedChanges {
		err := errors.New("shared transaction contains uncommitted database changes")
		p.finishIrreversibleAction(actionIndex, nil, err)
		p.sendStatementError(client, "25001",
			"unring cannot safely run this irreversible statement while the shared transaction "+
				"contains uncommitted database changes", true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}

	connection, err := pgconn.ConnectConfig(client.ctx, p.config.Copy())
	if err != nil {
		p.finishIrreversibleAction(actionIndex, nil, err)
		p.sendStatementError(client, "08001",
			fmt.Sprintf("unring could not open the non-transactional connection: %v", err), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = connection.Close(closeContext)
	}()
	if err := applyEscapeSessionState(client.ctx, connection, sessionState); err != nil {
		p.finishIrreversibleAction(actionIndex, nil, err)
		p.sendStatementError(client, "08001",
			fmt.Sprintf("unring could not mirror session state on the non-transactional connection: %v",
				err), true)
		if client.transactionSavepoint != "" {
			client.transactionFailed = true
		}
		return nil, true
	}
	results, execErr := connection.Exec(client.ctx, statement.SQL).ReadAll()
	if execErr != nil {
		p.finishIrreversibleAction(actionIndex, nil, execErr)
		p.sendPgError(client, execErr)
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
	p.finishIrreversibleAction(actionIndex, tags, nil)
	return tags, false
}

func (p *Proxy) beginIrreversibleAction(sql string) int {
	p.summaryMu.Lock()
	defer p.summaryMu.Unlock()
	p.irreversibleActions = append(p.irreversibleActions, IrreversibleAction{SQL: sql})
	return len(p.irreversibleActions) - 1
}

func (p *Proxy) finishIrreversibleAction(index int, tags []string, err error) {
	p.summaryMu.Lock()
	defer p.summaryMu.Unlock()
	if index < 0 || index >= len(p.irreversibleActions) {
		return
	}
	action := &p.irreversibleActions[index]
	action.CommandTags = append([]string(nil), tags...)
	if err != nil {
		action.Failed = true
		action.Error = err.Error()
	}
}

func (p *Proxy) requestApproval(
	ctx context.Context,
	request ApprovalRequest,
) (bool, error) {
	type result struct {
		approved bool
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		approved, err := p.approve(ctx, request)
		completed <- result{approved: approved, err: err}
	}()
	select {
	case outcome := <-completed:
		return outcome.approved, outcome.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

type escapeSessionState struct {
	sessionAuthorization  string
	role                  string
	settings              map[string]string
	hasUncommittedChanges bool
}

func (p *Proxy) escapeSessionStateLocked() (escapeSessionState, error) {
	rows, err := p.internalRowsLocked(
		"SELECT '__unring_session_authorization__', session_user " +
			"UNION ALL SELECT '__unring_role__', current_user " +
			"UNION ALL SELECT '__unring_transaction_id__', " +
			"COALESCE(txid_current_if_assigned()::text, '') " +
			"UNION ALL SELECT name, setting FROM pg_settings " +
			"WHERE (source = 'session' OR name = 'search_path') " +
			"AND name NOT IN ('role', 'session_authorization', " +
			"'transaction_isolation', 'transaction_read_only', " +
			"'transaction_deferrable') ORDER BY 1",
	)
	if err != nil {
		return escapeSessionState{}, err
	}
	state := escapeSessionState{settings: make(map[string]string)}
	for _, row := range rows {
		if len(row) != 2 || row[0] == nil || row[1] == nil {
			return escapeSessionState{}, errors.New("postgres returned malformed session state")
		}
		name, value := string(row[0]), string(row[1])
		switch name {
		case "__unring_session_authorization__":
			state.sessionAuthorization = value
		case "__unring_role__":
			state.role = value
		case "__unring_transaction_id__":
			state.hasUncommittedChanges = value != ""
		default:
			// Transaction characteristics are not session state. In
			// particular, the escape connection intentionally has no
			// transaction in which these settings could be applied.
			if !transactionScopedSetting(name) {
				state.settings[name] = value
			}
		}
	}
	if state.sessionAuthorization == "" || state.role == "" {
		return escapeSessionState{}, errors.New("postgres omitted session authorization or role")
	}
	return state, nil
}

func applyEscapeSessionState(
	ctx context.Context,
	connection *pgconn.PgConn,
	state escapeSessionState,
) error {
	names := make([]string, 0, len(state.settings))
	for name := range state.settings {
		if transactionScopedSetting(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result := connection.ExecParams(ctx, "SELECT set_config($1, $2, false)",
			[][]byte{[]byte(name), []byte(state.settings[name])},
			[]uint32{25, 25}, []int16{0, 0}, []int16{0}).Read()
		if result.Err != nil {
			return fmt.Errorf("set %s: %w", name, result.Err)
		}
	}
	if _, err := connection.Exec(ctx, "SET SESSION AUTHORIZATION "+
		quoteIdentifier(state.sessionAuthorization)).ReadAll(); err != nil {
		return fmt.Errorf("set session authorization: %w", err)
	}
	if state.role != state.sessionAuthorization {
		if _, err := connection.Exec(ctx, "SET ROLE "+
			quoteIdentifier(state.role)).ReadAll(); err != nil {
			return fmt.Errorf("set role: %w", err)
		}
	}
	// An irreversible command must not wait forever for locks held by unring's
	// own shared transaction. This still permits arbitrarily long work once the
	// command has acquired its locks.
	if _, err := connection.Exec(ctx, "SET lock_timeout = '1s'").ReadAll(); err != nil {
		return fmt.Errorf("set escape lock timeout: %w", err)
	}
	return nil
}

func transactionScopedSetting(name string) bool {
	switch strings.ToLower(name) {
	case "transaction_isolation", "transaction_read_only", "transaction_deferrable":
		return true
	default:
		return false
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
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
	client.lastError = err.Error()
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		client.lastError = fmt.Sprintf("%s (SQLSTATE %s)", postgresError.Message, postgresError.Code)
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
	client.lastError = fmt.Sprintf("%s (SQLSTATE %s)", message, code)
	client.backend.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR", Code: code, Message: message,
	})
}

func (p *Proxy) loseTransaction(client *clientState, statement, operation string) {
	client.backend.Send(&pgproto3.ErrorResponse{
		Severity: "FATAL", Code: "XX000", Message: ErrTransactionLost.Error(),
		Detail: "The real PostgreSQL backend reported idle state after a " + operation +
			"; unring can no longer promise rollback.",
	})
	_ = client.backend.Flush()
	p.markFatal(ErrTransactionLost)
	p.summaryMu.Lock()
	if count := len(p.unintercepted); count > 0 && p.unintercepted[count-1].Statement == "" {
		p.unintercepted[count-1].Statement = statement
	}
	p.summaryMu.Unlock()
}

func postgresErrorText(message *pgproto3.ErrorResponse) string {
	if message == nil {
		return "PostgreSQL statement failed"
	}
	return fmt.Sprintf("%s (SQLSTATE %s)", message.Message, message.Code)
}

func (p *Proxy) updateParameter(message *pgproto3.ParameterStatus) {
	p.paramsMu.Lock()
	p.params[message.Name] = message.Value
	p.paramsMu.Unlock()
}

func (p *Proxy) relayCopyIn(client *clientState) error {
	for {
		var message pgproto3.FrontendMessage
		var err error
		if client.incoming == nil {
			message, err = client.backend.Receive()
		} else {
			input, ok := <-client.incoming
			if !ok {
				err = net.ErrClosed
			} else {
				message, err = input.message, input.err
			}
		}
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

func (p *Proxy) internalRowsLocked(sql string) ([][][]byte, error) {
	p.frontend.Send(&pgproto3.Query{String: sql})
	if err := p.frontend.Flush(); err != nil {
		return nil, fmt.Errorf("send %q: %w", sql, err)
	}
	var rows [][][]byte
	var responseErr error
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return nil, fmt.Errorf("receive result for %q: %w", sql, err)
		}
		switch message := message.(type) {
		case *pgproto3.RowDescription, *pgproto3.CommandComplete:
		case *pgproto3.DataRow:
			row := make([][]byte, len(message.Values))
			for index, value := range message.Values {
				if value != nil {
					row[index] = make([]byte, len(value))
					copy(row[index], value)
				}
			}
			rows = append(rows, row)
		case *pgproto3.ErrorResponse:
			responseErr = fmt.Errorf("postgres %s: %s (SQLSTATE %s)",
				strings.ToLower(message.Severity), message.Message, message.Code)
		case *pgproto3.ParameterStatus:
			p.updateParameter(message)
		case *pgproto3.NoticeResponse, *pgproto3.NotificationResponse:
		case *pgproto3.ReadyForQuery:
			if responseErr != nil {
				return nil, responseErr
			}
			if message.TxStatus != 'T' {
				return nil, fmt.Errorf("session-state query ended with transaction status %q",
					message.TxStatus)
			}
			return rows, nil
		default:
			return nil, fmt.Errorf("unexpected backend message %T while running row query %q",
				message, sql)
		}
	}
}

func (p *Proxy) recordQuery(record QueryRecord) {
	p.summaryMu.Lock()
	p.queries = append(p.queries, record)
	p.summaryMu.Unlock()
}
