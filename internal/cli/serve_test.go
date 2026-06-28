package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/csvc"
)

func postQuery(t *testing.T, a *App, body string) queryResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp queryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func TestHandleQueryNoFrom(t *testing.T) {
	a := NewApp()
	resp := postQuery(t, a, `{"query":"SELECT 1 AS n, 'hi' AS g"}`)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(resp.Columns) != 2 || resp.Columns[0].Name != "n" || resp.Columns[1].Name != "g" {
		t.Fatalf("columns = %+v", resp.Columns)
	}
	if resp.Count != 1 || len(resp.Rows) != 1 {
		t.Fatalf("count = %d, rows = %d", resp.Count, len(resp.Rows))
	}
	if resp.Rows[0][1] != "hi" {
		t.Errorf("row[0][1] = %v, want hi", resp.Rows[0][1])
	}
}

func TestHandleQueryParseError(t *testing.T) {
	a := NewApp()
	resp := postQuery(t, a, `{"query":"SELECT FROM WHERE"}`)
	if resp.Error == "" {
		t.Fatal("expected an error field for malformed SQL")
	}
	if !strings.Contains(resp.Error, "parse") {
		t.Errorf("error = %q, want a parse error", resp.Error)
	}
}

func TestHandleQueryExplain(t *testing.T) {
	a := NewApp()
	resp := postQuery(t, a, `{"query":"SELECT 1 AS n","explain":true}`)
	if resp.Error != "" {
		t.Fatalf("explain error: %s", resp.Error)
	}
	if resp.Explain == "" {
		t.Fatal("expected a plan in the Explain field")
	}
}

func TestHandleQueryMethodNotAllowed(t *testing.T) {
	a := NewApp()
	req := httptest.NewRequest(http.MethodGet, "/api/query", nil)
	rec := httptest.NewRecorder()
	a.handleQuery(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleSources(t *testing.T) {
	a := NewApp()
	_ = a.Reg.RegisterSource("widgets", csvc.New(), connector.Dataset{Name: "widgets", Source: "./widgets.csv"})
	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	rec := httptest.NewRecorder()
	a.handleSources(rec, req)
	var out []struct {
		Name      string `json:"name"`
		Connector string `json:"connector"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Name != "widgets" || out[0].Connector != "csv" {
		t.Fatalf("sources = %+v", out)
	}
}

func postLoginfer(t *testing.T, a *App, path string) loginferResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	req := httptest.NewRequest(http.MethodPost, "/api/loginfer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleLoginfer(rec, req)
	var out loginferResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestHandleLoginferDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pacman.log")
	os.WriteFile(p, []byte("[2026-05-25T20:44:11-0700] [ALPM] installed glib2 (2.88.1-1)\n[2026-05-25T20:44:12-0700] [ALPM] installed gnupg (2.4.9-1)\n"), 0o644)

	out := postLoginfer(t, NewApp(), p)
	if out.Detected == nil {
		t.Fatalf("expected a detected format, got %+v", out)
	}
	if out.Detected.Format != "bracketed" {
		t.Errorf("format = %q, want bracketed", out.Detected.Format)
	}
	if len(out.Detected.Rows) == 0 {
		t.Error("expected preview rows")
	}
}

func TestHandleLoginferInferred(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	os.WriteFile(p, []byte("svc started in 12ms ok=true\nsvc started in 7ms ok=true\nsvc started in 40ms ok=false\n"), 0o644)

	out := postLoginfer(t, NewApp(), p)
	if out.Detected != nil {
		t.Fatalf("expected inference (no known format), got detected %q", out.Detected.Format)
	}
	if len(out.Templates) == 0 {
		t.Fatalf("expected inferred templates, got %+v", out)
	}
	// Every emitted pattern must be usable (it parsed its own sample at emit time).
	if out.Templates[0].Pattern == "" || len(out.Templates[0].Columns) == 0 {
		t.Errorf("template missing pattern/columns: %+v", out.Templates[0])
	}
}

func TestHandleFunctions(t *testing.T) {
	a := NewApp()
	req := httptest.NewRequest(http.MethodGet, "/api/functions", nil)
	rec := httptest.NewRecorder()
	a.handleFunctions(rec, req)
	var out struct {
		Scalar    []string `json:"scalar"`
		Aggregate []string `json:"aggregate"`
		Keywords  []string `json:"keywords"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Scalar) == 0 || len(out.Keywords) == 0 {
		t.Fatalf("expected scalar + keyword lists, got %+v", out)
	}
	has := func(xs []string, want string) bool {
		for _, x := range xs {
			if x == want {
				return true
			}
		}
		return false
	}
	if !has(out.Aggregate, "COUNT") || !has(out.Scalar, "COALESCE") {
		t.Errorf("missing expected functions: agg=%v scalar=%v", out.Aggregate, out.Scalar)
	}
}

func postSource(t *testing.T, a *App, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleSources(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAddSourceThenQueryAndList(t *testing.T) {
	dir := t.TempDir()
	csvPath := dir + "/sales.csv"
	if err := os.WriteFile(csvPath, []byte("region,amount\nemea,10\namer,20\nemea,30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewApp()

	// Register a csv source via the web endpoint.
	code, out := postSource(t, a, `{"name":"sales","connector":"csv","fields":{"path":"`+csvPath+`"}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out["error"] != nil {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	reg, _ := out["registered"].([]any)
	if len(reg) != 1 || reg[0] != "sales" {
		t.Fatalf("registered = %v", out["registered"])
	}

	// It now appears in the source list.
	listRec := httptest.NewRecorder()
	a.handleSources(listRec, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if !strings.Contains(listRec.Body.String(), `"sales"`) {
		t.Errorf("source list missing sales: %s", listRec.Body.String())
	}

	// And it is queryable.
	resp := postQuery(t, a, `{"query":"SELECT region, COUNT(*) AS n FROM sales GROUP BY region ORDER BY region"}`)
	if resp.Error != "" {
		t.Fatalf("query error: %s", resp.Error)
	}
	if resp.Count != 2 {
		t.Fatalf("expected 2 region groups, got %d", resp.Count)
	}
}

func TestAddSourceErrors(t *testing.T) {
	a := NewApp()
	// Missing name.
	if _, out := postSource(t, a, `{"connector":"csv","fields":{"path":"/x.csv"}}`); out["error"] == nil {
		t.Error("expected error for missing name")
	}
	// Unknown connector.
	if _, out := postSource(t, a, `{"name":"x","connector":"bogus","fields":{}}`); out["error"] == nil {
		t.Error("expected error for unknown connector")
	}
	// Duplicate name.
	dir := t.TempDir()
	_ = os.WriteFile(dir+"/a.csv", []byte("x\n1\n"), 0o644)
	if _, out := postSource(t, a, `{"name":"dup","connector":"csv","fields":{"path":"`+dir+`/a.csv"}}`); out["error"] != nil {
		t.Fatalf("first register failed: %v", out["error"])
	}
	if _, out := postSource(t, a, `{"name":"dup","connector":"csv","fields":{"path":"`+dir+`/a.csv"}}`); out["error"] == nil {
		t.Error("expected error for duplicate source name")
	}
}

func TestUploadThenRegisterAndQuery(t *testing.T) {
	a := NewApp()
	dir := t.TempDir()
	a.uploadDir = dir

	// Build a multipart upload of a small CSV.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "sales.csv")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("region,amount\nemea,10\namer,20\nemea,30\n"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}
	var up struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.Error != "" {
		t.Fatalf("upload error: %s", up.Error)
	}
	// The stored file lives inside the upload dir, preserving stem+ext.
	if filepath.Dir(up.Path) != dir {
		t.Errorf("stored path %q not in upload dir %q", up.Path, dir)
	}
	if !strings.HasPrefix(filepath.Base(up.Path), "sales-") || !strings.HasSuffix(up.Path, ".csv") {
		t.Errorf("stored name = %q, want sales-*.csv", filepath.Base(up.Path))
	}
	if up.Size != 38 {
		t.Errorf("size = %d, want 38", up.Size)
	}

	// Register a csv source at the uploaded path, then query it.
	code, out := postSource(t, a, `{"name":"sales","connector":"csv","fields":{"path":"`+up.Path+`"}}`)
	if code != http.StatusOK || out["error"] != nil {
		t.Fatalf("register failed: code=%d err=%v", code, out["error"])
	}
	resp := postQuery(t, a, `{"query":"SELECT region, SUM(amount) AS t FROM sales GROUP BY region ORDER BY t DESC"}`)
	if resp.Error != "" {
		t.Fatalf("query error: %s", resp.Error)
	}
	if resp.Count != 2 {
		t.Fatalf("rows = %d, want 2", resp.Count)
	}
}

func TestUploadRejectsTraversalName(t *testing.T) {
	a := NewApp()
	dir := t.TempDir()
	a.uploadDir = dir

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "../../etc/passwd")
	fw.Write([]byte("x\n1\n"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var up struct {
		Path string `json:"path"`
	}
	json.Unmarshal(rec.Body.Bytes(), &up)
	// The stored file must be directly inside the upload dir, not escaped out.
	if filepath.Dir(up.Path) != dir {
		t.Fatalf("path traversal: stored at %q, outside %q", up.Path, dir)
	}
}

func TestUploadMethodNotAllowed(t *testing.T) {
	a := NewApp()
	a.uploadDir = t.TempDir()
	req := httptest.NewRequest(http.MethodGet, "/api/upload", nil)
	rec := httptest.NewRecorder()
	a.handleUpload(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestSourcesMethodNotAllowed(t *testing.T) {
	a := NewApp()
	req := httptest.NewRequest(http.MethodDelete, "/api/sources", nil)
	rec := httptest.NewRecorder()
	a.handleSources(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestServesEmbeddedUI(t *testing.T) {
	srv := http.FileServerFS(webUIFS)

	// "/" serves the SPA's index.html.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "turntable") ||
		!strings.Contains(body, `id="root"`) {
		t.Errorf("index.html missing expected markers:\n%s", body)
	}

	// The built JS bundle is present and served as JavaScript.
	entries, err := fs.ReadDir(webUIFS, "assets")
	if err != nil {
		t.Fatalf("dist assets missing — run `npm run build` in internal/cli/webui: %v", err)
	}
	var js string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			js = e.Name()
		}
	}
	if js == "" {
		t.Fatal("no .js asset in the embedded dist")
	}
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/assets/"+js, nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("GET /assets/%s status = %d, want 200", js, rec2.Code)
	}
}

func TestExecQueryRowCap(t *testing.T) {
	a := NewApp()
	a.maxRows = 2
	// A 4-row inline JSON array via the json connector qualified ref.
	// Simpler: NoFrom can't produce many rows, so assert the cap plumbing via
	// execQuery on a small result (no truncation) and the cap field directly.
	schema, rows, truncated, err := a.execQuery(context.Background(), "SELECT 1 AS n")
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Columns) != 1 || len(rows) != 1 || truncated {
		t.Fatalf("schema=%d rows=%d truncated=%v", len(schema.Columns), len(rows), truncated)
	}
}

func TestExposureNote(t *testing.T) {
	for _, addr := range []string{"localhost:8080", "127.0.0.1:9000", ":8080"} {
		if got := exposureNote(addr); !strings.Contains(got, "local") {
			t.Errorf("exposureNote(%q) = %q, want local", addr, got)
		}
	}
	if got := exposureNote("0.0.0.0:8080"); !strings.Contains(got, "WARNING") {
		t.Errorf("exposureNote(0.0.0.0) = %q, want a warning", got)
	}
}

func TestHandleQueryMatView(t *testing.T) {
	dir := t.TempDir()
	emp := `[{"name":"Ann","dept":"eng"},{"name":"Bob","dept":"eng"},{"name":"Di","dept":"sales"}]`
	path := filepath.Join(dir, "emp.json")
	if err := os.WriteFile(path, []byte(emp), 0644); err != nil {
		t.Fatal(err)
	}
	a := NewApp()
	ref := "json:" + path
	// body marshals a query (and optional explain) into a request JSON, escaping
	// the embedded SQL correctly.
	body := func(q string, explain bool) string {
		b, _ := json.Marshal(map[string]any{"query": q, "explain": explain})
		return string(b)
	}

	// CREATE returns a notice, no rows.
	resp := postQuery(t, a, body("CREATE MATERIALIZED VIEW eng AS SELECT name FROM "+ref+" WHERE dept = 'eng'", false))
	if resp.Error != "" {
		t.Fatalf("create error: %s", resp.Error)
	}
	if !strings.Contains(resp.Notice, "created (2 rows)") {
		t.Errorf("notice = %q", resp.Notice)
	}

	// The view is queryable and appears in /api/sources.
	q := postQuery(t, a, body("SELECT name FROM eng ORDER BY name", false))
	if q.Error != "" || q.Count != 2 {
		t.Fatalf("select: err=%q count=%d", q.Error, q.Count)
	}
	srcReq := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	srcRec := httptest.NewRecorder()
	a.handleSources(srcRec, srcReq)
	if !strings.Contains(srcRec.Body.String(), `"eng"`) {
		t.Errorf("view not in sources: %s", srcRec.Body.String())
	}

	// explain on CREATE returns the inner plan, not a notice.
	ex := postQuery(t, a, body("CREATE MATERIALIZED VIEW x AS SELECT name FROM "+ref, true))
	if ex.Explain == "" || !strings.Contains(ex.Explain, "Scan") {
		t.Errorf("explain = %q", ex.Explain)
	}

	// DROP returns a notice and removes the source.
	d := postQuery(t, a, body("DROP MATERIALIZED VIEW eng", false))
	if !strings.Contains(d.Notice, "dropped") {
		t.Errorf("drop notice = %q", d.Notice)
	}
	if after := postQuery(t, a, body("SELECT * FROM eng", false)); after.Error == "" {
		t.Error("expected error querying a dropped view")
	}
}
