package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeData creates a temp dir with users.json, orders.csv, and a config, and
// returns the config path.
func writeData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	users := `[{"id":1,"name":"Alice","age":30,"active":true},
{"id":2,"name":"Bob","age":25,"active":false},
{"id":3,"name":"Carol","age":35,"active":true}]`
	orders := "order_id,user_id,amount,region\n101,1,50.50,west\n102,1,120.00,east\n103,2,15.25,west\n104,3,200.00,south\n"
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(users), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orders.csv"), []byte(orders), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := "sources:\n  users:\n    connector: json\n    path: " + filepath.Join(dir, "users.json") + "\n  orders:\n    connector: csv\n    path: " + filepath.Join(dir, "orders.csv") + "\n"
	cfgPath := filepath.Join(dir, "octoparser.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func run(t *testing.T, cfgPath string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	app := NewApp()
	var out, errb bytes.Buffer
	app.Out = &out
	app.Err = &errb
	all := append([]string{"-c", cfgPath}, args...)
	code = app.Run(context.Background(), all)
	return out.String(), errb.String(), code
}

func TestE2E_SelectStar(t *testing.T) {
	cfg := writeData(t)
	out, _, code := run(t, cfg, "SELECT * FROM users ORDER BY name")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !contains(out, "Alice") || !contains(out, "Bob") || !contains(out, "Carol") {
		t.Errorf("output missing names:\n%s", out)
	}
}

func TestE2E_WhereOrderBy(t *testing.T) {
	cfg := writeData(t)
	// age > 28 keeps Alice (30) and Carol (35); filters Bob (25).
	out, _, _ := run(t, cfg, "SELECT name, age FROM users WHERE age > 28 ORDER BY age DESC")
	if !contains(out, "Carol") || !contains(out, "Alice") {
		t.Errorf("output:\n%s", out)
	}
	if contains(out, "Bob") {
		t.Errorf("Bob should be filtered out:\n%s", out)
	}
}

func TestE2E_Aggregate(t *testing.T) {
	cfg := writeData(t)
	out, _, _ := run(t, cfg, "SELECT region, COUNT(*) AS n, SUM(amount) AS total FROM orders GROUP BY region ORDER BY n DESC")
	if !contains(out, "west") || !contains(out, "65.75") {
		t.Errorf("output:\n%s", out)
	}
}

func TestE2E_Join(t *testing.T) {
	cfg := writeData(t)
	out, _, _ := run(t, cfg, "SELECT u.name, COUNT(o.order_id) AS orders FROM orders o JOIN users u ON u.id = o.user_id GROUP BY u.name ORDER BY u.name")
	if !contains(out, "Alice") || !contains(out, " 2") {
		t.Errorf("output:\n%s", out)
	}
}

func TestE2E_JSONOutput(t *testing.T) {
	cfg := writeData(t)
	out, _, _ := run(t, cfg, "-o", "json", "SELECT name FROM users WHERE id = 1")
	if !contains(out, "Alice") {
		t.Errorf("output:\n%s", out)
	}
}

func TestE2E_QualifiedRef(t *testing.T) {
	dir := t.TempDir()
	users := `[{"id":1,"name":"Al"}]`
	p := filepath.Join(dir, "u.json")
	os.WriteFile(p, []byte(users), 0644)
	app := NewApp()
	var out, errb bytes.Buffer
	app.Out = &out
	app.Err = &errb
	code := app.Run(context.Background(), []string{"SELECT name FROM json:" + p})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !contains(out.String(), "Al") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestE2E_Explain(t *testing.T) {
	cfg := writeData(t)
	out, _, _ := run(t, cfg, "--explain", "SELECT name FROM users WHERE age > 20")
	if !contains(out, "Scan") || !contains(out, "Filter") {
		t.Errorf("output:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}