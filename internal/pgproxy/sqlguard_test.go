package pgproxy

import "testing"

func TestUnsafeClientSQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sql    string
		unsafe bool
	}{
		{name: "select", sql: "SELECT 1"},
		{name: "commit", sql: "COMMIT", unsafe: true},
		{name: "rollback after query", sql: "SELECT 1; /* explanation */ ROLLBACK", unsafe: true},
		{
			name:   "bare carriage return ends line comment",
			sql:    "SELECT 1; --\rCOMMIT;",
			unsafe: true,
		},
		{
			name:   "bare carriage return cannot hide commit begin and guessed savepoint",
			sql:    "SELECT 1; --\rCOMMIT; BEGIN; SAVEPOINT unring_internal_1;",
			unsafe: true,
		},
		{name: "start transaction", sql: "-- comment\nSTART TRANSACTION", unsafe: true},
		{name: "prepare transaction", sql: "PREPARE TRANSACTION 'session'", unsafe: true},
		{name: "commit prepared", sql: "COMMIT PREPARED 'session'", unsafe: true},
		{name: "rollback prepared", sql: "ROLLBACK PREPARED 'session'", unsafe: true},
		{name: "client savepoint", sql: "SAVEPOINT client_checkpoint"},
		{name: "client release", sql: "RELEASE SAVEPOINT client_checkpoint"},
		{name: "client rollback to", sql: "ROLLBACK TO SAVEPOINT client_checkpoint"},
		{name: "word in string", sql: "SELECT 'commit; rollback'"},
		{
			name:   "plain string ending in backslash cannot hide commit",
			sql:    `UPDATE config SET dir = 'C:\'; COMMIT;`,
			unsafe: true,
		},
		{
			name:   "escape string escaped quote then outer commit",
			sql:    `SELECT E'\''; COMMIT`,
			unsafe: true,
		},
		{
			name: "escape string can contain transaction words",
			sql:  `SELECT E'quoted \'; COMMIT inside string'`,
		},
		{
			name: "doubled quotes and semicolon stay in plain string",
			sql:  `SELECT 'it''s not a; COMMIT'`,
		},
		{
			name:   "doubled quotes do not hide outer commit",
			sql:    `SELECT 'it''s safe'; COMMIT`,
			unsafe: true,
		},
		{name: "word in identifier", sql: `SELECT "commit" FROM example`},
		{name: "word in dollar body", sql: "DO $body$ BEGIN RAISE NOTICE 'COMMIT'; END $body$"},
		{
			name:   "utf8 dollar tag cannot hide outer commit",
			sql:    "SELECT $\xc3\xa9$'$\xc3\xa9$; COMMIT;",
			unsafe: true,
		},
		{
			name: "utf8 dollar tag contains transaction words",
			sql:  "INSERT INTO log VALUES ($\xe4\xb8\xad$; COMMIT;$\xe4\xb8\xad$)",
		},
		{
			name:   "continued escape string cannot hide commit",
			sql:    "SELECT E'a'\n'\\'e'; COMMIT; --'",
			unsafe: true,
		},
		{
			name: "continued escape string contains transaction words",
			sql:  "SELECT E'a'\n'\\'; COMMIT; --'",
		},
		{
			name:   "quotes in dollar body do not hide outer rollback",
			sql:    `DO $body$ BEGIN RAISE NOTICE 'quoted '' COMMIT'; END $body$; ROLLBACK`,
			unsafe: true,
		},
		{name: "nested comment", sql: "/* outer /* inner */ COMMIT */ SELECT 1"},
		{name: "prepare statement", sql: "PREPARE query AS SELECT 1"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := unsafeClientSQL(test.sql) != ""
			if got != test.unsafe {
				t.Fatalf("unsafeClientSQL(%q) unsafe = %v, want %v", test.sql, got, test.unsafe)
			}
		})
	}
}

func TestUnsafeClientSQLFailsClosedOnParserError(t *testing.T) {
	t.Parallel()

	if reason := unsafeClientSQL("SELECT ("); reason == "" {
		t.Fatal("query rejected by the embedded PostgreSQL parser was forwarded")
	}
}
