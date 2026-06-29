package config

import (
	"os"
	"testing"
)

func TestParseInterpolation(t *testing.T) {
	os.Setenv("TURNTABLE_PGUSER", "alice")
	os.Setenv("TURNTABLE_PGPASSWORD", "s3cr3t")
	os.Setenv("TURNTABLE_PGHOST", "db.example.com")
	defer func() {
		os.Unsetenv("TURNTABLE_PGUSER")
		os.Unsetenv("TURNTABLE_PGPASSWORD")
		os.Unsetenv("TURNTABLE_PGHOST")
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
func TestValidateSourceSecrets(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		ok   bool
	}{
		{"trello literal token rejected",
			Source{Connector: "trello", Options: map[string]any{"key": "${K}", "token": "literal"}}, false},
		{"trello env refs ok",
			Source{Connector: "trello", Options: map[string]any{"key": "${TRELLO_KEY}", "token": "${TRELLO_TOKEN}"}}, true},
		{"postgres literal dsn rejected",
			Source{Connector: "sql", Driver: "postgres", DSN: "postgres://u:p@h/db"}, false},
		{"postgres env dsn ok",
			Source{Connector: "sql", Driver: "postgres", DSN: "${PG_DSN}"}, true},
		{"sqlite literal dsn ok (just a path)",
			Source{Connector: "sql", Driver: "sqlite", DSN: "./data.db"}, true},
		{"default-with-default form ok",
			Source{Connector: "azuredevops", Options: map[string]any{"pat": "${PAT:-}"}}, true},
		{"non-sensitive literal ok",
			Source{Connector: "http", URL: "https://api.example.com", Options: map[string]any{"method": "GET"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSourceSecrets(c.src)
			if c.ok && err != nil {
				t.Errorf("expected ok, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.env"
	content := "# comment\nFOO=bar\nexport BAZ=\"quoted val\"\nPRESET=fromfile\n\nbadline\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PRESET", "fromenv") // real env should win over .env
	defer func() {
		os.Unsetenv("FOO")
		os.Unsetenv("BAZ")
		os.Unsetenv("PRESET")
	}()
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO"); got != "bar" {
		t.Errorf("FOO = %q, want bar", got)
	}
	if got := os.Getenv("BAZ"); got != "quoted val" {
		t.Errorf("BAZ = %q, want 'quoted val'", got)
	}
	if got := os.Getenv("PRESET"); got != "fromenv" {
		t.Errorf("PRESET = %q, want fromenv (env wins over .env)", got)
	}
	// Missing file is a no-op.
	if err := LoadDotEnv(dir + "/nope.env"); err != nil {
		t.Errorf("missing .env should be a no-op, got %v", err)
	}
}

func TestAppendSourcePreservesAndAdds(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/turntable.yaml"
	orig := "# top comment\nsources:\n  notes: # inline\n    connector: json\n    path: ./n.json\ndefaults:\n  output: table\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	src := Source{Connector: "sql", Driver: "postgres", DSN: "${PG_DSN}", Table: "*"}
	if err := AppendSource(path, "warehouse", src); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	s := string(out)
	for _, want := range []string{"# top comment", "notes:", "warehouse:", "${PG_DSN}", "defaults:"} {
		if !contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// Re-parse and confirm both sources are present and PG_DSN is NOT expanded.
	f, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(f.Sources) != 2 {
		t.Errorf("sources = %d, want 2", len(f.Sources))
	}

	// Replacing an existing source updates it in place (still 2 sources).
	if err := AppendSource(path, "warehouse", Source{Connector: "sql", Driver: "mysql", DSN: "${MY_DSN}"}); err != nil {
		t.Fatal(err)
	}
	out2, _ := os.ReadFile(path)
	if !contains(string(out2), "${MY_DSN}") || contains(string(out2), "${PG_DSN}") {
		t.Errorf("replace failed:\n%s", out2)
	}
}

func TestAppendSourceCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/new.yaml"
	if err := AppendSource(path, "x", Source{Connector: "json", Path: "./x.json"}); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || len(f.Sources) != 1 {
		t.Fatalf("load created file: err=%v sources=%d", err, len(f.Sources))
	}
}

func TestAppendSourceNoPath(t *testing.T) {
	if err := AppendSource("", "x", Source{Connector: "json"}); err == nil {
		t.Error("expected error when no config path is set")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
