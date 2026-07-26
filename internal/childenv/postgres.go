// Package childenv builds environment overrides for wrapped child processes.
package childenv

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres returns a copy of base with PostgreSQL clients pointed at the local
// proxy. It never calls os.Setenv.
func Postgres(base []string, proxyAddress string, backend *pgconn.Config) ([]string, error) {
	host, port, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("split postgres proxy address: %w", err)
	}

	databaseURL := (&url.URL{
		Scheme:   "postgresql",
		User:     url.User(backend.User),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + backend.Database,
		RawPath:  "/" + url.PathEscape(backend.Database),
		RawQuery: "sslmode=disable",
	}).String()

	overrides := map[string]string{
		"DATABASE_URL": databaseURL,
		"PGDATABASE":   backend.Database,
		"PGHOST":       host,
		"PGPORT":       port,
		"PGSSLMODE":    "disable",
		"PGUSER":       backend.User,
	}

	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for _, key := range []string{
		"DATABASE_URL", "PGDATABASE", "PGHOST", "PGPORT", "PGSSLMODE", "PGUSER",
	} {
		result = append(result, key+"="+overrides[key])
	}
	return result, nil
}
