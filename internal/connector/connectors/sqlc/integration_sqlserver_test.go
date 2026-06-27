//go:build integration

package sqlc

import (
	"database/sql"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestSQLServerIntegration boots a real SQL Server in a Docker container and
// exercises the connector end to end (schema discovery, table enumeration,
// [bracket] quoting, @pN params, and TOP-based limit pushdown).
//
// Unlike the Postgres/MySQL cases there is no embeddable pure-Go SQL Server, so
// this needs Docker. It skips (rather than fails) when docker is absent, the
// container can't start/pull, or the server never becomes ready — SQL Server is
// slow to boot and wants ~2 GB RAM. Env overrides:
//   - TURNTABLE_MSSQL_DSN:   use an already-running server, skip the container
//   - TURNTABLE_MSSQL_IMAGE: override the image (default: mssql/server:2022-latest)
//
// First run pulls a ~1.5 GB image; allow a generous `go test -timeout`.
func TestSQLServerIntegration(t *testing.T) {
	// SQL Server SA password: meets the complexity policy (upper/lower/digit/
	// symbol). Passed via the ADO-style DSN below to avoid URL-escaping.
	const password = "StrongP@ss123"
	const port = "14333"

	dsn := os.Getenv("TURNTABLE_MSSQL_DSN")
	if dsn == "" {
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skip("docker not found; skipping SQL Server integration test")
		}
		image := envOr("TURNTABLE_MSSQL_IMAGE", "mcr.microsoft.com/mssql/server:2022-latest")
		const name = "turntable-mssql-it"
		// Clear any leftover container from a previous interrupted run.
		_ = exec.Command("docker", "rm", "-f", name).Run()
		run := exec.Command("docker", "run", "-d", "--rm",
			"--name", name,
			"-e", "ACCEPT_EULA=Y",
			"-e", "MSSQL_SA_PASSWORD="+password,
			"-p", port+":1433",
			image,
		)
		if out, err := run.CombinedOutput(); err != nil {
			t.Skipf("could not start SQL Server container (%v): %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
		dsn = "server=localhost," + port + ";user id=sa;password=" + password +
			";database=master;encrypt=disable"
	}

	// SQL Server takes ~20-60s to accept logins on a cold start.
	if !waitReady("sqlserver", dsn, 120*time.Second) {
		t.Skip("SQL Server never became ready (insufficient memory / slow pull?); skipping")
	}

	seedInventory(t, "sqlserver", dsn, "FLOAT")
	checkConnector(t, "sqlserver", dsn)
}

// waitReady polls until the server accepts a connection or the timeout elapses.
func waitReady(driver, dsn string, timeout time.Duration) bool {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := db.Ping(); err == nil {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
