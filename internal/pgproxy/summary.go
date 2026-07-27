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

const catalogSnapshotSQL = `
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
ORDER BY 2, 3`

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
	rows, err := p.internalRowsLocked(catalogSnapshotSQL)
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
	finalCatalog, err := p.captureCatalogLocked()
	if err != nil {
		p.setIncompleteSummary(fmt.Errorf("capture sealed postgres catalog: %w", err))
		return
	}
	rowChanges, err := p.captureRowChangesLocked(p.catalogInitial, finalCatalog)
	if err != nil {
		p.setIncompleteSummary(fmt.Errorf("capture sealed postgres row counts: %w", err))
		return
	}
	changes := ChangeSummary{
		Rows: rowChanges, Schema: diffCatalog(p.catalogInitial, finalCatalog), Complete: true,
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
	p.unintercepted = append(p.unintercepted, UninterceptedItem{
		Detail: "PostgreSQL change summary is incomplete: " + err.Error(),
	})
	p.summaryMu.Unlock()
}

func (p *Proxy) captureRowChangesLocked(initial, final catalogSnapshot) ([]RowChange, error) {
	tables := make(map[uint32]string)
	for _, snapshot := range []catalogSnapshot{initial, final} {
		for _, object := range snapshot {
			if object.Kind == "table" || object.Kind == "foreign table" ||
				object.Kind == "materialized view" {
				tables[object.OID] = object.Name
			}
		}
	}
	if len(tables) == 0 {
		return nil, nil
	}
	oids := make([]uint32, 0, len(tables))
	for oid := range tables {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
	values := make([]string, 0, len(oids))
	for _, oid := range oids {
		values = append(values, fmt.Sprintf("(%d::oid, %s::text)", oid, quoteLiteral(tables[oid])))
	}
	rows, err := p.internalRowsLocked(
		"SELECT name, pg_stat_get_xact_tuples_inserted(oid)::text, " +
			"pg_stat_get_xact_tuples_updated(oid)::text, " +
			"pg_stat_get_xact_tuples_deleted(oid)::text FROM (VALUES " +
			strings.Join(values, ",") + ") AS known(oid, name) ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	var changes []RowChange
	for _, row := range rows {
		if len(row) != 4 || row[0] == nil {
			return nil, fmt.Errorf("postgres returned malformed row-change summary")
		}
		counts := [3]int64{}
		for index := range counts {
			if row[index+1] == nil {
				continue
			}
			value, err := strconv.ParseInt(string(row[index+1]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse row-change count %q: %w", row[index+1], err)
			}
			counts[index] = value
		}
		if counts[0] != 0 || counts[1] != 0 || counts[2] != 0 {
			changes = append(changes, RowChange{
				Table: string(row[0]), Inserted: counts[0], Updated: counts[1], Deleted: counts[2],
			})
		}
	}
	return changes, nil
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

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (p *Proxy) recordUnintercepted(statement, detail string) {
	p.summaryMu.Lock()
	p.unintercepted = append(p.unintercepted, UninterceptedItem{
		Statement: statement, Detail: detail,
	})
	p.summaryMu.Unlock()
}
