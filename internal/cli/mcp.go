package cli

// MCP server: `turntable mcp` exposes turntable to MCP clients (Claude Code
// and friends) over stdio — a third transport over the same App methods as
// the web API (serve.go) and the REPL. See docs/mcp-server-design.md.
//
// Stdout carries only JSON-RPC frames in this mode: nothing here may write to
// a.Out. Startup warnings already go to a.Err (stderr), which MCP clients
// surface as server logs.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/april/turntable/internal/config"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// defaultMCPMaxRows caps the rows a query tool call returns when the call does
// not ask for more. Unlike the web cap (a rendering guard) this is a token
// budget — rows land in the model's context — so the default is small and the
// truncated flag nudges the agent to aggregate instead of page.
const defaultMCPMaxRows = 200

// mcpCmd runs the MCP stdio server until the client disconnects or ctx is
// cancelled. Flags, config, .env, and declared sources were already handled by
// Run, so `-c`, `--max-rows`, and `--strict` all apply here.
func (a *App) mcpCmd(ctx context.Context, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(a.Err, "usage: turntable [flags] mcp   (no arguments; flags like -c go before the subcommand)")
		return 1
	}
	if a.dashDir == "" {
		a.dashDir = dashDirPath // dashboard tools read/write the standard store
	}
	err := a.newMCPServer().Run(ctx, &mcp.StdioTransport{})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fmt.Fprintf(a.Err, "mcp: %v\n", err)
		return 1
	}
	return 0
}

// newMCPServer builds the MCP server with the tool set registered. Split from
// mcpCmd so tests can connect over an in-memory transport.
func (a *App) newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "turntable",
		Title:   "turntable — query heterogeneous data sources with SQL",
		Version: mcpVersion(),
	}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "query",
		Description: "Run a query in turntable's SQL dialect against the registered data sources " +
			"(files, databases, APIs — see list_sources; joins across different sources work). " +
			"Largely standard SELECT with CTEs, window functions, and set operations; FROM takes a " +
			"registered source name, a view, or an inline connector-qualified ref like csv:./data.csv. " +
			"Session statements (CREATE [OR REPLACE] VIEW, CREATE/REFRESH/DROP MATERIALIZED VIEW) are " +
			"accepted too and return a notice instead of rows. Read-only against the underlying data: " +
			"no INSERT/UPDATE/DELETE/DDL. Results are row-capped; prefer WHERE/aggregation over paging.",
	}, a.mcpQuery)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_sources",
		Description: "List the data sources and views currently registered (name + connector). " +
			"Any listed name can be used in a query's FROM clause.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpListSources)

	mcp.AddTool(s, &mcp.Tool{
		Name: "describe_source",
		Description: "Get the columns and types of a registered source or view. For local-file " +
			"sources also reports the backing file's path, modified time, and size. May contact the " +
			"remote system for database/API sources.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpDescribeSource)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_functions",
		Description: "List the scalar functions, aggregate functions, and keywords of turntable's " +
			"SQL dialect — the live registry, so prefer this over assuming standard SQL coverage.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpListFunctions)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_connectors",
		Description: "List the connectors available for add_source, each with its field specs: " +
			"key, whether it is required, select options, and whether it is sensitive. Sensitive " +
			"fields (credentials) must be passed as a sole ${ENV_VAR} reference, never a literal — " +
			"tool inputs are recorded in conversation transcripts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpListConnectors)

	mcp.AddTool(s, &mcp.Tool{
		Name: "add_source",
		Description: "Register a data source at runtime so queries can reference it by name — the " +
			"same path as the REPL's .use and the web UI's add-source form. See list_connectors for " +
			"each connector's fields; file connectors take a `path` field. Wildcards expand (sql " +
			"table=* registers every table; excel sheet=*; the response lists the registered names). " +
			"Credentials must be ${ENV_VAR} references. The source lasts for this server's lifetime; " +
			"pass save=true to also persist it (secret-free, declared form) to the config file.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, a.mcpAddSource)

	mcp.AddTool(s, &mcp.Tool{
		Name: "remove_source",
		Description: "Unregister a runtime source by name (undo for add_source, including one name " +
			"of a wildcard expansion). Does not touch the config file, materialized views (use DROP " +
			"MATERIALIZED VIEW), or regular views (use DROP VIEW).",
	}, a.mcpRemoveSource)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_dashboards",
		Description: "List the saved dashboards (one YAML file each under .turntable/dashboards/): " +
			"slug, name, description, panel count. A file that fails to parse is listed with its error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpListDashboards)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_dashboard",
		Description: "Get a dashboard's full definition (panels, variables, view configs) by slug — read it before modifying, then save_dashboard the whole updated definition back.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.mcpGetDashboard)

	mcp.AddTool(s, &mcp.Tool{
		Name: "save_dashboard",
		Description: "Create or update a dashboard (an upsert keyed by slug; empty slug derives one " +
			"from the name). Panels render top-to-bottom (width: full|half; half panels pair up). " +
			"Kinds: markdown (needs text), table/pivot/chart/stat (need a query). Queries may use " +
			"{{var}} substitution — quoted-literal, {{var:raw}} raw — with defaults declared under " +
			"variables. A panel's view holds per-kind settings referencing result columns BY NAME. " +
			"Example panel list: " +
			`[{"kind":"markdown","text":"# Sales\nPaid orders by region."},` +
			`{"kind":"chart","title":"Revenue","query":"SELECT region, SUM(amount) AS revenue FROM orders GROUP BY region",` +
			`"view":{"chart":{"type":"bar","x":"region","y":["revenue"]}}},` +
			`{"kind":"stat","title":"Total","width":"half","query":"SELECT ROUND(SUM(amount),2) AS total FROM orders"}]. ` +
			"Chart view keys: type (bar|line|area|pie|scatter), x, y (list), y2 (right axis), bandLo/bandHi, " +
			"thresholds (numbers). Pivot view keys: rows, cols, value, agg (sum|count|avg|min|max).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, a.mcpSaveDashboard)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_dashboard",
		Description: "Delete a saved dashboard by slug. Permanent (the YAML file is removed).",
	}, a.mcpDeleteDashboard)

	mcp.AddTool(s, &mcp.Tool{
		Name: "render_dashboard",
		Description: "Execute a dashboard's panels and write one self-contained HTML report (no " +
			"server or network needed to view it) — markdown/stat/table/pivot rendered in Go, charts " +
			"via an embedded Chart.js. Variables default from the definition; override with the " +
			"variables map. Returns the written file path.",
	}, a.mcpRenderDashboard)

	return s
}

// mcpVersion reports the module's build version when available (installed
// binaries), else "dev" (source builds).
func mcpVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// ---- query ---------------------------------------------------------------

type mcpQueryArgs struct {
	SQL     string `json:"sql" jsonschema:"the query to run, in turntable's SQL dialect"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"maximum rows to return (default 200); a larger result sets truncated=true"`
	Explain bool   `json:"explain,omitempty" jsonschema:"return the query plan instead of executing"`
}

type mcpQueryOut struct {
	Columns   []apiColumn `json:"columns,omitempty"`
	Rows      [][]any     `json:"rows,omitempty" jsonschema:"result rows as positional arrays aligned with columns"`
	Count     int         `json:"count" jsonschema:"number of rows returned (after the cap)"`
	Truncated bool        `json:"truncated,omitempty" jsonschema:"true when the result was cut at the row cap"`
	Hint      string      `json:"hint,omitempty"`
	ElapsedMs int64       `json:"elapsed_ms"`
	Explain   string      `json:"explain,omitempty" jsonschema:"the query plan, when explain was requested"`
	Notice    string      `json:"notice,omitempty" jsonschema:"outcome of a session statement (view/materialized-view DDL)"`
}

// mcpQuery is the query tool: parse, then either handle a session statement,
// explain, or execute with a per-call row cap. Query errors are returned as
// handler errors — the SDK packs them into the result with IsError set, so the
// model sees the message in-band and can self-correct (same policy as the web
// API's HTTP-200-with-error).
func (a *App) mcpQuery(ctx context.Context, req *mcp.CallToolRequest, in mcpQueryArgs) (*mcp.CallToolResult, mcpQueryOut, error) {
	var out mcpQueryOut
	if strings.TrimSpace(in.SQL) == "" {
		return nil, out, errors.New("empty query")
	}
	start := time.Now()

	stmt, err := sql.Parse(in.SQL)
	if err != nil {
		return nil, out, fmt.Errorf("parse error: %w", err)
	}

	// Session statements (view/matview DDL) mutate App state; unlike the
	// single-user web/REPL paths, MCP clients issue tool calls concurrently,
	// so serialize them.
	if isSessionStmt(stmt) {
		a.sessionMu.Lock()
		resp, _ := a.sessionStmtResponse(ctx, stmt, in.Explain)
		a.sessionMu.Unlock()
		out.ElapsedMs = time.Since(start).Milliseconds()
		if resp.Error != "" {
			return nil, out, errors.New(resp.Error)
		}
		out.Notice, out.Explain = resp.Notice, resp.Explain
		return nil, out, nil
	}

	if in.Explain {
		text, err := a.explainStmt(ctx, stmt)
		if err != nil {
			return nil, out, err
		}
		out.Explain = text
		out.ElapsedMs = time.Since(start).Milliseconds()
		return nil, out, nil
	}

	rowCap := a.mcpRowCap(in.MaxRows)
	schema, rows, truncated, err := a.execStmtCapped(ctx, stmt, rowCap)
	if err != nil {
		return nil, out, err
	}
	out.Columns = apiColumns(schema)
	out.Rows = jsonRows(rows)
	out.Count = len(rows)
	out.Truncated = truncated
	out.ElapsedMs = time.Since(start).Milliseconds()
	if truncated {
		out.Hint = fmt.Sprintf("result truncated at %d rows — narrow with WHERE, aggregate, or raise max_rows", rowCap)
	}
	return nil, out, nil
}

// isSessionStmt reports whether stmt is a view/matview session statement (the
// same set sessionStmtResponse handles), so the caller knows to take the lock.
func isSessionStmt(stmt sql.Statement) bool {
	switch stmt.(type) {
	case *sql.CreateMatViewStmt, *sql.RefreshMatViewStmt, *sql.DropMatViewStmt,
		*sql.CreateViewStmt, *sql.DropViewStmt:
		return true
	}
	return false
}

// mcpRowCap resolves a per-call row cap: the requested value (default
// defaultMCPMaxRows), never above the server-wide cap (--max-rows, else the
// web default).
func (a *App) mcpRowCap(requested int) int {
	hard := a.maxRows
	if hard <= 0 {
		hard = defaultServeMaxRows
	}
	if requested <= 0 {
		requested = defaultMCPMaxRows
	}
	return min(requested, hard)
}

// ---- discovery -----------------------------------------------------------

type mcpSourcesOut struct {
	Sources []srcInfo `json:"sources"`
}

func (a *App) mcpListSources(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, mcpSourcesOut, error) {
	return nil, mcpSourcesOut{Sources: a.sourceList()}, nil
}

type mcpDescribeArgs struct {
	Name string `json:"name" jsonschema:"the registered source or view name"`
}

type mcpDescribeOut struct {
	Source   string      `json:"source"`
	Columns  []apiColumn `json:"columns"`
	Path     string      `json:"path,omitempty" jsonschema:"backing file path (local-file sources only)"`
	Modified string      `json:"modified,omitempty" jsonschema:"file last-modified time, RFC3339 (local-file sources only)"`
	Size     int64       `json:"size,omitempty" jsonschema:"file size in bytes (local-file sources only)"`
}

func (a *App) mcpDescribeSource(ctx context.Context, req *mcp.CallToolRequest, in mcpDescribeArgs) (*mcp.CallToolResult, mcpDescribeOut, error) {
	var out mcpDescribeOut
	s, ok := a.Reg.Resolve(in.Name)
	if !ok {
		// A view has no connector; resolve its schema by planning the query.
		if schema, isView, verr := a.viewSchemaFor(ctx, in.Name); isView {
			if verr != nil {
				return nil, out, verr
			}
			return nil, mcpDescribeOut{Source: in.Name, Columns: apiColumns(schema)}, nil
		}
		return nil, out, fmt.Errorf("unknown source %q — list_sources shows what is registered", in.Name)
	}
	schema, err := s.Conn.Resolve(ctx, s.Dataset)
	if err != nil {
		return nil, out, err
	}
	out = mcpDescribeOut{Source: in.Name, Columns: apiColumns(schema)}
	if fm := fileMeta(s); fm != nil {
		out.Path, out.Modified, out.Size = fm.Path, fm.Modified, fm.Size
	}
	return nil, out, nil
}

// ---- source management -----------------------------------------------------

type mcpConnectorsOut struct {
	Connectors []ConnectorSpec `json:"connectors"`
}

func (a *App) mcpListConnectors(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, mcpConnectorsOut, error) {
	return nil, mcpConnectorsOut{Connectors: connectorSpecs}, nil
}

type mcpAddSourceArgs struct {
	Name      string            `json:"name" jsonschema:"the logical name to register (used in FROM)"`
	Connector string            `json:"connector" jsonschema:"a connector name from list_connectors"`
	Fields    map[string]string `json:"fields,omitempty" jsonschema:"connector fields (see list_connectors); sensitive values must be ${ENV_VAR} references"`
	Save      bool              `json:"save,omitempty" jsonschema:"also persist the (secret-free) source to the config file"`
}

type mcpAddSourceOut struct {
	Registered []string `json:"registered" jsonschema:"the logical names registered (several for a wildcard)"`
	Saved      string   `json:"saved,omitempty" jsonschema:"the config file the source was appended to"`
	SaveError  string   `json:"save_error,omitempty" jsonschema:"set when registration succeeded but persisting to the config failed"`
}

// mcpAddSource is the MCP twin of the web addSource handler: identical
// validation (secrets as ${ENV_VAR}), interpolation, wildcard expansion, and
// optional config persistence via registerRuntimeSource/AppendSource. The
// plugin connector is rejected here exactly as the web UI omits it: its
// command field is arbitrary exec, and "add a data source" must not quietly
// escalate to "run a program". Plugin sources declared in turntable.yaml load
// normally.
func (a *App) mcpAddSource(ctx context.Context, req *mcp.CallToolRequest, in mcpAddSourceArgs) (*mcp.CallToolResult, mcpAddSourceOut, error) {
	var out mcpAddSourceOut
	name := strings.TrimSpace(in.Name)
	if name == "" || strings.TrimSpace(in.Connector) == "" {
		return nil, out, errors.New("name and connector are required")
	}
	if in.Connector == "plugin" {
		return nil, out, errors.New("the plugin connector runs an external command and cannot be added at runtime — declare it in turntable.yaml instead")
	}
	if connectorSpecFor(in.Connector) == nil {
		return nil, out, fmt.Errorf("unknown connector %q — list_connectors shows what is available", in.Connector)
	}
	src := config.Source{Connector: in.Connector}
	for k, v := range in.Fields {
		applySourceField(&src, k, v)
	}
	a.sessionMu.Lock()
	names, err := a.registerRuntimeSource(ctx, name, src)
	a.sessionMu.Unlock()
	if err != nil {
		return nil, out, err
	}
	out.Registered = names
	if in.Save {
		if err := config.AppendSource(a.configPath, name, src); err != nil {
			out.SaveError = err.Error()
		} else {
			out.Saved = a.configPath
		}
	}
	return nil, out, nil
}

type mcpRemoveSourceArgs struct {
	Name string `json:"name" jsonschema:"the registered source name to remove"`
}

type mcpRemoveSourceOut struct {
	Removed string `json:"removed"`
}

func (a *App) mcpRemoveSource(ctx context.Context, req *mcp.CallToolRequest, in mcpRemoveSourceArgs) (*mcp.CallToolResult, mcpRemoveSourceOut, error) {
	var out mcpRemoveSourceOut
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if _, isMat := a.matViews[in.Name]; isMat {
		return nil, out, fmt.Errorf("%q is a materialized view — use DROP MATERIALIZED VIEW %s", in.Name, in.Name)
	}
	if !a.Reg.RemoveSource(in.Name) {
		return nil, out, fmt.Errorf("unknown source %q — list_sources shows what is registered", in.Name)
	}
	return nil, mcpRemoveSourceOut{Removed: in.Name}, nil
}

// ---- dashboards --------------------------------------------------------------

type mcpDashboardsOut struct {
	Dashboards []dashboardSummary `json:"dashboards"`
}

func (a *App) mcpListDashboards(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, mcpDashboardsOut, error) {
	var out mcpDashboardsOut
	list, err := a.listDashboards()
	if err != nil {
		return nil, out, err
	}
	return nil, mcpDashboardsOut{Dashboards: list}, nil
}

type mcpDashSlugArgs struct {
	Slug string `json:"slug" jsonschema:"the dashboard slug (see list_dashboards)"`
}

type mcpGetDashboardOut struct {
	Slug string `json:"slug"`
	Dashboard
}

func (a *App) mcpGetDashboard(ctx context.Context, req *mcp.CallToolRequest, in mcpDashSlugArgs) (*mcp.CallToolResult, mcpGetDashboardOut, error) {
	var out mcpGetDashboardOut
	if !slugRe.MatchString(in.Slug) {
		return nil, out, fmt.Errorf("bad dashboard slug %q", in.Slug)
	}
	d, err := a.loadDashboard(in.Slug)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, out, fmt.Errorf("unknown dashboard %q — list_dashboards shows what exists", in.Slug)
		}
		return nil, out, err
	}
	return nil, mcpGetDashboardOut{Slug: in.Slug, Dashboard: *d}, nil
}

type mcpSaveDashboardArgs struct {
	Slug string `json:"slug,omitempty" jsonschema:"update this dashboard; empty derives the slug from the name (a create, or an overwrite of the same-named dashboard)"`
	Dashboard
}

type mcpSaveDashboardOut struct {
	Slug  string `json:"slug"`
	Saved string `json:"saved" jsonschema:"the YAML file the definition was written to"`
}

func (a *App) mcpSaveDashboard(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveDashboardArgs) (*mcp.CallToolResult, mcpSaveDashboardOut, error) {
	var out mcpSaveDashboardOut
	a.sessionMu.Lock()
	slug, err := a.saveDashboardChecked(in.Slug, &in.Dashboard)
	a.sessionMu.Unlock()
	if err != nil {
		return nil, out, err
	}
	return nil, mcpSaveDashboardOut{Slug: slug, Saved: filepath.ToSlash(a.dashPath(slug))}, nil
}

type mcpDeleteDashboardOut struct {
	Deleted string `json:"deleted"`
}

func (a *App) mcpDeleteDashboard(ctx context.Context, req *mcp.CallToolRequest, in mcpDashSlugArgs) (*mcp.CallToolResult, mcpDeleteDashboardOut, error) {
	var out mcpDeleteDashboardOut
	if !slugRe.MatchString(in.Slug) {
		return nil, out, fmt.Errorf("bad dashboard slug %q", in.Slug)
	}
	a.sessionMu.Lock()
	err := os.Remove(a.dashPath(in.Slug))
	a.sessionMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, out, fmt.Errorf("unknown dashboard %q — list_dashboards shows what exists", in.Slug)
		}
		return nil, out, err
	}
	return nil, mcpDeleteDashboardOut{Deleted: in.Slug}, nil
}

type mcpRenderDashboardArgs struct {
	Slug      string            `json:"slug" jsonschema:"the dashboard slug to render"`
	Out       string            `json:"out,omitempty" jsonschema:"output HTML path (default <slug>.html in the working directory)"`
	Variables map[string]string `json:"variables,omitempty" jsonschema:"{{var}} overrides; unset variables use their declared defaults"`
}

type mcpRenderDashboardOut struct {
	Path string `json:"path" jsonschema:"absolute path of the written HTML report"`
}

func (a *App) mcpRenderDashboard(ctx context.Context, req *mcp.CallToolRequest, in mcpRenderDashboardArgs) (*mcp.CallToolResult, mcpRenderDashboardOut, error) {
	var out mcpRenderDashboardOut
	if !slugRe.MatchString(in.Slug) {
		return nil, out, fmt.Errorf("bad dashboard slug %q", in.Slug)
	}
	outPath := in.Out
	if outPath == "" {
		outPath = in.Slug + ".html"
	}
	if err := a.renderDashboard(ctx, in.Slug, outPath, in.Variables); err != nil {
		return nil, out, err
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		abs = outPath
	}
	return nil, mcpRenderDashboardOut{Path: abs}, nil
}

type mcpFunctionsOut struct {
	Scalar    []string `json:"scalar"`
	Aggregate []string `json:"aggregate"`
	Keywords  []string `json:"keywords"`
}

func (a *App) mcpListFunctions(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, mcpFunctionsOut, error) {
	return nil, mcpFunctionsOut{
		Scalar:    a.Funcs.Names(),
		Aggregate: engine.Aggregates(),
		Keywords:  sqlKeywords,
	}, nil
}
