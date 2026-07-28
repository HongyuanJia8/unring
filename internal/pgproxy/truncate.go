package pgproxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// truncateEffect is captured while every relation PostgreSQL can truncate is
// protected by ACCESS EXCLUSIVE. Counts are for physical ordinary tables;
// partitioned parents have no rows of their own and are intentionally omitted
// from Rows. Relations retains the full reviewed closure solely to identify
// owned sequences reset by RESTART IDENTITY.
type truncateEffect struct {
	Rows      map[uint32]RowChange
	Relations []uint32
}

type truncateRelation struct {
	OID                   uint32
	Kind                  string
	Name                  string
	AccessExclusiveLocked bool
	RowSecurityActive     bool
	HasTruncateTrigger    bool
}

func (p *Proxy) prepareTruncateEffectLocked(statement clientStatement) (*truncateEffect, error) {
	if len(statement.TruncateTargets) == 0 {
		return nil, nil
	}

	checkpoint := p.savepointPrefix + "_truncate_count"
	if _, err := p.internalQueryLocked("SAVEPOINT " + checkpoint); err != nil {
		return nil, fmt.Errorf("start exact TRUNCATE count: %w", err)
	}
	effect, err := p.captureTruncateEffectLocked(statement)
	finish := "RELEASE SAVEPOINT " + checkpoint
	if err != nil {
		finish = "ROLLBACK TO SAVEPOINT " + checkpoint + "; RELEASE SAVEPOINT " + checkpoint
	}
	if _, finishErr := p.internalQueryLocked(finish); finishErr != nil {
		return nil, fmt.Errorf("finish exact TRUNCATE count: %w", finishErr)
	}
	if err != nil {
		return nil, err
	}
	return effect, nil
}

func (p *Proxy) captureTruncateEffectLocked(statement clientStatement) (*truncateEffect, error) {
	lockTargets := make([]string, 0, len(statement.TruncateTargets))
	for _, target := range statement.TruncateTargets {
		name := target.Relation.regclassName()
		if !target.IncludeDescendants {
			name = "ONLY " + name
		}
		lockTargets = append(lockTargets, name)
	}
	if _, err := p.internalQueryLocked("LOCK TABLE " + strings.Join(lockTargets, ", ") +
		" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return nil, fmt.Errorf("could not lock the TRUNCATE targets before counting: %w", err)
	}

	initial, err := p.initialTruncateRelationsLocked(statement.TruncateTargets)
	if err != nil {
		return nil, err
	}
	if len(initial) == 0 {
		return nil, fmt.Errorf("PostgreSQL resolved no physical relations for the TRUNCATE targets")
	}
	initialOIDs := make([]uint32, 0, len(initial))
	for _, relation := range initial {
		initialOIDs = append(initialOIDs, relation.OID)
	}

	var relations []truncateRelation
	for attempt := 0; attempt < 64; attempt++ {
		relations, err = p.truncateClosureLocked(initialOIDs, statement.TruncateCascade)
		if err != nil {
			return nil, err
		}
		var unlocked []string
		for _, relation := range relations {
			if !relation.AccessExclusiveLocked {
				unlocked = append(unlocked, "ONLY "+relation.Name)
			}
		}
		if len(unlocked) == 0 {
			break
		}
		if _, err := p.internalQueryLocked("LOCK TABLE " + strings.Join(unlocked, ", ") +
			" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return nil, fmt.Errorf("could not lock a cascaded TRUNCATE relation before counting: %w", err)
		}
		if attempt == 63 {
			return nil, fmt.Errorf("the recursive TRUNCATE relation set did not become stable")
		}
	}

	for _, relation := range relations {
		switch relation.Kind {
		case "r", "p":
		case "f":
			return nil, fmt.Errorf("foreign table %s may change outside PostgreSQL's local locks", relation.Name)
		default:
			return nil, fmt.Errorf("relation %s has unsupported PostgreSQL kind %q", relation.Name, relation.Kind)
		}
		if relation.HasTruncateTrigger {
			return nil, fmt.Errorf("table %s has an enabled ON TRUNCATE trigger whose effects cannot be counted before it runs", relation.Name)
		}
		if relation.Kind == "r" && relation.RowSecurityActive {
			return nil, fmt.Errorf("row-level security prevents an exact count of table %s for the current role", relation.Name)
		}
	}

	queries := make([]string, 0, len(relations))
	for _, relation := range relations {
		if relation.Kind != "r" {
			continue
		}
		queries = append(queries, fmt.Sprintf(
			"SELECT %d::oid::text, count(*)::text FROM ONLY %s",
			relation.OID, relation.Name,
		))
	}
	relationOIDs := make([]uint32, 0, len(relations))
	for _, relation := range relations {
		relationOIDs = append(relationOIDs, relation.OID)
	}
	if len(queries) == 0 {
		return &truncateEffect{Rows: make(map[uint32]RowChange), Relations: relationOIDs}, nil
	}
	rows, err := p.internalRowsLocked(strings.Join(queries, " UNION ALL ") + " ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("exact COUNT(*) for TRUNCATE failed: %w", err)
	}
	byOID := make(map[uint32]truncateRelation, len(relations))
	for _, relation := range relations {
		byOID[relation.OID] = relation
	}
	effect := &truncateEffect{
		Rows:      make(map[uint32]RowChange, len(rows)),
		Relations: relationOIDs,
	}
	for _, row := range rows {
		if len(row) != 2 || row[0] == nil || row[1] == nil {
			return nil, fmt.Errorf("PostgreSQL returned a malformed exact TRUNCATE count")
		}
		oidValue, oidErr := strconv.ParseUint(string(row[0]), 10, 32)
		count, countErr := strconv.ParseInt(string(row[1]), 10, 64)
		relation, exists := byOID[uint32(oidValue)]
		if oidErr != nil || countErr != nil || count < 0 || !exists {
			return nil, fmt.Errorf("PostgreSQL returned an invalid exact TRUNCATE count")
		}
		effect.Rows[relation.OID] = RowChange{Table: relation.Name, Deleted: count}
	}
	return effect, nil
}

func (p *Proxy) initialTruncateRelationsLocked(targets []truncateTarget) ([]truncateRelation, error) {
	allSeeds := make([]string, 0, len(targets))
	recursiveSeeds := make([]string, 0, len(targets))
	for _, target := range targets {
		value := "to_regclass(" + quoteSQLLiteral(target.Relation.regclassName()) + ")::oid"
		allSeeds = append(allSeeds, "("+value+")")
		if target.IncludeDescendants {
			recursiveSeeds = append(recursiveSeeds, "("+value+")")
		}
	}
	recursiveSQL := "SELECT NULL::oid WHERE false"
	if len(recursiveSeeds) > 0 {
		recursiveSQL = "VALUES " + strings.Join(recursiveSeeds, ",")
	}
	query := `WITH RECURSIVE
seeds(oid) AS (VALUES ` + strings.Join(allSeeds, ",") + `),
descendants(oid) AS (
  ` + recursiveSQL + `
  UNION
  SELECT i.inhrelid FROM descendants d JOIN pg_inherits i ON i.inhparent = d.oid
),
affected(oid) AS (SELECT oid FROM seeds UNION SELECT oid FROM descendants)
` + truncateRelationSelect + `
 WHERE c.oid IN (SELECT oid FROM affected)
   AND (c.relpersistence <> 't' OR c.relnamespace = pg_my_temp_schema())
 ORDER BY c.oid`
	return p.readTruncateRelationsLocked(query)
}

func (p *Proxy) truncateClosureLocked(initial []uint32, cascade bool) ([]truncateRelation, error) {
	values := make([]string, 0, len(initial))
	for _, oid := range initial {
		values = append(values, fmt.Sprintf("(%d::oid, true)", oid))
	}
	work := "work(oid, truncates) AS (VALUES " + strings.Join(values, ",") + ")"
	if cascade {
		work = `constraint_ancestors(child_oid, oid, conparentid, confrelid) AS (
  SELECT oid, oid, conparentid, confrelid FROM pg_constraint WHERE contype = 'f'
  UNION ALL
  SELECT a.child_oid, p.oid, p.conparentid, p.confrelid
    FROM constraint_ancestors a JOIN pg_constraint p ON p.oid = a.conparentid
),
constraint_roots(child_oid, confrelid) AS (
  SELECT child_oid, confrelid FROM constraint_ancestors WHERE conparentid = 0
),
work(oid, truncates) AS (
  VALUES ` + strings.Join(values, ",") + `
  UNION
  SELECT addition.oid, addition.truncates
    FROM work w
    JOIN pg_constraint c ON c.contype = 'f' AND c.confrelid = w.oid
    LEFT JOIN constraint_roots root ON root.child_oid = c.oid
    CROSS JOIN LATERAL
      (VALUES (c.conrelid, true), (root.confrelid, false)) addition(oid, truncates)
   WHERE addition.oid IS NOT NULL AND addition.oid <> 0
)`
	}
	query := "WITH RECURSIVE " + work + "\n" + truncateRelationSelect + `
 WHERE c.oid IN (SELECT oid FROM work WHERE truncates)
   AND (c.relpersistence <> 't' OR c.relnamespace = pg_my_temp_schema())
 ORDER BY c.oid`
	return p.readTruncateRelationsLocked(query)
}

const truncateRelationSelect = `SELECT c.oid::text, c.relkind::text,
       format('%I.%I', n.nspname, c.relname),
       EXISTS (
         SELECT 1 FROM pg_locks l
          WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation'
            AND l.relation = c.oid AND l.mode = 'AccessExclusiveLock' AND l.granted
       )::text,
       row_security_active(c.oid)::text,
       EXISTS (
         SELECT 1 FROM pg_trigger t
          WHERE t.tgrelid = c.oid AND (t.tgtype::integer & 32) <> 0
            AND t.tgenabled <> 'D'
       )::text
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace`

func (p *Proxy) readTruncateRelationsLocked(query string) ([]truncateRelation, error) {
	rows, err := p.internalRowsLocked(query)
	if err != nil {
		return nil, fmt.Errorf("determine exact TRUNCATE relation set: %w", err)
	}
	relations := make([]truncateRelation, 0, len(rows))
	for _, row := range rows {
		if len(row) != 6 || row[0] == nil || row[1] == nil || row[2] == nil ||
			row[3] == nil || row[4] == nil || row[5] == nil {
			return nil, fmt.Errorf("PostgreSQL returned malformed TRUNCATE relation metadata")
		}
		oid, err := strconv.ParseUint(string(row[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse TRUNCATE relation OID %q: %w", row[0], err)
		}
		relations = append(relations, truncateRelation{
			OID: uint32(oid), Kind: string(row[1]), Name: string(row[2]),
			AccessExclusiveLocked: string(row[3]) == "true",
			RowSecurityActive:     string(row[4]) == "true",
			HasTruncateTrigger:    string(row[5]) == "true",
		})
	}
	sort.Slice(relations, func(i, j int) bool { return relations[i].OID < relations[j].OID })
	return relations, nil
}

func (p *Proxy) reconcileTruncateLocked(effect *truncateEffect, keep bool, restartIdentity bool) {
	if !keep || effect == nil || p.rowStats == nil {
		p.reconcileRowChangesLocked(false)
		return
	}
	current, err := p.captureRowStatsLocked()
	if err != nil {
		p.setRowLedgerError(err)
		return
	}
	// TRUNCATE resets the affected relations' backend-local counters. Apply
	// ordinary deltas everywhere else, then add the exact pre-truncate counts.
	filteredBefore := make(rowStatsSnapshot, len(p.rowStats))
	filteredCurrent := make(rowStatsSnapshot, len(current))
	for oid, before := range p.rowStats {
		if _, truncated := effect.Rows[oid]; !truncated {
			filteredBefore[oid] = before
		}
	}
	for oid, after := range current {
		if _, truncated := effect.Rows[oid]; !truncated {
			filteredCurrent[oid] = after
		}
	}
	p.rowStats = filteredBefore
	p.applyRowStatsLocked(filteredCurrent, true)
	if p.rowLedgerErr != nil {
		p.rowStats = current
		return
	}
	p.rowStats = current
	for oid, exact := range effect.Rows {
		change := p.rowLedger[oid]
		change.Table = exact.Table
		change.Deleted += exact.Deleted
		if change.Inserted != 0 || change.Updated != 0 || change.Deleted != 0 {
			p.rowLedger[oid] = change
		}
	}
	if restartIdentity {
		if err := p.suppressRestartedSequencesLocked(effect.Relations); err != nil {
			p.setRowLedgerError(fmt.Errorf("identify transactionally reset sequences: %w", err))
			return
		}
	}
	if err := p.captureSequenceUsageLocked(); err != nil {
		p.setRowLedgerError(fmt.Errorf("capture non-transactional sequence use: %w", err))
	}
}
