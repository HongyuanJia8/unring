package childenv

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresReplacesOnlyChildEnvironment(t *testing.T) {
	t.Parallel()

	base := []string{
		"KEEP=original",
		"PGHOST=real.example",
		"PGPORT=5432",
		"DATABASE_URL=postgresql://real.example/real",
	}
	original := append([]string(nil), base...)
	config := &pgconn.Config{User: "test user", Database: "test/database"}

	got, err := Postgres(base, "127.0.0.1:45678", config)
	if err != nil {
		t.Fatalf("Postgres() error: %v", err)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatalf("Postgres mutated its input: got %v, want %v", base, original)
	}

	environment := environmentMap(got)
	if environment["KEEP"] != "original" {
		t.Fatalf("unrelated environment was not preserved: %v", environment)
	}
	if environment["PGHOST"] != "127.0.0.1" || environment["PGPORT"] != "45678" {
		t.Fatalf("proxy address not injected: %v", environment)
	}
	if environment["PGSSLMODE"] != "disable" {
		t.Fatalf("local TLS was not disabled: %v", environment)
	}

	parsed, err := url.Parse(environment["DATABASE_URL"])
	if err != nil {
		t.Fatalf("parse injected DATABASE_URL: %v", err)
	}
	if parsed.Host != "127.0.0.1:45678" ||
		parsed.User.Username() != "test user" ||
		parsed.Path != "/test/database" {
		t.Fatalf("unexpected injected DATABASE_URL: %s", parsed)
	}
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		for i := range entry {
			if entry[i] == '=' {
				result[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	return result
}
