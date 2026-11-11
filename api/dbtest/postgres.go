package dbtest

import (
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

func openPostgresDatabase(settings config) (database, error) {
	if settings.databaseUrl == "" {
		return database{}, errors.New("set DATABASE_URL or --database to a Postgres connection URL")
	}
	// Parse once here: sql.Open defers parsing and may later expose the URL in errors.
	settingsPostgres, err := pq.NewConfig(settings.databaseUrl)
	if err != nil {
		return database{}, errors.New("could not configure Postgres connection; check DATABASE_URL")
	}
	// pq needs an explicit connect timeout to bound SSL and startup handshakes too.
	if settingsPostgres.ConnectTimeout <= 0 || settingsPostgres.ConnectTimeout > settings.timeout {
		settingsPostgres.ConnectTimeout = settings.timeout
	}
	connector, err := pq.NewConnectorConfig(settingsPostgres)
	if err != nil {
		return database{}, errors.New("could not configure Postgres connection; check DATABASE_URL")
	}
	connection := sql.OpenDB(connector)
	connection.SetMaxOpenConns(1)
	return database{
		connection:   connection,
		driver:       "postgres",
		binaryType:   "BYTEA",
		tablesQuery:  "SELECT COUNT(*) FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema = current_schema() AND table_name IN ('borq_probe_state', 'borq_probe_records')",
		versionQuery: "SHOW server_version",
		writeLock:    "SELECT pg_advisory_xact_lock(742031805)",
		// Keep the state, rows and fingerprint on one snapshot during concurrent operations.
		isolation: sql.LevelRepeatableRead,
	}, nil
}
