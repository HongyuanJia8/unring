package pgproxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type catalogObject struct {
	OID         uint32
	Kind        string
	Name        string
	Fingerprint string
}

type catalogSnapshot map[string]catalogObject

const publicationNamespaceFingerprintToken = "__UNRING_PUBLICATION_NAMESPACE_FINGERPRINT__"

const catalogSnapshotSQLTemplate = `
SELECT c.oid::text,
       CASE c.relkind
         WHEN 'r' THEN 'table' WHEN 'p' THEN 'table'
         WHEN 'f' THEN 'foreign table' WHEN 'v' THEN 'view'
         WHEN 'm' THEN 'materialized view' WHEN 'S' THEN 'sequence'
       END,
       format('%I.%I', n.nspname, c.relname),
       concat_ws(E'\x1f', c.relkind::text, c.relpersistence::text,
         c.relowner::text, c.reltablespace::text,
         c.relrowsecurity::text, c.relforcerowsecurity::text,
         COALESCE(array_to_string(c.relacl, E'\x1e'), ''),
         COALESCE(array_to_string(c.reloptions, E'\x1e'), ''),
         COALESCE(pg_get_partkeydef(c.oid), ''),
         COALESCE((
           SELECT string_agg(concat_ws(E'\x1d', a.attnum::text, a.attname,
                     format_type(a.atttypid, a.atttypmod), a.attnotnull::text,
                     a.attidentity::text, a.attgenerated::text,
                     a.attcollation::text, a.attstorage::text,
                     a.attcompression::text, a.attstattarget::text,
                     COALESCE(pg_get_expr(d.adbin, d.adrelid), '')),
                     E'\x1c' ORDER BY a.attnum)
             FROM pg_attribute a
             LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
            WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
         ), ''),
         COALESCE((
           SELECT string_agg(concat_ws(E'\x1d', x.conname, x.contype::text,
                     pg_get_constraintdef(x.oid, true)), E'\x1c' ORDER BY x.conname)
             FROM pg_constraint x WHERE x.conrelid = c.oid
         ), ''),
         COALESCE((
           SELECT string_agg(pg_get_triggerdef(t.oid, true), E'\x1c' ORDER BY t.tgname)
             FROM pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal
         ), ''),
         COALESCE((
           SELECT string_agg(concat_ws(E'\x1d', p.polname, p.polcmd::text,
                     COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
                     COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')),
                     E'\x1c' ORDER BY p.polname)
             FROM pg_policy p WHERE p.polrelid = c.oid
         ), ''),
         COALESCE((
           SELECT concat_ws(E'\x1d', s.seqtypid::text, s.seqstart::text,
                    s.seqincrement::text, s.seqmax::text, s.seqmin::text,
                    s.seqcache::text, s.seqcycle::text)
             FROM pg_sequence s WHERE s.seqrelid = c.oid
         ), ''),
         COALESCE((
           SELECT concat_ws(E'\x1d', f.ftserver::text,
                    COALESCE(array_to_string(f.ftoptions, E'\x1e'), ''))
             FROM pg_foreign_table f WHERE f.ftrelid = c.oid
         ), ''),
         CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid, true) ELSE '' END)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p', 'f', 'v', 'm', 'S')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT c.oid::text, 'index', format('%I.%I', n.nspname, c.relname),
       concat_ws(E'\x1f', pg_get_indexdef(c.oid), c.relowner::text,
         c.reltablespace::text, COALESCE(array_to_string(c.reloptions, E'\x1e'), ''))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('i', 'I')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT n.oid::text, 'schema', format('%I', n.nspname),
       concat_ws(E'\x1f', pg_get_userbyid(n.nspowner), COALESCE(array_to_string(n.nspacl, E'\x1e'), ''))
  FROM pg_namespace n
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT t.oid::text, 'type', format('%I.%I', n.nspname, t.typname),
       concat_ws(E'\x1f', t.typtype::text, t.typcategory::text, t.typnotnull::text,
         t.typowner::text, COALESCE(array_to_string(t.typacl, E'\x1e'), ''),
         t.typbasetype::text, t.typtypmod::text, COALESCE(t.typdefault, ''),
         COALESCE((SELECT string_agg(e.enumlabel, E'\x1c' ORDER BY e.enumsortorder)
                     FROM pg_enum e WHERE e.enumtypid = t.oid), ''),
         COALESCE((SELECT string_agg(pg_get_constraintdef(x.oid, true), E'\x1c' ORDER BY x.conname)
                     FROM pg_constraint x WHERE x.contypid = t.oid), ''))
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE t.typrelid = 0 AND t.typtype IN ('d', 'e')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT p.oid::text,
       CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
       format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)),
       concat_ws(E'\x1f', p.prokind::text, p.prorettype::text, l.lanname,
         p.proowner::text, COALESCE(array_to_string(p.proacl, E'\x1e'), ''),
         p.prosecdef::text, p.proleakproof::text, p.provolatile::text,
         p.proparallel::text, COALESCE(array_to_string(p.proconfig, E'\x1e'), ''),
         p.prosrc, COALESCE(p.probin, ''))
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  JOIN pg_language l ON l.oid = p.prolang
 WHERE p.prokind IN ('f', 'p')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT e.oid::text, 'extension', format('%I', e.extname),
       concat_ws(E'\x1f', e.extversion, e.extrelocatable::text, e.extnamespace::text)
  FROM pg_extension e
UNION ALL
SELECT r.oid::text, 'rule',
       format('%I.%I.%I', n.nspname, c.relname, r.rulename),
       pg_get_ruledef(r.oid, true)
  FROM pg_rewrite r
  JOIN pg_class c ON c.oid = r.ev_class
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE r.rulename <> '_RETURN'
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT s.oid::text, 'extended statistic', format('%I.%I', n.nspname, s.stxname),
       concat_ws(E'\x1f', pg_get_statisticsobjdef(s.oid),
         (to_jsonb(s) - 'oid' - 'stxname' - 'stxnamespace')::text)
  FROM pg_statistic_ext s
  JOIN pg_namespace n ON n.oid = s.stxnamespace
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT e.oid::text, 'event trigger', format('%I', e.evtname),
       concat_ws(E'\x1f', e.evtevent, e.evtenabled::text,
         e.evtfoid::regprocedure::text,
         COALESCE(array_to_string(e.evttags, E'\x1e'), ''))
  FROM pg_event_trigger e
UNION ALL
SELECT p.oid::text, 'publication', format('%I', p.pubname),
       concat_ws(E'\x1f', (to_jsonb(p) - 'oid' - 'pubname')::text,
         COALESCE((
           SELECT string_agg(
                    (to_jsonb(pr) - 'oid' - 'prpubid')::text,
                    E'\x1c' ORDER BY pr.prrelid)
             FROM pg_publication_rel pr WHERE pr.prpubid = p.oid
         ), ''),
         ` + publicationNamespaceFingerprintToken + `)
  FROM pg_publication p
UNION ALL
SELECT s.oid::text, 'subscription', format('%I.%I', d.datname, s.subname),
       concat_ws(E'\x1f', s.subdbid::text, s.subowner::text,
         s.subenabled::text, s.subbinary::text, s.substream::text,
         COALESCE(s.subslotname, ''), s.subsynccommit,
         COALESCE(array_to_string(s.subpublications, E'\x1e'), ''))
  FROM pg_subscription s
  JOIN pg_database d ON d.oid = s.subdbid
UNION ALL
SELECT d.objoid::text, 'comment', pg_describe_object(d.classoid, d.objoid, d.objsubid),
       d.description
  FROM pg_description d
 WHERE d.description IS NOT NULL
UNION ALL
SELECT d.objoid::text, 'comment', pg_describe_object(d.classoid, d.objoid, 0),
       d.description
  FROM pg_shdescription d
 WHERE d.description IS NOT NULL
UNION ALL
SELECT c.oid::text, 'cast',
       format('%s AS %s', c.castsource::regtype, c.casttarget::regtype),
       concat_ws(E'\x1f', c.castcontext::text, c.castmethod::text,
         c.castfunc::regprocedure::text)
  FROM pg_cast c
  JOIN pg_type s ON s.oid = c.castsource
  JOIN pg_namespace sn ON sn.oid = s.typnamespace
  JOIN pg_type t ON t.oid = c.casttarget
  JOIN pg_namespace tn ON tn.oid = t.typnamespace
 WHERE sn.nspname NOT IN ('pg_catalog', 'information_schema')
    OR tn.nspname NOT IN ('pg_catalog', 'information_schema')
UNION ALL
SELECT o.oid::text, 'operator',
       format('%I.%I(%s,%s)', n.nspname, o.oprname,
         o.oprleft::regtype, o.oprright::regtype),
       concat_ws(E'\x1f', o.oprkind::text, o.oprcanmerge::text,
         o.oprcanhash::text, o.oprcode::regprocedure::text,
         o.oprcom::text, o.oprnegate::text)
  FROM pg_operator o
  JOIN pg_namespace n ON n.oid = o.oprnamespace
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
UNION ALL
SELECT c.oid::text, 'collation', format('%I.%I', n.nspname, c.collname),
       (to_jsonb(c) - 'oid' - 'collname' - 'collnamespace')::text
  FROM pg_collation c
  JOIN pg_namespace n ON n.oid = c.collnamespace
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
ORDER BY 2, 3`

func catalogSnapshotSQL(serverVersion int) string {
	publicationNamespaces := "''"
	// Publications over every table in a schema and the backing catalog were
	// both added in PostgreSQL 15. PostgreSQL resolves relation names while
	// planning, so a to_regclass guard cannot make a static reference safe on
	// 14; omit the impossible object class from that version's query instead.
	if serverVersion >= 150000 {
		publicationNamespaces = `COALESCE((
           SELECT string_agg(pn.pnnspid::regnamespace::text, E'\x1c' ORDER BY pn.pnnspid)
             FROM pg_publication_namespace pn WHERE pn.pnpubid = p.oid
         ), '')`
	}
	return strings.Replace(
		catalogSnapshotSQLTemplate,
		publicationNamespaceFingerprintToken,
		publicationNamespaces,
		1,
	)
}

func (p *Proxy) captureServerVersionLocked() (int, error) {
	rows, err := p.internalRowsLocked("SHOW server_version_num")
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] == nil {
		return 0, fmt.Errorf("PostgreSQL returned malformed server_version_num")
	}
	version, err := strconv.Atoi(string(rows[0][0]))
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("parse server_version_num %q", rows[0][0])
	}
	return version, nil
}

func (p *Proxy) captureCatalogLocked() (catalogSnapshot, error) {
	// Catalog deparser functions qualify names relative to search_path. Pin it
	// inside a private savepoint so a client SET cannot make unchanged objects
	// look altered, then roll back only this setting after the snapshot.
	savepoint := p.savepointPrefix + "_catalog"
	if _, err := p.internalQueryLocked(
		"SAVEPOINT " + savepoint + "; SET LOCAL search_path = pg_catalog",
	); err != nil {
		return nil, fmt.Errorf("pin catalog search path: %w", err)
	}
	rows, err := p.internalRowsLocked(catalogSnapshotSQL(p.serverVersion))
	restoreStatus, restoreErr := p.internalQueryLocked(
		"ROLLBACK TO SAVEPOINT " + savepoint + "; RELEASE SAVEPOINT " + savepoint,
	)
	if restoreErr != nil {
		return nil, fmt.Errorf("restore catalog search path: %w", restoreErr)
	}
	if restoreStatus != 'T' {
		return nil, fmt.Errorf("restore catalog search path ended with transaction status %q", restoreStatus)
	}
	if err != nil {
		return nil, err
	}
	snapshot := make(catalogSnapshot, len(rows))
	for _, row := range rows {
		if len(row) != 4 || row[0] == nil || row[1] == nil || row[2] == nil || row[3] == nil {
			return nil, fmt.Errorf("postgres returned malformed catalog summary row")
		}
		oid, err := strconv.ParseUint(string(row[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse catalog object OID %q: %w", row[0], err)
		}
		object := catalogObject{
			OID: uint32(oid), Kind: string(row[1]), Name: string(row[2]), Fingerprint: string(row[3]),
		}
		snapshot[object.Kind+"\x00"+object.Name] = object
	}
	return snapshot, nil
}

func (p *Proxy) freezeChangeSummaryLocked() {
	p.reconcileRowChangesLocked(true)
	finalCatalog, err := p.captureCatalogLocked()
	if err != nil {
		p.setIncompleteSummary(fmt.Errorf("capture sealed postgres catalog: %w", err))
		return
	}
	if p.rowLedgerErr != nil {
		p.setIncompleteSummary(fmt.Errorf("capture sealed postgres row counts: %w", p.rowLedgerErr))
		return
	}
	if len(p.uncertainEffects) > 0 {
		p.setIncompleteSummary(fmt.Errorf("exact effects could not be determined: %s",
			strings.Join(p.uncertainEffects, "; ")))
		return
	}
	changes := ChangeSummary{
		Rows: p.rowChangesLocked(), Schema: diffCatalog(p.catalogInitial, finalCatalog), Complete: true,
	}
	p.summaryMu.Lock()
	p.changes = changes
	p.sealedSummary = true
	p.summaryMu.Unlock()
}

func (p *Proxy) setIncompleteSummary(err error) {
	p.summaryMu.Lock()
	p.changes = ChangeSummary{Complete: false, Error: err.Error()}
	p.sealedSummary = true
	p.summaryMu.Unlock()
}

type rowStat struct {
	Table    string
	Inserted int64
	Updated  int64
	Deleted  int64
}

type rowStatsSnapshot map[uint32]rowStat
type rowLedgerSnapshot map[uint32]RowChange

const rowStatsSQL = `
SELECT c.oid::text, format('%I.%I', n.nspname, c.relname),
       pg_stat_get_xact_tuples_inserted(c.oid)::text,
       pg_stat_get_xact_tuples_updated(c.oid)::text,
       pg_stat_get_xact_tuples_deleted(c.oid)::text
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p', 'f', 'm')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname !~ '^pg_toast'
 ORDER BY c.oid`

func (p *Proxy) captureRowStatsLocked() (rowStatsSnapshot, error) {
	rows, err := p.internalRowsLocked(rowStatsSQL)
	if err != nil {
		return nil, err
	}
	snapshot := make(rowStatsSnapshot, len(rows))
	for _, row := range rows {
		if len(row) != 5 || row[0] == nil || row[1] == nil {
			return nil, fmt.Errorf("postgres returned malformed row-counter snapshot")
		}
		oid, err := strconv.ParseUint(string(row[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse row-counter relation OID %q: %w", row[0], err)
		}
		counts := [3]int64{}
		for index := range counts {
			if row[index+2] == nil {
				continue
			}
			value, err := strconv.ParseInt(string(row[index+2]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse row-counter value %q: %w", row[index+2], err)
			}
			counts[index] = value
		}
		snapshot[uint32(oid)] = rowStat{
			Table: string(row[1]), Inserted: counts[0], Updated: counts[1], Deleted: counts[2],
		}
	}
	return snapshot, nil
}

// reconcileRowChangesLocked samples PostgreSQL's backend-local relation
// counters after a statement boundary. Those counters include trigger writes,
// but PostgreSQL deliberately retains attempted writes after a subtransaction
// abort. Keeping the deltas in our own ledger lets client savepoints restore
// the staged view without guessing at SQL targets or misattributing triggers.
func (p *Proxy) reconcileRowChangesLocked(keep bool) {
	// Unit protocol harnesses that construct Proxy directly do not have a real
	// catalog or stats baseline. Production proxies always initialize this in
	// StartWithOptions.
	if p.rowStats == nil {
		return
	}
	current, err := p.captureRowStatsLocked()
	if err != nil {
		p.setRowLedgerError(err)
		return
	}
	p.applyRowStatsLocked(current, keep)
	if err := p.captureSequenceUsageLocked(); err != nil {
		p.setRowLedgerError(fmt.Errorf("capture non-transactional sequence use: %w", err))
	}
}

func (p *Proxy) prepareStatementRiskLocked(statement *clientStatement) {
	if statement == nil || statement.SummaryRisk != "" || statement.SummaryTarget == nil {
		return
	}
	rows, err := p.internalRowsLocked(`
SELECT c.relkind::text,
       EXISTS (
         SELECT 1 FROM pg_trigger t
          WHERE t.tgrelid = c.oid AND t.tgdeferrable
       )::text
  FROM pg_class c
 WHERE c.oid = to_regclass(` + quoteSQLLiteral(statement.SummaryTarget.regclassName()) + `)`)
	if err != nil {
		statement.SummaryRisk = "unring could not classify the statement's target for change accounting: " + err.Error()
		return
	}
	if len(rows) == 0 || len(rows[0]) != 2 || rows[0][0] == nil || rows[0][1] == nil {
		return
	}
	switch string(rows[0][0]) {
	case "f":
		statement.SummaryRisk = "the foreign table write is not included in PostgreSQL's local transaction row counters"
		statement.RiskRequiresRows = true
	}
	if string(rows[0][1]) == "true" {
		statement.SummaryRisk = "deferrable constraint triggers may perform additional writes at COMMIT, after the review decision"
		statement.RiskRequiresRows = true
	}
}

func summaryRiskApplies(statement clientStatement, tags []string) bool {
	if statement.SummaryRisk == "" {
		return false
	}
	if !statement.RiskRequiresRows {
		return true
	}
	for _, tag := range tags {
		fields := strings.Fields(tag)
		if len(fields) < 2 {
			continue
		}
		count, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err == nil && count > 0 {
			return true
		}
	}
	return false
}

func (p *Proxy) addUncertainEffectLocked(detail string) {
	if detail == "" {
		return
	}
	for _, existing := range p.uncertainEffects {
		if existing == detail {
			return
		}
	}
	p.uncertainEffects = append(p.uncertainEffects, detail)
}

func (p *Proxy) restoreUncertainEffectsLocked(length int) {
	if length < 0 {
		length = 0
	}
	if length < len(p.uncertainEffects) {
		p.uncertainEffects = p.uncertainEffects[:length]
	}
}

func (p *Proxy) captureSequenceUsageLocked() error {
	rows, err := p.internalRowsLocked(`
SELECT format('%I.%I', n.nspname, c.relname)
  FROM pg_locks l
  JOIN pg_class c ON c.oid = l.relation AND c.relkind = 'S'
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE l.pid = pg_backend_pid() AND l.granted
   AND l.locktype = 'relation' AND l.mode = 'RowExclusiveLock'
 ORDER BY 1`)
	if err != nil {
		return err
	}
	p.summaryMu.Lock()
	defer p.summaryMu.Unlock()
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			return fmt.Errorf("postgres returned malformed sequence-lock information")
		}
		name := string(row[0])
		if _, existedAtStart := p.catalogInitial["sequence\x00"+name]; existedAtStart {
			p.sequenceEffects[name] = struct{}{}
		}
	}
	return nil
}

func (p *Proxy) applyRowStatsLocked(current rowStatsSnapshot, keep bool) {
	defer func() { p.rowStats = current }()
	if !keep || p.rowLedgerErr != nil {
		return
	}
	for oid, after := range current {
		before := p.rowStats[oid]
		if after.Inserted < before.Inserted ||
			after.Updated < before.Updated ||
			after.Deleted < before.Deleted {
			p.setRowLedgerError(fmt.Errorf(
				"PostgreSQL reset row counters for %s; exact staged row counts are unavailable",
				after.Table,
			))
			return
		}
		change := p.rowLedger[oid]
		change.Table = after.Table
		change.Inserted += after.Inserted - before.Inserted
		change.Updated += after.Updated - before.Updated
		change.Deleted += after.Deleted - before.Deleted
		if change.Inserted != 0 || change.Updated != 0 || change.Deleted != 0 {
			p.rowLedger[oid] = change
		}
	}
}

func (p *Proxy) setRowLedgerError(err error) {
	if p.rowLedgerErr == nil {
		p.rowLedgerErr = err
	}
}

func cloneRowLedger(source rowLedgerSnapshot) rowLedgerSnapshot {
	clone := make(rowLedgerSnapshot, len(source))
	for oid, change := range source {
		clone[oid] = change
	}
	return clone
}

func (p *Proxy) restoreRowLedgerLocked(snapshot rowLedgerSnapshot) {
	p.rowLedger = cloneRowLedger(snapshot)
}

func (p *Proxy) rowChangesLocked() []RowChange {
	changes := make([]RowChange, 0, len(p.rowLedger))
	for _, change := range p.rowLedger {
		if change.Inserted != 0 || change.Updated != 0 || change.Deleted != 0 {
			changes = append(changes, change)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Table < changes[j].Table })
	return changes
}

func diffCatalog(initial, final catalogSnapshot) []SchemaChange {
	var changes []SchemaChange
	for key, before := range initial {
		after, exists := final[key]
		switch {
		case !exists:
			changes = append(changes, SchemaChange{Action: "dropped", Kind: before.Kind, Object: before.Name})
		case before.Fingerprint != after.Fingerprint:
			changes = append(changes, SchemaChange{Action: "altered", Kind: before.Kind, Object: before.Name})
		}
	}
	for key, after := range final {
		if _, exists := initial[key]; !exists {
			changes = append(changes, SchemaChange{Action: "created", Kind: after.Kind, Object: after.Name})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		left := changes[i].Kind + "\x00" + changes[i].Object + "\x00" + changes[i].Action
		right := changes[j].Kind + "\x00" + changes[j].Object + "\x00" + changes[j].Action
		return left < right
	})
	return changes
}

func (p *Proxy) recordUnintercepted(statement, detail string) {
	p.summaryMu.Lock()
	p.unintercepted = append(p.unintercepted, UninterceptedItem{
		Statement: statement, Detail: detail,
	})
	p.summaryMu.Unlock()
}
