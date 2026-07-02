package cli

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/april/turntable/internal/config"
	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/logc"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/loginfer"
	"github.com/april/turntable/internal/plan"
	"github.com/april/turntable/internal/sql"
)

// webUIDist holds the built React/Vite single-page app. The source lives in
// webui/ and is compiled to webui/dist/ (committed) by `npm run build` — see
// the //go:generate directive and webui/README.md. The dist tree is embedded
// so `go build` needs no Node toolchain.
//
//go:generate sh -c "cd webui && npm install && npm run build"
//go:embed all:webui/dist
var webUIDist embed.FS

// webUIFS is the dist directory rooted at its top level (webui/dist -> /).
var webUIFS = mustSubFS(webUIDist, "webui/dist")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("webui dist embed: " + err.Error())
	}
	return sub
}

// defaultServeMaxRows caps how many rows a single web query returns when no
// --max-rows is set, so the browser never has to render an unbounded result.
const defaultServeMaxRows = 5000

// uploadDirPath is the project-relative directory where web uploads are stored.
// It is persistent (kept across restarts) so a source saved to the config can
// point at an uploaded file by this stable relative path.
const uploadDirPath = ".turntable/data"

// serve runs the web query UI on addr until the context is cancelled. It is a
// browser-based complement to the REPL: the same parse/plan/exec path, exposed
// over HTTP as a small JSON API plus a single-page UI.
func (a *App) serve(ctx context.Context, addr string) int {
	// A persistent, project-relative directory for files uploaded through the UI.
	// Unlike a temp dir it survives restarts, so a saved source pointing at an
	// uploaded file keeps working. Files accumulate here until the user prunes it.
	if err := os.MkdirAll(uploadDirPath, 0o755); err != nil {
		fmt.Fprintf(a.Err, "serve: cannot create upload dir %q: %v\n", uploadDirPath, err)
		return 1
	}
	a.uploadDir = uploadDirPath
	a.dashDir = dashDirPath // created lazily on first dashboard save

	mux := http.NewServeMux()
	// Serve the embedded SPA (index.html + hashed assets) at the root; the API
	// routes below are more specific, so they take precedence in the mux.
	mux.Handle("/", http.FileServerFS(webUIFS))
	mux.HandleFunc("/api/query", a.handleQuery)
	mux.HandleFunc("/api/sources", a.handleSources)
	mux.HandleFunc("/api/schema", a.handleSchema)
	mux.HandleFunc("/api/upload", a.handleUpload)
	mux.HandleFunc("/api/functions", a.handleFunctions)
	mux.HandleFunc("/api/connectors", a.handleConnectors)
	mux.HandleFunc("/api/loginfer", a.handleLoginfer)
	mux.HandleFunc("/api/dashboards", a.handleDashboards)
	mux.HandleFunc("/api/dashboards/", a.handleDashboardItem)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(a.Err, "serve: cannot listen on %q: %v\n", addr, err)
		return 1
	}
	fmt.Fprintf(a.Err, "turntable web UI on http://%s  — read-only queries; sources can be added/uploaded at runtime (%s)\n", ln.Addr(), exposureNote(addr))

	// Shut down cleanly when the process is interrupted.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(a.Err, "serve error: %v\n", err)
		return 1
	}
	return 0
}

// exposureNote describes how reachable the bound address is, so the operator
// understands that queries run with this process's file and network access.
func exposureNote(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return "local only"
	default:
		return "WARNING: reachable from other hosts — queries run with this process's access"
	}
}

// ---- JSON API ----------------------------------------------------------------

type queryRequest struct {
	Query   string `json:"query"`
	Explain bool   `json:"explain"`
}

type apiColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

type queryResponse struct {
	Columns []apiColumn `json:"columns,omitempty"`
	// NB: no omitempty — a zero-row result must still serialize "rows": [] (the
	// success path always builds a non-nil slice). Omitting it left the web
	// UI's result.rows undefined, crashing the results pane on any empty
	// result. Error/notice/explain responses serialize "rows": null, which the
	// frontend never reads (isTable is false).
	Rows [][]any `json:"rows"`
	Count     int         `json:"count"`
	ElapsedMs int64       `json:"elapsed_ms"`
	Truncated bool        `json:"truncated,omitempty"`
	Explain   string      `json:"explain,omitempty"`
	// Notice carries the human-readable result of a session statement that
	// produces no rows (e.g. CREATE/REFRESH/DROP MATERIALIZED VIEW).
	Notice string `json:"notice,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handleQuery runs one SELECT and returns columns + rows as JSON. Query errors
// are returned in the Error field with HTTP 200 so the UI can display them
// inline; only malformed requests get a 4xx.
func (a *App) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req queryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSON(w, queryResponse{Error: "empty query"})
		return
	}

	start := time.Now()

	stmt, perr := sql.Parse(req.Query)
	if perr != nil {
		writeJSON(w, queryResponse{Error: "parse error: " + perr.Error(), ElapsedMs: time.Since(start).Milliseconds()})
		return
	}

	// View statements (CREATE/REFRESH/DROP [MATERIALIZED] VIEW) are session
	// commands, not row queries: handle them here (the plan/exec path does not
	// support them) and return a notice.
	if resp, ok := a.sessionStmtResponse(r.Context(), stmt, req.Explain); ok {
		resp.ElapsedMs = time.Since(start).Milliseconds()
		writeJSON(w, resp)
		return
	}

	if req.Explain {
		text, err := a.explainStmt(r.Context(), stmt)
		resp := queryResponse{Explain: text, ElapsedMs: time.Since(start).Milliseconds()}
		if err != nil {
			resp.Error = err.Error()
		}
		writeJSON(w, resp)
		return
	}

	schema, rows, truncated, err := a.execStmt(r.Context(), stmt)
	if err != nil {
		writeJSON(w, queryResponse{Error: err.Error(), ElapsedMs: time.Since(start).Milliseconds()})
		return
	}

	resp := queryResponse{
		Columns:   apiColumns(schema),
		Rows:      jsonRows(rows),
		Count:     len(rows),
		Truncated: truncated,
		ElapsedMs: time.Since(start).Milliseconds(),
	}
	writeJSON(w, resp)
}

// sessionStmtResponse handles a view session statement (regular or materialized)
// for the web API, returning a filled queryResponse and true. For any other
// statement it returns (zero, false) so the caller proceeds with the normal
// plan/exec path. Under explain it returns the inner/stored query's plan instead
// of running anything.
func (a *App) sessionStmtResponse(ctx context.Context, stmt sql.Statement, explain bool) (queryResponse, bool) {
	var notice string
	var err error
	switch s := stmt.(type) {
	case *sql.CreateMatViewStmt:
		if explain {
			return a.explainResponse(ctx, s.Query), true
		}
		notice, err = a.createMatViewCore(ctx, s)
	case *sql.RefreshMatViewStmt:
		if explain {
			mv, ok := a.matViews[s.Name]
			if !ok {
				return queryResponse{Error: fmt.Sprintf("materialized view %q does not exist", s.Name)}, true
			}
			return a.explainResponse(ctx, mv.query), true
		}
		notice, err = a.refreshMatViewCore(ctx, s)
	case *sql.DropMatViewStmt:
		if explain {
			return queryResponse{Notice: fmt.Sprintf("DROP MATERIALIZED VIEW %s (no plan)", s.Name)}, true
		}
		notice, err = a.dropMatViewCore(s)
	case *sql.CreateViewStmt:
		if explain {
			return a.explainResponse(ctx, s.Query), true
		}
		notice, err = a.createViewCore(ctx, s)
	case *sql.DropViewStmt:
		if explain {
			return queryResponse{Notice: fmt.Sprintf("DROP VIEW %s (no plan)", s.Name)}, true
		}
		notice, err = a.dropViewCore(s)
	default:
		return queryResponse{}, false
	}
	if err != nil {
		return queryResponse{Error: err.Error()}, true
	}
	return queryResponse{Notice: notice}, true
}

// explainResponse builds query and returns its plan as an explain response.
func (a *App) explainResponse(ctx context.Context, query sql.Statement) queryResponse {
	text, err := a.explainStmt(ctx, query)
	if err != nil {
		return queryResponse{Error: err.Error()}
	}
	return queryResponse{Explain: text}
}

// execQuery parses and executes a query (used by tests); execStmt does the work.
func (a *App) execQuery(ctx context.Context, query string) (engine.Schema, []engine.Row, bool, error) {
	stmt, err := sql.Parse(query)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("parse error: %w", err)
	}
	return a.execStmt(ctx, stmt)
}

// execStmt plans and executes a SELECT, returning the schema and rows (capped).
// The row cap honors --max-rows, else defaultServeMaxRows.
func (a *App) execStmt(ctx context.Context, stmt sql.Statement) (engine.Schema, []engine.Row, bool, error) {
	rowCap := a.maxRows
	if rowCap <= 0 {
		rowCap = defaultServeMaxRows
	}
	return a.execStmtCapped(ctx, stmt, rowCap)
}

// execStmtCapped plans and executes a SELECT, returning the schema and up to
// rowCap rows, plus whether the result was truncated at the cap.
func (a *App) execStmtCapped(ctx context.Context, stmt sql.Statement, rowCap int) (engine.Schema, []engine.Row, bool, error) {
	p, err := plan.Build(ctx, stmt, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("plan error: %w", err)
	}
	it, schema, err := plan.Exec(ctx, p)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("exec error: %w", err)
	}

	// Read one past the cap so we can report truncation.
	readLimit := rowCap + 1
	capped := engine.NewLimitIter(it, &readLimit, 0)
	rows, err := engine.Materialize(ctx, capped)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("exec error: %w", err)
	}
	truncated := false
	if len(rows) > rowCap {
		rows = rows[:rowCap]
		truncated = true
	}
	return schema, rows, truncated, nil
}

func (a *App) explainQuery(ctx context.Context, query string) (string, error) {
	stmt, err := sql.Parse(query)
	if err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}
	return a.explainStmt(ctx, stmt)
}

func (a *App) explainStmt(ctx context.Context, stmt sql.Statement) (string, error) {
	p, err := plan.Build(ctx, stmt, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		return "", fmt.Errorf("plan error: %w", err)
	}
	return formatPlan(p.Root, 0), nil
}

// handleSources lists registered sources (GET) or registers a new one (POST).
func (a *App) handleSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listSources(w, r)
	case http.MethodPost:
		a.addSource(w, r)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (a *App) listSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.sourceList())
}

// srcInfo is one source-list entry, shared by the web API and the MCP server.
type srcInfo struct {
	Name      string `json:"name"`
	Connector string `json:"connector" jsonschema:"the connector prefix, or view for a regular view"`
}

// sourceList returns the registered sources plus regular views, sorted by name.
func (a *App) sourceList() []srcInfo {
	sources := a.Reg.Sources()
	out := make([]srcInfo, 0, len(sources))
	for _, s := range sources {
		out = append(out, srcInfo{Name: s.Name, Connector: connectorName(s)})
	}
	for _, v := range a.Reg.ViewNames() {
		out = append(out, srcInfo{Name: v, Connector: "view"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type addSourceRequest struct {
	Name      string            `json:"name"`
	Connector string            `json:"connector"`
	Fields    map[string]string `json:"fields"`
	Save      bool              `json:"save"` // also persist to the config file
}

// addSource registers a source at runtime — the web equivalent of the REPL's
// .use — through registerRuntimeSource, so behavior (sensitive-field validation,
// ${ENV_VAR} interpolation, wildcards, option routing) is identical. Registration
// errors are returned in the JSON body so the UI can show them inline. With
// save=true the (declared, secret-free) source is also appended to the config.
func (a *App) addSource(w http.ResponseWriter, r *http.Request) {
	var req addSourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Connector) == "" {
		writeJSON(w, map[string]any{"error": "name and connector are required"})
		return
	}
	src := config.Source{Connector: req.Connector}
	for k, v := range req.Fields {
		applySourceField(&src, k, v)
	}
	names, err := a.registerRuntimeSource(r.Context(), req.Name, src)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	resp := map[string]any{"registered": names}
	if req.Save {
		if err := config.AppendSource(a.configPath, req.Name, src); err != nil {
			resp["saveError"] = err.Error()
		} else {
			resp["saved"] = a.configPath
		}
	}
	writeJSON(w, resp)
}

// handleSchema returns the columns of a named source (introspection, like the
// REPL's .schema). Resolving may touch the network for remote connectors, so
// it runs on demand rather than at startup.
func (a *App) handleSchema(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("source")
	s, ok := a.Reg.Resolve(name)
	if !ok {
		// A view has no connector; resolve its schema by planning the query.
		if schema, isView, verr := a.viewSchemaFor(r.Context(), name); isView {
			if verr != nil {
				writeJSON(w, map[string]any{"error": verr.Error()})
				return
			}
			a.writeSchemaJSON(w, name, schema, nil)
			return
		}
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	schema, err := s.Conn.Resolve(r.Context(), s.Dataset)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	a.writeSchemaJSON(w, name, schema, fileMeta(s))
}

// fileMetaInfo describes the backing file of a local-file source: its path,
// last-modified time (RFC3339), and size in bytes.
type fileMetaInfo struct {
	Path     string
	Modified string
	Size     int64
}

// fileMeta returns the source file's metadata when the source is a local-file
// connector with a stat-able path — so clients can show how fresh a file
// source is (it is read live on every query). Nil for non-file or unreachable
// sources.
func fileMeta(s connector.Source) *fileMetaInfo {
	if !isFileConnector(s.Conn.Name()) {
		return nil
	}
	fi, err := os.Stat(s.Dataset.Source)
	if err != nil || fi.IsDir() {
		return nil
	}
	return &fileMetaInfo{
		Path:     s.Dataset.Source,
		Modified: fi.ModTime().UTC().Format(time.RFC3339),
		Size:     fi.Size(),
	}
}

// apiColumns converts an engine schema's columns to the JSON column shape.
func apiColumns(schema engine.Schema) []apiColumn {
	cols := make([]apiColumn, len(schema.Columns))
	for i, c := range schema.Columns {
		cols[i] = apiColumn{Name: c.Name, Type: c.Type.String(), Nullable: c.Nullable}
	}
	return cols
}

// writeSchemaJSON writes a source/view schema as the columns response, merging
// the file metadata (modified-time/size) when present.
func (a *App) writeSchemaJSON(w http.ResponseWriter, name string, schema engine.Schema, fm *fileMetaInfo) {
	resp := map[string]any{"source": name, "columns": apiColumns(schema)}
	if fm != nil {
		resp["path"] = fm.Path
		resp["modified"] = fm.Modified
		resp["size"] = fm.Size
	}
	writeJSON(w, resp)
}

// handleConnectors lists the connector field specs (connspec.go) that drive
// the add-source modal and the MCP list_connectors tool.
func (a *App) handleConnectors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, connectorSpecs)
}

// handleFunctions lists the SQL functions available in the dialect (the same
// data as the REPL `.functions` command), for editor autocompletion and
// discovery. Scalar and aggregate names are reported separately.
func (a *App) handleFunctions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"scalar":    a.Funcs.Names(),
		"aggregate": engine.Aggregates(),
		"keywords":  sqlKeywords,
	})
}

// sqlKeywords are the dialect keywords offered as editor completions (the
// grammar lives in internal/sql; this list is for UX only).
var sqlKeywords = []string{
	"SELECT", "DISTINCT", "FROM", "WHERE", "GROUP BY", "HAVING", "ORDER BY",
	"ASC", "DESC", "LIMIT", "OFFSET", "JOIN", "INNER JOIN", "LEFT JOIN",
	"RIGHT JOIN", "FULL JOIN", "ON", "AND", "OR", "NOT", "IN", "EXISTS",
	"BETWEEN", "LIKE", "ILIKE", "IS NULL", "IS NOT NULL", "AS", "CASE", "WHEN",
	"THEN", "ELSE", "END", "CAST", "UNION", "UNION ALL", "INTERSECT", "EXCEPT",
	"WITH", "OVER", "PARTITION BY",
}

// loginferResponse is the result of analyzing a log file for the add-source UI:
// either a recognized format (with a column/row preview) or, for an unrecognized
// file, a set of inferred templates with ready-to-use patterns.
type loginferResponse struct {
	Detected  *detectedFormat     `json:"detected,omitempty"`
	Templates []loginfer.Template `json:"templates,omitempty"`
	Error     string              `json:"error,omitempty"`
}

type detectedFormat struct {
	Format  string      `json:"format"`
	Columns []apiColumn `json:"columns"`
	Rows    [][]any     `json:"rows"`
}

// handleLoginfer analyzes a log file path: if the connector recognizes the
// format it returns that plus a small parsed preview; otherwise it mines the
// sampled lines into templates, each carrying a `pattern` regex the client can
// use directly. POST { "path": "..." }.
func (a *App) handleLoginfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSON(w, loginferResponse{Error: "path required"})
		return
	}

	name, schema, err := logc.Detect(req.Path, nil)
	if err != nil {
		writeJSON(w, loginferResponse{Error: err.Error()})
		return
	}
	if name != "raw" {
		rows, _ := a.logPreviewRows(r.Context(), req.Path, nil, 5)
		writeJSON(w, loginferResponse{Detected: &detectedFormat{Format: name, Columns: apiColumns(schema), Rows: rows}})
		return
	}

	sample, err := logc.Sample(req.Path, 500)
	if err != nil {
		writeJSON(w, loginferResponse{Error: err.Error()})
		return
	}
	writeJSON(w, loginferResponse{Templates: loginfer.Infer(sample)})
}

// logPreviewRows scans a few rows from a log file through the log connector for
// the detected-format preview.
func (a *App) logPreviewRows(ctx context.Context, path string, opts map[string]any, n int) ([][]any, error) {
	ds := connector.Dataset{Source: path, Options: opts}
	it, err := logc.New().Scan(ctx, connector.ScanRequest{Dataset: ds})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out [][]any
	for len(out) < n {
		row, ok, err := it.Next()
		if err != nil || !ok {
			break
		}
		cells := make([]any, len(row.Values))
		for j, v := range row.Values {
			cells[j] = jsonValue(v)
		}
		out = append(out, cells)
	}
	return out, nil
}

// maxUploadBytes bounds a single uploaded file (a guard, not a hard product
// limit). Data is streamed to disk, not buffered in memory.
const maxUploadBytes = 512 << 20 // 512 MiB

// handleUpload accepts a multipart file upload, stores it under the persistent
// upload directory (.turntable/data), and returns the stored relative path. The
// client then registers a file-connector source pointing at that path via POST
// /api/sources — which can be saved to the config, since the path is durable.
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if a.uploadDir == "" {
		writeJSON(w, map[string]any{"error": "uploads are not available"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Keep the original (path-stripped, sanitized) name so the stored path is
	// predictable; a name clash gets a "-N" suffix rather than clobbering.
	base := sanitizeFilename(filepath.Base(header.Filename))
	if strings.TrimSuffix(base, filepath.Ext(base)) == "" {
		base = "upload" + filepath.Ext(base)
	}
	dst, err := createUpload(a.uploadDir, base)
	if err != nil {
		writeJSON(w, map[string]any{"error": "cannot store upload: " + err.Error()})
		return
	}
	defer dst.Close()

	n, err := io.Copy(dst, file)
	if err != nil {
		os.Remove(dst.Name())
		writeJSON(w, map[string]any{"error": "write upload: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"path":     filepath.ToSlash(dst.Name()),
		"filename": filepath.Base(dst.Name()),
		"size":     n,
	})
}

// createUpload atomically creates a new file in dir named base, appending "-N"
// to the stem on a name clash (O_EXCL avoids races and never overwrites). It
// gives up after a bounded number of attempts.
func createUpload(dir, base string) (*os.File, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; i < 10000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("too many files named like %q", base)
}

// sanitizeFilename reduces a filename to a safe set of characters, preventing
// path traversal and odd names. It keeps letters, digits, dot, dash, underscore.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".") // no leading dots (hidden/relative)
	if out == "" {
		return "upload"
	}
	return out
}

// jsonRows converts engine rows to positional JSON-encodable cell arrays.
func jsonRows(rows []engine.Row) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		cells := make([]any, len(row.Values))
		for j, v := range row.Values {
			cells[j] = jsonValue(v)
		}
		out[i] = cells
	}
	return out
}

// jsonValue converts an engine.Value into a JSON-encodable Go value: NULL ->
// null, time -> RFC3339 string, everything else its native value.
func jsonValue(v engine.Value) any {
	if v.IsNull() {
		return nil
	}
	if v.Type == engine.TypeTime {
		if t, ok := v.V.(time.Time); ok {
			return t.Format(time.RFC3339)
		}
	}
	return v.V
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
