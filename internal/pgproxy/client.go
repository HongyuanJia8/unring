package pgproxy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgproto3"
)

type clientSavepoint struct {
	clientName           string
	backendName          string
	rows                 rowLedgerSnapshot
	uncertain            int
	sequenceSuppressions map[string]sequenceSuppression
}

type preparedStatement struct {
	backendName string
	query       string
	statement   clientStatement
	synthetic   bool
}

type portal struct {
	backendName string
	statement   *preparedStatement
}

type clientState struct {
	id      uint64
	backend *pgproto3.Backend

	locked                    bool
	extended                  bool
	extendedFailed            bool
	cycleSavepoint            string
	pendingEscape             *clientStatement
	rollbackCycle             bool
	cycleUncertain            []string
	cycleRows                 rowLedgerSnapshot
	cycleUncertainBase        int
	cycleSequenceSuppressions map[string]sequenceSuppression
	lastError                 string

	transactionSavepoint            string
	transactionFailed               bool
	transactionRows                 rowLedgerSnapshot
	transactionUncertain            int
	transactionSequenceSuppressions map[string]sequenceSuppression
	savepoints                      []clientSavepoint

	prepared map[string]*preparedStatement
	portals  map[string]*portal
	objectID uint64

	ctx      context.Context
	cancel   context.CancelFunc
	incoming chan clientInput
}

type clientInput struct {
	message pgproto3.FrontendMessage
	err     error
}

func newClientState(id uint64, backend *pgproto3.Backend) *clientState {
	return &clientState{
		id:       id,
		backend:  backend,
		prepared: make(map[string]*preparedStatement),
		portals:  make(map[string]*portal),
		ctx:      context.Background(),
	}
}

func (client *clientState) transactionStatus() byte {
	if client.transactionSavepoint == "" {
		return 'I'
	}
	if client.transactionFailed {
		return 'E'
	}
	return 'T'
}

func (client *clientState) nextObjectName(prefix, kind string) string {
	client.objectID++
	return fmt.Sprintf("%s_c%d_%s%d", prefix, client.id, kind, client.objectID)
}

func (p *Proxy) acquireClient(client *clientState) {
	if client.locked {
		return
	}
	p.queryMu.Lock()
	client.locked = true
}

func (p *Proxy) releaseClientIfIdle(client *clientState) {
	if !client.locked || client.extended {
		return
	}
	client.locked = false
	p.queryMu.Unlock()
}

func (p *Proxy) releaseClient(client *clientState) {
	if !client.locked {
		return
	}
	client.locked = false
	p.queryMu.Unlock()
}

func (p *Proxy) cleanupClient(client *clientState) {
	p.acquireClient(client)

	if client.extended {
		if err := p.synchronizeExtendedBackendLocked(nil); err != nil {
			p.markFatal(fmt.Errorf("synchronize abandoned extended query: %w", err))
			client.extended = false
			client.locked = false
			p.queryMu.Unlock()
			return
		}
		if client.cycleSavepoint != "" {
			if _, err := p.internalQueryLocked("ROLLBACK TO SAVEPOINT " + client.cycleSavepoint +
				"; RELEASE SAVEPOINT " + client.cycleSavepoint); err != nil {
				p.markFatal(fmt.Errorf("recover abandoned extended query: %w", err))
			}
			p.restoreRowLedgerLocked(client.cycleRows)
			p.restoreUncertainEffectsLocked(client.cycleUncertainBase)
			p.reconcileRowChangesLocked(false)
			p.restoreSequenceSuppressionsLocked(client.cycleSequenceSuppressions)
		}
		client.extended = false
		client.cycleSavepoint = ""
		client.cycleUncertain = nil
		client.cycleRows = nil
		client.cycleUncertainBase = 0
		client.cycleSequenceSuppressions = nil
	}

	if client.transactionSavepoint != "" {
		if p.Err() == nil {
			if _, err := p.internalQueryLocked("ROLLBACK TO SAVEPOINT " + client.transactionSavepoint +
				"; RELEASE SAVEPOINT " + client.transactionSavepoint); err != nil {
				p.markFatal(fmt.Errorf("roll back disconnected client transaction: %w", err))
			}
			p.restoreRowLedgerLocked(client.transactionRows)
			p.restoreUncertainEffectsLocked(client.transactionUncertain)
			p.reconcileRowChangesLocked(false)
			p.restoreSequenceSuppressionsLocked(client.transactionSequenceSuppressions)
		}
		p.clearClientTransaction(client)
	}

	if p.Err() == nil {
		if err := p.closeClientObjectsLocked(client); err != nil {
			p.markFatal(fmt.Errorf("clean up disconnected client's protocol objects: %w", err))
		}
	}
	client.locked = false
	p.queryMu.Unlock()
}
