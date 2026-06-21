package config

import (
	"os"
	"testing"
)

func TestParseInterpolation(t *testing.T) {
	os.Setenv("OCTOPARSER_PGUSER", "alice")
	os.Setenv("OCTOPARSER_PGPASSWORD", "s3cr3t")
	os.Setenv("OCTOPARSER_PGHOST", "db.example.com")
	defer func() {
		os.Unsetenv("OCTOPARSER_PGUSER")
		os.Unsetenv("OCTOPARSER_PGPASSWORD")
		os.Unsetenv("OCTOPARSER_PGHOST")
	}()

	src := `
sources:
  warehouse:
    connector: sql
    driver: postgres
    dsn: "postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:5432/analytics"
  fallback:
    connector: sql
    driver: postgres
    dsn: "postgres://${MISSING:-admin}@${MISSING:-localhost}/db"
defaults:
  output: table
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := f.Sources["warehouse"].DSN; got != "postgres://alice:s3cr3t@db.example.com:5432/analytics" {
		t.Errorf("warehouse dsn = %q", got)
	}
	if got := f.Sources["fallback"].DSN; got != "postgres://admin@localhost/db" {
		t.Errorf("fallback dsn = %q", got)
	}
}