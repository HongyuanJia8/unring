package pgproxy

import (
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/jackc/pgx/v5/pgproto3"
)

func (p *Proxy) relayExtended(client *clientState, message pgproto3.FrontendMessage) {
	if _, ok := message.(*pgproto3.Flush); ok {
		_ = client.backend.Flush()
		return
	}
	if _, ok := message.(*pgproto3.Sync); !ok && !client.extended {
		p.startExtended(client)
		if p.Err() != nil {
			return
		}
	}

	if client.extendedFailed {
		if _, ok := message.(*pgproto3.Sync); !ok {
			return
		}
	}

	switch message := message.(type) {
	case *pgproto3.Parse:
		p.extendedParse(client, message)
	case *pgproto3.Bind:
		p.extendedBind(client, message)
	case *pgproto3.Describe:
		p.extendedDescribe(client, message)
	case *pgproto3.Execute:
		p.extendedExecute(client, message)
	case *pgproto3.Close:
		p.extendedClose(client, message)
	case *pgproto3.Sync:
		p.extendedSync(client)
	}
}

func (p *Proxy) startExtended(client *clientState) {
	p.acquireClient(client)
	client.extended = true
	client.extendedFailed = false
	client.cycleSavepoint = client.nextObjectName(p.savepointPrefix, "x")
	if _, err := p.internalQueryLocked("SAVEPOINT " + client.cycleSavepoint); err != nil {
		p.markFatal(fmt.Errorf("start extended-query cycle: %w", err))
		return
	}
	client.cycleRows = cloneRowLedger(p.rowLedger)
	client.cycleUncertainBase = len(p.uncertainEffects)
}

func (p *Proxy) extendedParse(client *clientState, message *pgproto3.Parse) {
	statements, err := analyzeClientSQL(message.Query)
	if err != nil || len(statements) > 1 {
		text := "cannot insert multiple commands into a prepared statement"
		if err != nil {
			text = err.Error()
			p.recordUnintercepted(message.Query, text)
		}
		p.extendedLocalError(client, "42601", text)
		return
	}
	statement := clientStatement{SQL: message.Query}
	if len(statements) == 1 {
		statement = statements[0]
	}
	if p.escapeLeaseHeldByOther(client) {
		p.extendedLocalError(client, "55P03",
			"another client is running an approved non-transactional statement; unring cannot safely interleave this statement")
		return
	}
	p.prepareStatementRiskLocked(&statement)
	if statement.Kind == statementForbidden {
		p.extendedLocalError(client, "0A000",
			"unring cannot allow prepared-transaction control in the shared transaction")
		return
	}

	if existing := client.prepared[message.Name]; existing != nil {
		if message.Name != "" {
			p.extendedLocalError(client, "42P05", fmt.Sprintf(
				"prepared statement %q already exists", message.Name))
			return
		} else {
			if !existing.synthetic {
				if err := p.closeBackendObjectLocked('S', existing.backendName); err != nil {
					p.markFatal(err)
					return
				}
			}
			delete(client.prepared, message.Name)
			for name, portal := range client.portals {
				if portal.statement == existing {
					delete(client.portals, name)
				}
			}
		}
	}

	prepared := &preparedStatement{
		backendName: client.nextObjectName(p.savepointPrefix, "s"),
		query:       message.Query,
		statement:   statement,
		synthetic: statement.Kind != statementRegular || statement.Irreversible != "" ||
			len(statement.TruncateTargets) > 0 ||
			statement.Refusal != "",
	}
	if prepared.synthetic {
		client.prepared[message.Name] = prepared
		client.backend.Send(&pgproto3.ParseComplete{})
		return
	}

	forward := *message
	forward.Name = prepared.backendName
	response, ok := p.exchangeExtendedLocked(client, &forward, func(message pgproto3.BackendMessage) bool {
		switch message.(type) {
		case *pgproto3.ParseComplete, *pgproto3.ErrorResponse:
			return true
		default:
			return false
		}
	})
	if ok {
		if _, failed := response.(*pgproto3.ErrorResponse); !failed {
			client.prepared[message.Name] = prepared
		}
	}
}

func (p *Proxy) extendedBind(client *clientState, message *pgproto3.Bind) {
	prepared := client.prepared[message.PreparedStatement]
	if prepared == nil {
		p.extendedLocalError(client, "26000", fmt.Sprintf(
			"prepared statement %q does not exist", message.PreparedStatement))
		return
	}
	if existing := client.portals[message.DestinationPortal]; existing != nil {
		if message.DestinationPortal != "" {
			p.extendedLocalError(client, "42P03", fmt.Sprintf(
				"portal %q already exists", message.DestinationPortal))
			return
		}
		if !existing.statement.synthetic {
			if err := p.closeBackendObjectLocked('P', existing.backendName); err != nil {
				p.markFatal(err)
				return
			}
		}
		delete(client.portals, message.DestinationPortal)
	}

	bound := &portal{
		backendName: client.nextObjectName(p.savepointPrefix, "p"),
		statement:   prepared,
	}
	if prepared.synthetic {
		if len(message.Parameters) != 0 {
			p.extendedLocalError(client, "08P01",
				"this unring-managed statement does not accept bind parameters")
			return
		}
		client.portals[message.DestinationPortal] = bound
		client.backend.Send(&pgproto3.BindComplete{})
		return
	}

	forward := *message
	forward.DestinationPortal = bound.backendName
	forward.PreparedStatement = prepared.backendName
	response, ok := p.exchangeExtendedLocked(client, &forward, func(message pgproto3.BackendMessage) bool {
		switch message.(type) {
		case *pgproto3.BindComplete, *pgproto3.ErrorResponse:
			return true
		default:
			return false
		}
	})
	if ok {
		if _, failed := response.(*pgproto3.ErrorResponse); !failed {
			client.portals[message.DestinationPortal] = bound
		}
	}
}

func (p *Proxy) extendedDescribe(client *clientState, message *pgproto3.Describe) {
	var backendName string
	var synthetic bool
	switch message.ObjectType {
	case 'S':
		prepared := client.prepared[message.Name]
		if prepared == nil {
			p.extendedLocalError(client, "26000",
				fmt.Sprintf("prepared statement %q does not exist", message.Name))
			return
		}
		backendName, synthetic = prepared.backendName, prepared.synthetic
		if synthetic {
			client.backend.Send(&pgproto3.ParameterDescription{})
			client.backend.Send(&pgproto3.NoData{})
			return
		}
	case 'P':
		portal := client.portals[message.Name]
		if portal == nil {
			p.extendedLocalError(client, "34000", fmt.Sprintf("portal %q does not exist", message.Name))
			return
		}
		backendName, synthetic = portal.backendName, portal.statement.synthetic
		if synthetic {
			client.backend.Send(&pgproto3.NoData{})
			return
		}
	default:
		p.extendedLocalError(client, "08P01", "invalid Describe object type")
		return
	}

	forward := *message
	forward.Name = backendName
	p.exchangeExtendedLocked(client, &forward, func(message pgproto3.BackendMessage) bool {
		switch message.(type) {
		case *pgproto3.RowDescription, *pgproto3.NoData, *pgproto3.ErrorResponse:
			return true
		default:
			return false
		}
	})
}

func (p *Proxy) extendedExecute(client *clientState, message *pgproto3.Execute) {
	client.lastError = ""
	portal := client.portals[message.Portal]
	if portal == nil {
		p.extendedLocalError(client, "34000", fmt.Sprintf("portal %q does not exist", message.Portal))
		return
	}
	statement := portal.statement.statement
	if statement.Kind == statementRegular && p.escapeLeaseHeldByOther(client) {
		p.extendedLocalError(client, "55P03",
			"another client is running an approved non-transactional statement; unring cannot safely interleave this statement")
		return
	}
	if statement.Kind == statementRegular &&
		p.sharedLeaseHeldByOther(client) {
		if !statement.ReadOnly {
			p.extendedLocalError(client, "55P03",
				"another client has an open transaction; unring cannot safely interleave this statement")
			return
		}
		client.rollbackCycle = true
	}
	if client.transactionFailed && statement.Kind != statementRollback &&
		statement.Kind != statementRollbackTo && statement.Kind != statementCommit {
		p.extendedLocalError(client, "25P02",
			"current transaction is aborted, commands ignored until end of transaction block")
		return
	}
	if portal.statement.synthetic {
		if statement.Kind != statementRegular || statement.Irreversible != "" {
			if err := p.rotateExtendedCycleSavepointLocked(client, false); err != nil {
				p.markFatal(err)
				return
			}
		}
		tags, failed := p.executeStatementLocked(client, statement)
		if (statement.Kind != statementRegular || statement.Irreversible != "") && p.Err() == nil {
			if err := p.rotateExtendedCycleSavepointLocked(client, true); err != nil {
				p.markFatal(err)
				return
			}
		}
		record := QueryRecord{SQL: portal.statement.query, CommandTags: tags, Failed: failed}
		if failed {
			record.Error = client.lastError
		}
		p.recordQuery(record)
		if failed {
			client.extendedFailed = true
		}
		return
	}

	forward := *message
	forward.Portal = portal.backendName
	record := QueryRecord{SQL: portal.statement.query}
	response, ok := p.exchangeExtendedLocked(client, &forward, func(message pgproto3.BackendMessage) bool {
		switch current := message.(type) {
		case *pgproto3.CommandComplete:
			record.CommandTags = append(record.CommandTags, string(current.CommandTag))
			return true
		case *pgproto3.EmptyQueryResponse, *pgproto3.PortalSuspended, *pgproto3.ErrorResponse:
			return true
		default:
			return false
		}
	})
	if ok {
		if responseError, failed := response.(*pgproto3.ErrorResponse); failed &&
			responseError.Code == "25001" {
			pending := portal.statement.statement
			pending.Irreversible = responseError.Message
			client.pendingEscape = &pending
			return
		}
		_, record.Failed = response.(*pgproto3.ErrorResponse)
		if responseError, failed := response.(*pgproto3.ErrorResponse); failed {
			record.Error = postgresErrorText(responseError)
		}
		p.recordQuery(record)
		if !record.Failed && summaryRiskApplies(statement, record.CommandTags) {
			client.cycleUncertain = append(client.cycleUncertain, statement.SummaryRisk)
		}
	}
}

func (p *Proxy) rotateExtendedCycleSavepointLocked(client *clientState, create bool) error {
	if client.cycleSavepoint != "" {
		keep := !client.extendedFailed && !client.rollbackCycle
		command := "RELEASE SAVEPOINT " + client.cycleSavepoint
		if !keep {
			command = "ROLLBACK TO SAVEPOINT " + client.cycleSavepoint +
				"; RELEASE SAVEPOINT " + client.cycleSavepoint
		}
		if _, err := p.internalQueryLocked(command); err != nil {
			return fmt.Errorf("release extended-query savepoint around transaction control: %w", err)
		}
		if keep {
			for _, detail := range client.cycleUncertain {
				p.addUncertainEffectLocked(detail)
			}
		} else {
			p.restoreRowLedgerLocked(client.cycleRows)
			p.restoreUncertainEffectsLocked(client.cycleUncertainBase)
		}
		p.reconcileRowChangesLocked(keep && len(client.cycleUncertain) == 0)
		client.cycleUncertain = nil
		client.cycleRows = nil
		client.cycleUncertainBase = 0
		client.cycleSavepoint = ""
	}
	if !create {
		return nil
	}
	client.cycleSavepoint = client.nextObjectName(p.savepointPrefix, "x")
	if _, err := p.internalQueryLocked("SAVEPOINT " + client.cycleSavepoint); err != nil {
		return fmt.Errorf("restore extended-query savepoint after transaction control: %w", err)
	}
	client.cycleRows = cloneRowLedger(p.rowLedger)
	client.cycleUncertainBase = len(p.uncertainEffects)
	return nil
}

func (p *Proxy) extendedClose(client *clientState, message *pgproto3.Close) {
	switch message.ObjectType {
	case 'S':
		prepared := client.prepared[message.Name]
		if prepared == nil {
			client.backend.Send(&pgproto3.CloseComplete{})
			return
		}
		if err := p.closePreparedLocked(client, message.Name, prepared); err != nil {
			p.markFatal(err)
		}
	case 'P':
		portal := client.portals[message.Name]
		if portal == nil {
			client.backend.Send(&pgproto3.CloseComplete{})
			return
		}
		if err := p.closePortalLocked(client, message.Name, portal); err != nil {
			p.markFatal(err)
		}
	default:
		p.extendedLocalError(client, "08P01", "invalid Close object type")
	}
}

func (p *Proxy) closePreparedLocked(
	client *clientState,
	clientName string,
	prepared *preparedStatement,
) error {
	if !prepared.synthetic {
		if err := p.closeBackendObjectLocked('S', prepared.backendName); err != nil {
			return err
		}
	}
	client.backend.Send(&pgproto3.CloseComplete{})
	delete(client.prepared, clientName)
	for name, portal := range client.portals {
		if portal.statement == prepared {
			delete(client.portals, name)
		}
	}
	return nil
}

func (p *Proxy) closePortalLocked(client *clientState, clientName string, portal *portal) error {
	if !portal.statement.synthetic {
		if err := p.closeBackendObjectLocked('P', portal.backendName); err != nil {
			return err
		}
	}
	client.backend.Send(&pgproto3.CloseComplete{})
	delete(client.portals, clientName)
	return nil
}

func (p *Proxy) closeBackendObjectLocked(objectType byte, name string) error {
	p.frontend.Send(&pgproto3.Close{ObjectType: objectType, Name: name})
	p.frontend.Send(&pgproto3.Flush{})
	if err := p.frontend.Flush(); err != nil {
		return fmt.Errorf("send backend object close: %w", err)
	}
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return fmt.Errorf("receive backend object close: %w", err)
		}
		switch message := message.(type) {
		case *pgproto3.CloseComplete:
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("postgres error closing protocol object: %s (SQLSTATE %s)",
				message.Message, message.Code)
		case *pgproto3.ParameterStatus:
			p.updateParameter(message)
		}
	}
}

func (p *Proxy) exchangeExtendedLocked(
	client *clientState,
	message pgproto3.FrontendMessage,
	terminal func(pgproto3.BackendMessage) bool,
) (pgproto3.BackendMessage, bool) {
	p.frontend.Send(message)
	// PostgreSQL may buffer every extended-query response until it receives
	// Flush or Sync. This proxy processes one frontend message at a time, so it
	// must request an upstream flush before synchronously waiting for that
	// message's response. Without this, pgx Prepare deadlocks after Parse.
	p.frontend.Send(&pgproto3.Flush{})
	if err := p.frontend.Flush(); err != nil {
		p.markFatal(fmt.Errorf("send %T to real postgres: %w", message, err))
		return nil, false
	}
	for {
		response, err := p.frontend.Receive()
		if err != nil {
			p.markFatal(fmt.Errorf("receive response to %T from real postgres: %w", message, err))
			return nil, false
		}
		switch response := response.(type) {
		case *pgproto3.ParameterStatus:
			p.updateParameter(response)
		case *pgproto3.ErrorResponse:
			client.extendedFailed = true
			if _, executing := message.(*pgproto3.Execute); executing &&
				response.Code == "25001" && terminal(response) {
				return response, true
			}
		case *pgproto3.CopyInResponse:
			client.backend.Send(response)
			if err := client.backend.Flush(); err != nil {
				p.markFatal(fmt.Errorf("send copy-in response to client: %w", err))
				return nil, false
			}
			if err := p.relayCopyIn(client); err != nil {
				if !errors.Is(err, net.ErrClosed) {
					p.markFatal(fmt.Errorf("relay extended copy-in data: %w", err))
				}
				return nil, false
			}
			continue
		case *pgproto3.CopyBothResponse:
			p.recordUnintercepted("", "PostgreSQL copy-both traffic is unsupported")
			p.sendStatementError(client, "0A000",
				"unring does not support PostgreSQL copy-both mode", true)
			_ = client.backend.Flush()
			p.markFatal(errors.New("real postgres entered unsupported copy-both mode"))
			return nil, false
		}
		client.backend.Send(response)
		if terminal(response) {
			if err := client.backend.Flush(); err != nil {
				p.markFatal(fmt.Errorf("flush response to %T to client: %w", message, err))
				return nil, false
			}
			return response, true
		}
	}
}

func (p *Proxy) extendedLocalError(client *clientState, code, message string) {
	p.sendStatementError(client, code, message, false)
	client.extendedFailed = true
}

func (p *Proxy) extendedSync(client *clientState) {
	if !client.extended {
		client.backend.Send(&pgproto3.ReadyForQuery{TxStatus: client.transactionStatus()})
		_ = client.backend.Flush()
		return
	}
	if err := p.finishExtendedCycleLocked(client, true); err != nil {
		p.markFatal(err)
	}
}

func (p *Proxy) finishExtendedCycleLocked(client *clientState, sendReady bool) error {
	if err := p.synchronizeExtendedBackendLocked(client); err != nil {
		return err
	}
	keepRows := !client.extendedFailed && !client.rollbackCycle
	recovery := "RELEASE SAVEPOINT " + client.cycleSavepoint
	if !keepRows {
		recovery = "ROLLBACK TO SAVEPOINT " + client.cycleSavepoint +
			"; RELEASE SAVEPOINT " + client.cycleSavepoint
	}
	if _, err := p.internalQueryLocked(recovery); err != nil {
		return fmt.Errorf("finish extended-query cycle: %w", err)
	}
	if keepRows {
		for _, detail := range client.cycleUncertain {
			p.addUncertainEffectLocked(detail)
		}
	} else {
		p.restoreRowLedgerLocked(client.cycleRows)
		p.restoreUncertainEffectsLocked(client.cycleUncertainBase)
	}
	p.reconcileRowChangesLocked(keepRows && len(client.cycleUncertain) == 0)
	if client.extendedFailed && client.pendingEscape == nil &&
		client.transactionSavepoint != "" {
		client.transactionFailed = true
	}
	client.extended = false
	client.extendedFailed = false
	client.rollbackCycle = false
	client.cycleSavepoint = ""
	client.cycleUncertain = nil
	client.cycleRows = nil
	client.cycleUncertainBase = 0

	if client.pendingEscape != nil {
		statement := *client.pendingEscape
		client.pendingEscape = nil
		tags, failed := p.executeIrreversibleLocked(client, statement)
		p.recordQuery(QueryRecord{SQL: statement.SQL, CommandTags: tags, Failed: failed})
	}
	if client.transactionSavepoint == "" {
		if err := p.closePortalsLocked(client); err != nil {
			return fmt.Errorf("close autocommit portals: %w", err)
		}
	}
	if sendReady {
		client.backend.Send(&pgproto3.ReadyForQuery{TxStatus: client.transactionStatus()})
		_ = client.backend.Flush()
		p.releaseClientIfIdle(client)
	}
	return nil
}

func (p *Proxy) synchronizeExtendedBackendLocked(client *clientState) error {
	p.frontend.Send(&pgproto3.Sync{})
	if err := p.frontend.Flush(); err != nil {
		return fmt.Errorf("send extended-query Sync: %w", err)
	}
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return fmt.Errorf("receive extended-query Sync response: %w", err)
		}
		switch message := message.(type) {
		case *pgproto3.ParameterStatus:
			p.updateParameter(message)
			if client != nil {
				client.backend.Send(message)
			}
		case *pgproto3.NoticeResponse, *pgproto3.NotificationResponse:
			if client != nil {
				client.backend.Send(message)
			}
		case *pgproto3.ReadyForQuery:
			if message.TxStatus == 'I' {
				return ErrTransactionLost
			}
			if message.TxStatus != 'T' && message.TxStatus != 'E' {
				return fmt.Errorf("postgres backend reported unknown transaction status %q", message.TxStatus)
			}
			return nil
		}
	}
}

func (p *Proxy) closePortalsLocked(client *clientState) error {
	if len(client.portals) == 0 {
		return nil
	}
	names := make([]string, 0, len(client.portals))
	for name := range client.portals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		portal := client.portals[name]
		if portal.statement.synthetic {
			delete(client.portals, name)
			continue
		}
		p.frontend.Send(&pgproto3.Close{ObjectType: 'P', Name: portal.backendName})
	}
	p.frontend.Send(&pgproto3.Sync{})
	if err := p.frontend.Flush(); err != nil {
		return err
	}
	closed := 0
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return err
		}
		switch message := message.(type) {
		case *pgproto3.CloseComplete:
			closed++
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("postgres error closing portal: %s (SQLSTATE %s)",
				message.Message, message.Code)
		case *pgproto3.ReadyForQuery:
			if message.TxStatus != 'T' || closed != countNonSyntheticPortals(client, names) {
				return fmt.Errorf("portal cleanup ended with status %q after %d closes",
					message.TxStatus, closed)
			}
			client.portals = make(map[string]*portal)
			return nil
		}
	}
}

func countNonSyntheticPortals(client *clientState, names []string) int {
	count := 0
	for _, name := range names {
		if portal := client.portals[name]; portal != nil && !portal.statement.synthetic {
			count++
		}
	}
	return count
}

func (p *Proxy) closeClientObjectsLocked(client *clientState) error {
	if err := p.closePortalsLocked(client); err != nil {
		return err
	}
	if len(client.prepared) == 0 {
		return nil
	}
	names := make([]string, 0, len(client.prepared))
	for name := range client.prepared {
		names = append(names, name)
	}
	sort.Strings(names)
	count := 0
	for _, name := range names {
		prepared := client.prepared[name]
		if prepared.synthetic {
			delete(client.prepared, name)
			continue
		}
		p.frontend.Send(&pgproto3.Close{ObjectType: 'S', Name: prepared.backendName})
		count++
	}
	if count == 0 {
		return nil
	}
	p.frontend.Send(&pgproto3.Sync{})
	if err := p.frontend.Flush(); err != nil {
		return err
	}
	closed := 0
	for {
		message, err := p.frontend.Receive()
		if err != nil {
			return err
		}
		switch message := message.(type) {
		case *pgproto3.CloseComplete:
			closed++
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("postgres error closing prepared statement: %s (SQLSTATE %s)",
				message.Message, message.Code)
		case *pgproto3.ReadyForQuery:
			if message.TxStatus != 'T' || closed != count {
				return fmt.Errorf("prepared-statement cleanup ended with status %q after %d/%d closes",
					message.TxStatus, closed, count)
			}
			client.prepared = make(map[string]*preparedStatement)
			return nil
		}
	}
}
