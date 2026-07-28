package pgproxy

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type statementKind uint8

const (
	statementRegular statementKind = iota
	statementBegin
	statementCommit
	statementRollback
	statementSavepoint
	statementRollbackTo
	statementRelease
	statementForbidden
)

type clientStatement struct {
	SQL              string
	Kind             statementKind
	Savepoint        string
	Chain            bool
	Options          bool
	Irreversible     string
	Refusal          string
	ReadOnly         bool
	RollbackAfter    bool
	LockTargets      []relationReference
	LockOperation    string
	SummaryRisk      string
	SummaryTarget    *relationReference
	RiskRequiresRows bool
	TruncateTargets  []truncateTarget
	TruncateCascade  bool
	TruncateRestart  bool
	FunctionCalls    []functionReference
}

type functionReference struct {
	Catalog string
	Schema  string
	Name    string
}

type truncateTarget struct {
	Relation           relationReference
	IncludeDescendants bool
}

type relationReference struct {
	Catalog string
	Schema  string
	Name    string
}

// analyzeClientSQL uses PostgreSQL's own parser, embedded by libpg_query, as
// the sole authority for statement boundaries and statement types. There is
// intentionally no handwritten lexer or text fallback: parser failure is a
// refusal to forward, because guessing here could expose the outer transaction.
func analyzeClientSQL(sql string) ([]clientStatement, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf(
			"unring could not verify this query with PostgreSQL's parser and refused to forward it: %w",
			err,
		)
	}

	statements := make([]clientStatement, 0, len(tree.GetStmts()))
	for index, raw := range tree.GetStmts() {
		start := int(raw.GetStmtLocation())
		end := len(sql)
		if raw.GetStmtLen() > 0 {
			end = start + int(raw.GetStmtLen())
		} else if index+1 < len(tree.GetStmts()) {
			end = int(tree.GetStmts()[index+1].GetStmtLocation())
		}
		if start < 0 || end < start || end > len(sql) {
			return nil, errorsInternalStatementBounds(sql, start, end)
		}

		statement := clientStatement{SQL: strings.TrimSpace(sql[start:end])}
		node := raw.GetStmt()
		if transaction := node.GetTransactionStmt(); transaction != nil {
			statement.Savepoint = transaction.GetSavepointName()
			statement.Chain = transaction.GetChain()
			statement.Options = len(transaction.GetOptions()) != 0
			switch transaction.GetKind() {
			case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN,
				pg_query.TransactionStmtKind_TRANS_STMT_START:
				statement.Kind = statementBegin
			case pg_query.TransactionStmtKind_TRANS_STMT_COMMIT:
				statement.Kind = statementCommit
			case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK:
				statement.Kind = statementRollback
			case pg_query.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
				statement.Kind = statementSavepoint
			case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
				statement.Kind = statementRollbackTo
			case pg_query.TransactionStmtKind_TRANS_STMT_RELEASE:
				statement.Kind = statementRelease
			case pg_query.TransactionStmtKind_TRANS_STMT_PREPARE,
				pg_query.TransactionStmtKind_TRANS_STMT_COMMIT_PREPARED,
				pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK_PREPARED:
				statement.Kind = statementForbidden
			default:
				return nil, fmt.Errorf(
					"unring refused unaudited PostgreSQL transaction statement kind %s",
					transaction.GetKind(),
				)
			}
		}

		statement.Irreversible = irreversibleReason(node)
		statement.LockTargets, statement.LockOperation = maintenanceLockTargets(node)
		statement.SummaryRisk, statement.SummaryTarget = summaryRisk(node, statement.SQL)
		statement.TruncateTargets, statement.TruncateCascade, statement.TruncateRestart = truncateDetails(node.GetTruncateStmt())
		statement.FunctionCalls = functionReferences(node)
		statement.ReadOnly = readOnlySelect(node.GetSelectStmt())
		if node.GetDiscardStmt() != nil &&
			node.GetDiscardStmt().GetTarget() == pg_query.DiscardMode_DISCARD_ALL {
			statement.Irreversible = ""
			statement.Refusal = "unring cannot emulate DISCARD ALL without discarding its shared transaction"
		}
		statements = append(statements, statement)
	}
	return statements, nil
}

func maintenanceLockTargets(node *pg_query.Node) ([]relationReference, string) {
	if index := node.GetIndexStmt(); index != nil && index.GetConcurrent() {
		return rangeVarReferences(index.GetRelation()), "CREATE INDEX CONCURRENTLY"
	}
	if vacuum := node.GetVacuumStmt(); vacuum != nil && vacuum.GetIsVacuumcmd() &&
		hasDefinition(vacuum.GetOptions(), "full") {
		var targets []relationReference
		for _, raw := range vacuum.GetRels() {
			targets = append(targets,
				rangeVarReferences(raw.GetVacuumRelation().GetRelation())...)
		}
		return targets, "VACUUM FULL"
	}
	if cluster := node.GetClusterStmt(); cluster != nil {
		return rangeVarReferences(cluster.GetRelation()), "CLUSTER"
	}
	if reindex := node.GetReindexStmt(); reindex != nil && reindexOutsideTransaction(reindex) {
		return rangeVarReferences(reindex.GetRelation()), "REINDEX"
	}
	if drop := node.GetDropStmt(); drop != nil && drop.GetConcurrent() &&
		drop.GetRemoveType() == pg_query.ObjectType_OBJECT_INDEX {
		var targets []relationReference
		for _, object := range drop.GetObjects() {
			if target, ok := relationReferenceFromName(object); ok {
				targets = append(targets, target)
			}
		}
		return targets, "DROP INDEX CONCURRENTLY"
	}
	return nil, ""
}

func summaryRisk(node *pg_query.Node, sql string) (string, *relationReference) {
	switch {
	case node.GetDoStmt() != nil:
		return "DO can execute SQL inside a procedural block that unring cannot inspect for exact row accounting", nil
	case node.GetCallStmt() != nil:
		return "CALL can execute SQL inside a procedure that unring cannot inspect for exact row accounting", nil
	case node.GetRefreshMatViewStmt() != nil:
		return "REFRESH MATERIALIZED VIEW rewrites through a transient relation; exact affected-row counts are unavailable", nil
	case node.GetAlterSubscriptionStmt() != nil:
		return "ALTER SUBSCRIPTION changes version-specific or protected catalog state that unring cannot compare portably", nil
	case containsLargeObjectMutation(sql):
		return "large-object data changed outside the relations covered by PostgreSQL's per-table transaction counters", nil
	}
	var relation *pg_query.RangeVar
	switch {
	case node.GetInsertStmt() != nil:
		relation = node.GetInsertStmt().GetRelation()
	case node.GetUpdateStmt() != nil:
		relation = node.GetUpdateStmt().GetRelation()
	case node.GetDeleteStmt() != nil:
		relation = node.GetDeleteStmt().GetRelation()
	case node.GetMergeStmt() != nil:
		relation = node.GetMergeStmt().GetRelation()
	}
	refs := rangeVarReferences(relation)
	if len(refs) == 1 {
		return "", &refs[0]
	}
	return "", nil
}

func functionReferences(node *pg_query.Node) []functionReference {
	if node == nil {
		return nil
	}
	seen := make(map[functionReference]struct{})
	var references []functionReference
	var visit func(protoreflect.Message)
	visit = func(message protoreflect.Message) {
		if call, ok := message.Interface().(*pg_query.FuncCall); ok {
			parts := make([]string, 0, len(call.GetFuncname()))
			for _, raw := range call.GetFuncname() {
				value := raw.GetString_()
				if value == nil || value.GetSval() == "" {
					parts = nil
					break
				}
				parts = append(parts, value.GetSval())
			}
			if len(parts) > 0 && len(parts) <= 3 {
				reference := functionReference{Name: parts[len(parts)-1]}
				if len(parts) >= 2 {
					reference.Schema = parts[len(parts)-2]
				}
				if len(parts) == 3 {
					reference.Catalog = parts[0]
				}
				if _, exists := seen[reference]; !exists {
					seen[reference] = struct{}{}
					references = append(references, reference)
				}
			}
		}
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			if field.IsList() && field.Kind() == protoreflect.MessageKind {
				list := value.List()
				for index := 0; index < list.Len(); index++ {
					visit(list.Get(index).Message())
				}
			} else if field.Kind() == protoreflect.MessageKind && message.Has(field) {
				visit(value.Message())
			}
			return true
		})
	}
	visit(node.ProtoReflect())
	return references
}

func truncateDetails(statement *pg_query.TruncateStmt) ([]truncateTarget, bool, bool) {
	if statement == nil {
		return nil, false, false
	}
	targets := make([]truncateTarget, 0, len(statement.GetRelations()))
	for _, node := range statement.GetRelations() {
		relation := node.GetRangeVar()
		refs := rangeVarReferences(relation)
		if len(refs) != 1 {
			continue
		}
		targets = append(targets, truncateTarget{
			Relation: refs[0], IncludeDescendants: relation.GetInh(),
		})
	}
	return targets, statement.GetBehavior() == pg_query.DropBehavior_DROP_CASCADE,
		statement.GetRestartSeqs()
}

func containsLargeObjectMutation(sql string) bool {
	json, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return false
	}
	lower := strings.ToLower(json)
	for _, function := range []string{"lo_create", "lo_from_bytea", "lo_import", "lo_put", "lo_unlink"} {
		if strings.Contains(lower, `"sval":"`+function+`"`) ||
			strings.Contains(lower, `"sval": "`+function+`"`) {
			return true
		}
	}
	return false
}

func hasDefinition(options []*pg_query.Node, name string) bool {
	for _, option := range options {
		if definition := option.GetDefElem(); definition != nil &&
			strings.EqualFold(definition.GetDefname(), name) {
			return true
		}
	}
	return false
}

func rangeVarReferences(relation *pg_query.RangeVar) []relationReference {
	if relation == nil || relation.GetRelname() == "" {
		return nil
	}
	return []relationReference{{
		Catalog: relation.GetCatalogname(),
		Schema:  relation.GetSchemaname(),
		Name:    relation.GetRelname(),
	}}
}

func relationReferenceFromName(node *pg_query.Node) (relationReference, bool) {
	list := node.GetList()
	if list == nil || len(list.GetItems()) == 0 || len(list.GetItems()) > 3 {
		return relationReference{}, false
	}
	parts := make([]string, 0, len(list.GetItems()))
	for _, item := range list.GetItems() {
		value := item.GetString_()
		if value == nil || value.GetSval() == "" {
			return relationReference{}, false
		}
		parts = append(parts, value.GetSval())
	}
	target := relationReference{Name: parts[len(parts)-1]}
	if len(parts) >= 2 {
		target.Schema = parts[len(parts)-2]
	}
	if len(parts) == 3 {
		target.Catalog = parts[0]
	}
	return target, true
}

func readOnlySelect(statement *pg_query.SelectStmt) bool {
	if statement == nil || statement.GetIntoClause() != nil {
		return false
	}
	with := statement.GetWithClause()
	if with == nil {
		return true
	}
	for _, raw := range with.GetCtes() {
		query := raw.GetCommonTableExpr().GetCtequery()
		if query == nil || query.GetSelectStmt() == nil ||
			!readOnlySelect(query.GetSelectStmt()) {
			return false
		}
	}
	return true
}

func errorsInternalStatementBounds(sql string, start, end int) error {
	return fmt.Errorf(
		"unring's PostgreSQL parser returned invalid statement bounds %d:%d for %d-byte query",
		start, end, len(sql),
	)
}

func irreversibleReason(node *pg_query.Node) string {
	switch {
	case serverSideCopyDestination(node.GetCopyStmt()):
		return "server-side COPY writes a file or runs a program outside the shared transaction"
	case node.GetCreatedbStmt() != nil:
		return "CREATE DATABASE cannot run inside a transaction block"
	case node.GetDropdbStmt() != nil:
		return "DROP DATABASE cannot run inside a transaction block"
	case node.GetCreateTableSpaceStmt() != nil:
		return "CREATE TABLESPACE cannot run inside a transaction block"
	case node.GetDropTableSpaceStmt() != nil:
		return "DROP TABLESPACE cannot run inside a transaction block"
	case node.GetAlterSystemStmt() != nil:
		return "ALTER SYSTEM cannot run inside a transaction block"
	case node.GetCheckPointStmt() != nil:
		return "CHECKPOINT changes cluster state outside the shared transaction"
	case clusterAll(node.GetClusterStmt()):
		return "CLUSTER without a table cannot run inside a transaction block"
	case node.GetVacuumStmt() != nil && node.GetVacuumStmt().GetIsVacuumcmd():
		return "VACUUM cannot run inside a transaction block"
	case node.GetIndexStmt() != nil && node.GetIndexStmt().GetConcurrent():
		return "CREATE INDEX CONCURRENTLY cannot run inside a transaction block"
	case node.GetDropStmt() != nil && node.GetDropStmt().GetConcurrent():
		return "DROP INDEX CONCURRENTLY cannot run inside a transaction block"
	case reindexOutsideTransaction(node.GetReindexStmt()):
		return "this form of REINDEX cannot run inside a transaction block"
	case alterDatabaseTablespace(node.GetAlterDatabaseStmt()):
		return "ALTER DATABASE SET TABLESPACE cannot run inside a transaction block"
	default:
		return ""
	}
}

func serverSideCopyDestination(statement *pg_query.CopyStmt) bool {
	return statement != nil && !statement.GetIsFrom() && statement.GetFilename() != ""
}

func clusterAll(statement *pg_query.ClusterStmt) bool {
	return statement != nil && statement.GetRelation() == nil
}

func reindexOutsideTransaction(statement *pg_query.ReindexStmt) bool {
	if statement == nil {
		return false
	}
	switch statement.GetKind() {
	case pg_query.ReindexObjectType_REINDEX_OBJECT_SCHEMA,
		pg_query.ReindexObjectType_REINDEX_OBJECT_SYSTEM,
		pg_query.ReindexObjectType_REINDEX_OBJECT_DATABASE:
		return true
	}
	for _, parameter := range statement.GetParams() {
		if definition := parameter.GetDefElem(); definition != nil &&
			strings.EqualFold(definition.GetDefname(), "concurrently") {
			return true
		}
	}
	return false
}

func alterDatabaseTablespace(statement *pg_query.AlterDatabaseStmt) bool {
	if statement == nil {
		return false
	}
	for _, option := range statement.GetOptions() {
		if definition := option.GetDefElem(); definition != nil &&
			strings.EqualFold(definition.GetDefname(), "tablespace") {
			return true
		}
	}
	return false
}

// unsafeClientSQL is retained for the original guard regression suite. A
// non-empty result now means "must not be forwarded unchanged": ordinary
// transaction control is translated, while prepared-transaction control is
// rejected. Keeping detection separate protects the parser boundary without
// conflating it with the new execution policy.
func unsafeClientSQL(sql string) string {
	statements, err := analyzeClientSQL(sql)
	if err != nil {
		return err.Error()
	}
	for _, statement := range statements {
		switch statement.Kind {
		case statementBegin, statementCommit, statementRollback:
			return "unring detected transaction control that must be translated"
		case statementForbidden:
			return "unring cannot allow prepared-transaction control in the shared transaction"
		}
	}
	return ""
}
