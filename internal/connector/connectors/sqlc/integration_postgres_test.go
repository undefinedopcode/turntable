//go:build integration

package sqlc

import (
	"io"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// TestPostgresIntegration boots a real Postgres (embedded-postgres downloads a
// server binary on first run) and exercises the connector end to end. If the
// server can't start — e.g. no network for the one-time binary download — the
// test skips rather than fails.
func TestPostgresIntegration(t *testing.T) {
	cfg := embeddedpostgres.DefaultConfig().
		Port(5433).
		Logger(io.Discard)
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		t.Skipf("embedded postgres unavailable (first run needs network to fetch the binary): %v", err)
	}
	defer func() { _ = pg.Stop() }()

	dsn := cfg.GetConnectionURL() + "?sslmode=disable"
	seedInventory(t, "postgres", dsn, "DOUBLE PRECISION")
	checkConnector(t, "postgres", dsn)
}
