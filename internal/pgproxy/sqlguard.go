package pgproxy

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// unsafeClientSQL uses PostgreSQL's own parser, embedded by libpg_query, as
// the sole authority for statement boundaries and transaction statement
// types. There is intentionally no handwritten lexer or text fallback.
func unsafeClientSQL(sql string) string {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		// libpg_query embeds a specific PostgreSQL major version, while unring
		// may proxy a newer server that accepts syntax this parser does not.
		// Forwarding a parse error could therefore execute a batch containing
		// transaction control on that newer server. Fail closed instead.
		return fmt.Sprintf(
			"unring could not verify this query with PostgreSQL's parser and refused to forward it: %v",
			err,
		)
	}

	for _, rawStatement := range tree.GetStmts() {
		transaction := rawStatement.GetStmt().GetTransactionStmt()
		if transaction == nil {
			continue
		}
		if reason := unsafeTransactionKind(transaction.GetKind()); reason != "" {
			return reason
		}
	}
	return ""
}

func unsafeTransactionKind(kind pg_query.TransactionStmtKind) string {
	switch kind {
	case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN,
		pg_query.TransactionStmtKind_TRANS_STMT_START,
		pg_query.TransactionStmtKind_TRANS_STMT_COMMIT,
		pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK:
		return "unring owns the shared transaction; client transaction-control commands are not supported"

	case pg_query.TransactionStmtKind_TRANS_STMT_PREPARE,
		pg_query.TransactionStmtKind_TRANS_STMT_COMMIT_PREPARED,
		pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK_PREPARED:
		return "unring cannot allow prepared-transaction control in the shared transaction"

	case pg_query.TransactionStmtKind_TRANS_STMT_SAVEPOINT,
		pg_query.TransactionStmtKind_TRANS_STMT_RELEASE,
		pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
		// Client savepoints do not finish the outer transaction. Unring's own
		// savepoints live in an unpredictable per-session namespace.
		return ""

	case pg_query.TransactionStmtKind_TRANSACTION_STMT_KIND_UNDEFINED:
		return "unring refused an unknown PostgreSQL transaction statement kind"

	default:
		// A future libpg_query may add a transaction kind with semantics this
		// version of unring has not audited.
		return fmt.Sprintf(
			"unring refused unaudited PostgreSQL transaction statement kind %s",
			kind,
		)
	}
}
