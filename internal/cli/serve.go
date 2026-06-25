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
	"github.com/april/turntable/internal/engine"
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

// serve runs the web query UI on addr until the context is cancelled. It is a
// browser-based complement to the REPL: the same parse/plan/exec path, exposed
// over HTTP as a small JSON API plus a single-page UI.
func (a *App) serve(ctx context.Context, addr string) int {
	// A per-session scratch directory for files uploaded through the UI. It is
	// removed when serve() returns (process shutdown).
	dir, err := os.MkdirTemp("", "turntable-uploads-")
	if err != nil {
		fmt.Fprintf(a.Err, "serve: cannot create upload dir: %v\n", err)
		return 1
	}
	a.uploadDir = dir
	defer os.RemoveAll(dir)

	mux := http.NewServeMux()
	// Serve the embedded SPA (index.html + hashed assets) at the root; the API
	// routes below are more specific, so they take precedence in the mux.
	mux.Handle("/", http.FileServerFS(webUIFS))
	mux.HandleFunc("/api/query", a.handleQuery)
	mux.HandleFunc("/api/sources", a.handleSources)
	mux.HandleFunc("/api/schema", a.handleSchema)
	mux.HandleFunc("/api/upload", a.handleUpload)
	mux.HandleFunc("/api/functions", a.handleFunctions)

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
	Columns   []apiColumn `json:"columns,omitempty"`
	Rows      [][]any     `json:"rows,omitempty"`
	Count     int         `json:"count"`
	ElapsedMs int64       `json:"elapsed_ms"`
	Truncated bool        `json:"truncated,omitempty"`
	Explain   string      `json:"explain,omitempty"`
	Error     string      `json:"error,omitempty"`
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
	if req.Explain {
		text, err := a.explainQuery(r.Context(), req.Query)
		resp := queryResponse{Explain: text, ElapsedMs: time.Since(start).Milliseconds()}
		if err != nil {
			resp.Error = err.Error()
		}
		writeJSON(w, resp)
		return
	}

	schema, rows, truncated, err := a.execQuery(r.Context(), req.Query)
	if err != nil {
		writeJSON(w, queryResponse{Error: err.Error(), ElapsedMs: time.Since(start).Milliseconds()})
		return
	}

	resp := queryResponse{
		Columns:   make([]apiColumn, len(schema.Columns)),
		Rows:      make([][]any, len(rows)),
		Count:     len(rows),
		Truncated: truncated,
		ElapsedMs: time.Since(start).Milliseconds(),
	}
	for i, c := range schema.Columns {
		resp.Columns[i] = apiColumn{Name: c.Name, Type: c.Type.String(), Nullable: c.Nullable}
	}
	for i, row := range rows {
		cells := make([]any, len(row.Values))
		for j, v := range row.Values {
			cells[j] = jsonValue(v)
		}
		resp.Rows[i] = cells
	}
	writeJSON(w, resp)
}

// execQuery parses, plans, and executes a SELECT, returning the schema and rows
// (capped). The row cap honors --max-rows, else defaultServeMaxRows.
func (a *App) execQuery(ctx context.Context, query string) (engine.Schema, []engine.Row, bool, error) {
	stmt, err := sql.Parse(query)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("parse error: %w", err)
	}
	p, err := plan.Build(ctx, stmt, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("plan error: %w", err)
	}
	it, schema, err := plan.Exec(ctx, p)
	if err != nil {
		return engine.Schema{}, nil, false, fmt.Errorf("exec error: %w", err)
	}

	rowCap := a.maxRows
	if rowCap <= 0 {
		rowCap = defaultServeMaxRows
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
	type srcInfo struct {
		Name      string `json:"name"`
		Connector string `json:"connector"`
	}
	sources := a.Reg.Sources()
	out := make([]srcInfo, 0, len(sources))
	for _, s := range sources {
		out = append(out, srcInfo{Name: s.Name, Connector: connectorName(s)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

type addSourceRequest struct {
	Name      string            `json:"name"`
	Connector string            `json:"connector"`
	Fields    map[string]string `json:"fields"`
}

// addSource registers a source at runtime — the web equivalent of the REPL's
// .use — through the same registerSourceExpand path used by config loading, so
// behavior (wildcards, validation, option routing) is identical. Registration
// errors are returned in the JSON body so the UI can show them inline.
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
	names, err := a.registerSourceExpand(r.Context(), req.Name, src)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"registered": names})
}

// handleSchema returns the columns of a named source (introspection, like the
// REPL's .schema). Resolving may touch the network for remote connectors, so
// it runs on demand rather than at startup.
func (a *App) handleSchema(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("source")
	s, ok := a.Reg.Resolve(name)
	if !ok {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	schema, err := s.Conn.Resolve(r.Context(), s.Dataset)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	cols := make([]apiColumn, len(schema.Columns))
	for i, c := range schema.Columns {
		cols[i] = apiColumn{Name: c.Name, Type: c.Type.String(), Nullable: c.Nullable}
	}
	writeJSON(w, map[string]any{"source": name, "columns": cols})
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

// maxUploadBytes bounds a single uploaded file (a guard, not a hard product
// limit). Data is streamed to disk, not buffered in memory.
const maxUploadBytes = 512 << 20 // 512 MiB

// handleUpload accepts a multipart file upload, stores it in the per-session
// upload directory, and returns the stored path. The client then registers a
// file-connector source pointing at that path via POST /api/sources. The file
// stays on disk only for the life of the server process.
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

	// Build a safe destination name from the original (path-stripped) filename,
	// preserving the extension; CreateTemp injects randomness for uniqueness and
	// confines the file to uploadDir.
	base := sanitizeFilename(filepath.Base(header.Filename))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = "upload"
	}
	dst, err := os.CreateTemp(a.uploadDir, stem+"-*"+ext)
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
		"path":     dst.Name(),
		"filename": base,
		"size":     n,
	})
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
