package pgproxy

import (
	"strings"
	"unicode"
)

// unsafeClientSQL protects the transaction that belongs to unring rather than
// any individual client. It is deliberately conservative: commands that can
// finish or prepare the outer transaction are rejected instead of guessed at.
func unsafeClientSQL(sql string) string {
	return unsafeClientSQLMode(sql, false)
}

func unsafeClientSQLMode(sql string, plainStringBackslashEscapes bool) string {
	for _, firstWords := range statementFirstWords(sql, plainStringBackslashEscapes) {
		if len(firstWords) == 0 {
			continue
		}
		switch firstWords[0] {
		case "ABORT", "BEGIN", "COMMIT", "END", "ROLLBACK":
			return "unring owns the shared transaction; client transaction-control commands are not supported"
		case "START":
			if len(firstWords) > 1 && firstWords[1] == "TRANSACTION" {
				return "unring owns the shared transaction; START TRANSACTION is not supported"
			}
		case "PREPARE":
			if len(firstWords) > 1 && firstWords[1] == "TRANSACTION" {
				return "unring cannot allow PREPARE TRANSACTION to detach the shared transaction"
			}
		}
	}
	return ""
}

// statementFirstWords returns at most the first two bare words of each
// semicolon-delimited statement while ignoring strings, quoted identifiers,
// dollar-quoted bodies, and comments.
func statementFirstWords(sql string, plainStringBackslashEscapes bool) [][]string {
	var statements [][]string
	var words []string
	for i := 0; i < len(sql); {
		switch {
		case isSpace(sql[i]):
			i++
		case sql[i] == ';':
			if len(words) > 0 {
				statements = append(statements, words)
			}
			words = nil
			i++
		case (sql[i] == 'E' || sql[i] == 'e') &&
			i+1 < len(sql) && sql[i+1] == '\'':
			i = skipSingleQuoted(sql, i+2, true)
		case sql[i] == '\'':
			i = skipSingleQuoted(sql, i+1, plainStringBackslashEscapes)
		case sql[i] == '"':
			i = skipDoubleQuoted(sql, i+1)
		case i+1 < len(sql) && sql[i:i+2] == "--":
			i = skipLineComment(sql, i+2)
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			i = skipBlockComment(sql, i+2)
		case sql[i] == '$':
			if next, ok := skipDollarQuoted(sql, i); ok {
				i = next
			} else {
				i++
			}
		case isWordStart(rune(sql[i])):
			start := i
			i++
			for i < len(sql) && isWordPart(rune(sql[i])) {
				i++
			}
			if len(words) < 2 {
				words = append(words, strings.ToUpper(sql[start:i]))
			}
		default:
			i++
		}
	}
	if len(words) > 0 {
		statements = append(statements, words)
	}
	return statements
}

func skipSingleQuoted(sql string, i int, backslashEscapes bool) int {
	for i < len(sql) {
		if sql[i] == '\'' {
			if i+1 < len(sql) && sql[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1
		}
		if backslashEscapes && sql[i] == '\\' && i+1 < len(sql) {
			i += 2
			continue
		}
		i++
	}
	return i
}

func skipDoubleQuoted(sql string, i int) int {
	for i < len(sql) {
		if sql[i] == '"' {
			if i+1 < len(sql) && sql[i+1] == '"' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipLineComment(sql string, i int) int {
	for i < len(sql) && sql[i] != '\n' {
		i++
	}
	return i
}

func skipBlockComment(sql string, i int) int {
	depth := 1
	for i < len(sql) && depth > 0 {
		switch {
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			depth++
			i += 2
		case i+1 < len(sql) && sql[i:i+2] == "*/":
			depth--
			i += 2
		default:
			i++
		}
	}
	return i
}

func skipDollarQuoted(sql string, i int) (int, bool) {
	end := i + 1
	for end < len(sql) && (sql[end] == '_' || unicode.IsLetter(rune(sql[end])) ||
		unicode.IsDigit(rune(sql[end]))) {
		end++
	}
	if end >= len(sql) || sql[end] != '$' {
		return i, false
	}
	tag := sql[i : end+1]
	closeAt := strings.Index(sql[end+1:], tag)
	if closeAt < 0 {
		return len(sql), true
	}
	return end + 1 + closeAt + len(tag), true
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func isWordStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isWordPart(r rune) bool {
	return isWordStart(r) || unicode.IsDigit(r) || r == '$'
}
