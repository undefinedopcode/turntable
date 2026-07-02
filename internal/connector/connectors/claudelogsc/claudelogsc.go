// Package claudelogsc is the Claude Code transcripts connector. It reads the
// JSONL session logs Claude Code writes under ~/.claude/projects/<slug>/<id>.jsonl
// and exposes the conversation messages (user/assistant/system events) as
// flattened, typed rows. Bookkeeping events (titles, mode changes, snapshots,
// etc.) are skipped.
//
// Three views are available via the `kind` option:
//
//	messages (default)  one row per conversation message (text + tool_uses count,
//	                    plus the assistant messages' model, token usage, and an
//	                    estimated API cost_usd at list prices — so per-model API
//	                    spend is a GROUP BY model away)
//	tools               one row per tool_use block (tool_name, tool_id, the input
//	                    re-encoded as JSON), so a message that calls N tools is N
//	                    rows. Query tool usage, or grep tool inputs as strings.
//	tool_results        one row per tool_result block (tool_use_id, is_error,
//	                    text). Join tool_use_id back to a tools-view tool_id to
//	                    pair each call with its output.
//
// Options (path wins over project wins over the default):
//
//	kind     "messages" (default), "tools", or "tool_results".
//	path     a .jsonl session file, or a directory of them.
//	project  a project slug (e.g. "-home-april-projects-foo") or a working-dir
//	         path (slugified) -> ~/.claude/projects/<slug>.
//	base     override the projects root (default ~/.claude/projects); for tests.
//
// With no path/project set, it defaults to the current working directory's
// project — the transcripts of the project turntable is running in.
package claudelogsc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// maxLine bounds a single JSONL line (transcripts can have very long lines).
const maxLine = 64 * 1024 * 1024

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "claudelogs" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve returns the schema for the dataset's kind (messages or tools).
func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return schemaFor(kindOf(ds))
}

// messageColumns: one row per conversation message. The token columns come
// from the API usage block on assistant messages (NULL elsewhere); cost_usd is
// computed from list prices — see costUSD for the estimate's caveats. Per-model
// API spend is one GROUP BY away:
//
//	SELECT model, ROUND(SUM(cost_usd), 2) AS usd, SUM(output_tokens) AS out_tok
//	FROM transcripts GROUP BY model ORDER BY usd DESC
var messageColumns = []engine.Column{
	{Name: "session_id", Type: engine.TypeString, Nullable: true},
	{Name: "session_file", Type: engine.TypeString, Nullable: true},
	{Name: "uuid", Type: engine.TypeString, Nullable: true},
	{Name: "parent_uuid", Type: engine.TypeString, Nullable: true},
	{Name: "type", Type: engine.TypeString, Nullable: true},
	{Name: "role", Type: engine.TypeString, Nullable: true},
	{Name: "model", Type: engine.TypeString, Nullable: true},
	{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
	{Name: "text", Type: engine.TypeString, Nullable: true},
	{Name: "tool_uses", Type: engine.TypeInt, Nullable: true},
	{Name: "input_tokens", Type: engine.TypeInt, Nullable: true},
	{Name: "output_tokens", Type: engine.TypeInt, Nullable: true},
	{Name: "cache_write_tokens", Type: engine.TypeInt, Nullable: true},
	{Name: "cache_read_tokens", Type: engine.TypeInt, Nullable: true},
	{Name: "cost_usd", Type: engine.TypeFloat, Nullable: true},
	{Name: "cwd", Type: engine.TypeString, Nullable: true},
	{Name: "git_branch", Type: engine.TypeString, Nullable: true},
}

// toolColumns: one row per tool_use block (a message may have several).
var toolColumns = []engine.Column{
	{Name: "session_id", Type: engine.TypeString, Nullable: true},
	{Name: "session_file", Type: engine.TypeString, Nullable: true},
	{Name: "message_uuid", Type: engine.TypeString, Nullable: true},
	{Name: "parent_uuid", Type: engine.TypeString, Nullable: true},
	{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
	{Name: "tool_name", Type: engine.TypeString, Nullable: true},
	{Name: "tool_id", Type: engine.TypeString, Nullable: true},
	{Name: "tool_input", Type: engine.TypeString, Nullable: true}, // JSON-encoded input
}

// toolResultColumns: one row per tool_result block. Join tool_use_id back to a
// tools-view tool_id to pair each call with its output.
var toolResultColumns = []engine.Column{
	{Name: "session_id", Type: engine.TypeString, Nullable: true},
	{Name: "session_file", Type: engine.TypeString, Nullable: true},
	{Name: "message_uuid", Type: engine.TypeString, Nullable: true},
	{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
	{Name: "tool_use_id", Type: engine.TypeString, Nullable: true},
	{Name: "is_error", Type: engine.TypeBool, Nullable: true},
	{Name: "text", Type: engine.TypeString, Nullable: true},
}

// kindOf returns the requested view: "messages" (default) or "tools".
func kindOf(ds connector.Dataset) string {
	k := strings.ToLower(strings.TrimSpace(stringOpt(ds.Options, "kind")))
	if k == "" {
		return "messages"
	}
	return k
}

func schemaFor(kind string) (engine.Schema, error) {
	switch kind {
	case "messages":
		return engine.Schema{Columns: messageColumns}, nil
	case "tools":
		return engine.Schema{Columns: toolColumns}, nil
	case "tool_results":
		return engine.Schema{Columns: toolResultColumns}, nil
	default:
		return engine.Schema{}, fmt.Errorf("claudelogs: unknown kind %q (messages|tools|tool_results)", kind)
	}
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	kind := kindOf(req.Dataset)
	if _, err := schemaFor(kind); err != nil {
		return nil, err
	}
	files, err := resolveFiles(req.Dataset)
	if err != nil {
		return nil, err
	}
	return &logIter{files: files, kind: kind}, nil
}

// ---- file resolution ---------------------------------------------------------

func resolveFiles(ds connector.Dataset) ([]string, error) {
	path := stringOpt(ds.Options, "path")
	if path == "" {
		path = ds.Source
	}
	if path != "" {
		return expandPath(path)
	}

	base := stringOpt(ds.Options, "base")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home: %w", err)
		}
		base = filepath.Join(home, ".claude", "projects")
	}
	slug := stringOpt(ds.Options, "project")
	if slug == "" {
		// Default to the current working directory's project.
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		slug = slugify(cwd)
	} else if strings.Contains(slug, "/") {
		slug = slugify(slug)
	}
	return listJSONL(filepath.Join(base, slug))
}

// slugify maps a working-directory path to a Claude Code project slug
// ("/home/april/projects/foo" -> "-home-april-projects-foo").
func slugify(dir string) string { return strings.ReplaceAll(dir, "/", "-") }

// expandPath resolves a ~-prefixed file or directory into the list of .jsonl
// files it denotes.
func expandPath(p string) ([]string, error) {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("claudelogs path %q: %w", p, err)
	}
	if info.IsDir() {
		return listJSONL(p)
	}
	return []string{p}, nil
}

// listJSONL returns the .jsonl files directly under dir, sorted for determinism.
func listJSONL(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read transcripts dir %q: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- iterator ----------------------------------------------------------------

type logIter struct {
	files   []string
	fi      int
	f       *os.File
	scanner *bufio.Scanner
	kind    string
	pending []engine.Row // rows produced from the current line not yet returned
	closed  bool
}

// messageTypes are the event types we surface as rows; everything else (titles,
// mode changes, snapshots, …) is skipped.
var messageTypes = map[string]bool{"user": true, "assistant": true, "system": true}

type event struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	GitBranch  string `json:"gitBranch"`
	Message    struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *usage          `json:"usage"` // present on assistant messages
	} `json:"message"`
}

// usage is the API token accounting attached to assistant messages. The
// cache_creation breakdown (per-TTL write tokens) appears in newer transcripts;
// the flat cache_creation_input_tokens total is always present alongside it.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (it *logIter) Next() (engine.Row, bool, error) {
	for {
		// Drain rows already produced from the current line (a tools-view
		// message can yield several).
		if len(it.pending) > 0 {
			r := it.pending[0]
			it.pending = it.pending[1:]
			return r, true, nil
		}
		if it.scanner == nil {
			if it.fi >= len(it.files) {
				return engine.Row{}, false, nil
			}
			f, err := os.Open(it.files[it.fi])
			if err != nil {
				return engine.Row{}, false, fmt.Errorf("open %q: %w", it.files[it.fi], err)
			}
			it.f = f
			it.scanner = bufio.NewScanner(f)
			it.scanner.Buffer(make([]byte, 64*1024), maxLine)
		}
		if !it.scanner.Scan() {
			if err := it.scanner.Err(); err != nil {
				return engine.Row{}, false, err
			}
			it.f.Close()
			it.f, it.scanner = nil, nil
			it.fi++
			continue
		}
		line := it.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Tolerate the occasional malformed/unknown line rather than abort.
			continue
		}
		if !messageTypes[ev.Type] {
			continue
		}
		it.pending = it.rowsFor(ev) // may be empty (e.g. a message with no tools)
	}
}

// rowsFor turns one message event into the rows for the iterator's kind: a
// single message row, or one row per tool_use block.
func (it *logIter) rowsFor(ev event) []engine.Row {
	file := strings.TrimSuffix(filepath.Base(it.files[it.fi]), ".jsonl")
	if it.kind == "tools" {
		calls := extractToolCalls(ev.Message.Content)
		rows := make([]engine.Row, len(calls))
		for i, c := range calls {
			rows[i] = engine.Row{Values: []engine.Value{
				strOrNull(ev.SessionID),
				strOrNull(file),
				strOrNull(ev.UUID),
				strOrNull(ev.ParentUUID),
				timeVal(ev.Timestamp),
				strOrNull(c.name),
				strOrNull(c.id),
				strOrNull(c.input),
			}}
		}
		return rows
	}
	if it.kind == "tool_results" {
		results := extractToolResults(ev.Message.Content)
		rows := make([]engine.Row, len(results))
		for i, r := range results {
			rows[i] = engine.Row{Values: []engine.Value{
				strOrNull(ev.SessionID),
				strOrNull(file),
				strOrNull(ev.UUID),
				timeVal(ev.Timestamp),
				strOrNull(r.useID),
				engine.BoolVal(r.isError),
				strOrNull(r.text),
			}}
		}
		return rows
	}
	text, tools := extractText(ev.Message.Content)
	u := ev.Message.Usage
	return []engine.Row{{Values: []engine.Value{
		strOrNull(ev.SessionID),
		strOrNull(file),
		strOrNull(ev.UUID),
		strOrNull(ev.ParentUUID),
		strOrNull(ev.Type),
		strOrNull(ev.Message.Role),
		strOrNull(ev.Message.Model),
		timeVal(ev.Timestamp),
		strOrNull(text),
		engine.IntVal(int64(tools)),
		usageVal(u, func(u *usage) int64 { return u.InputTokens }),
		usageVal(u, func(u *usage) int64 { return u.OutputTokens }),
		usageVal(u, func(u *usage) int64 { return u.CacheCreationInputTokens }),
		usageVal(u, func(u *usage) int64 { return u.CacheReadInputTokens }),
		costUSD(ev.Message.Model, ev.Timestamp, u),
		strOrNull(ev.Cwd),
		strOrNull(ev.GitBranch),
	}}}
}

func usageVal(u *usage, get func(*usage) int64) engine.Value {
	if u == nil {
		return engine.Null()
	}
	return engine.IntVal(get(u))
}

func (it *logIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	if it.f != nil {
		return it.f.Close()
	}
	return nil
}

// ---- cost estimation -----------------------------------------------------------

// modelPrice is USD per million tokens at list price.
type modelPrice struct{ in, out float64 }

// modelPrices maps model-ID prefixes to list prices. Transcript model IDs may
// carry date suffixes (claude-opus-4-5-20251101), so entries are prefixes,
// matched in order — keep more specific prefixes before shorter ones.
// Prices as of 2026-07 (source: Anthropic pricing docs); update as they change.
var modelPrices = []struct {
	prefix string
	price  modelPrice
}{
	{"claude-fable-5", modelPrice{10, 50}},
	{"claude-mythos", modelPrice{10, 50}},
	{"claude-opus-4-8", modelPrice{5, 25}},
	{"claude-opus-4-7", modelPrice{5, 25}},
	{"claude-opus-4-6", modelPrice{5, 25}},
	{"claude-opus-4-5", modelPrice{5, 25}},
	{"claude-opus-4", modelPrice{15, 75}}, // opus 4.0 / 4.1 (incl. dated ids)
	{"claude-3-opus", modelPrice{15, 75}},
	{"claude-sonnet-5", modelPrice{3, 15}}, // intro pricing handled in priceFor
	{"claude-sonnet-4", modelPrice{3, 15}}, // sonnet 4.6 / 4.5 / 4.0
	{"claude-3-7-sonnet", modelPrice{3, 15}},
	{"claude-3-5-sonnet", modelPrice{3, 15}},
	{"claude-haiku-4-5", modelPrice{1, 5}},
	{"claude-3-5-haiku", modelPrice{0.8, 4}},
	{"claude-3-haiku", modelPrice{0.25, 1.25}},
}

// sonnet5IntroEnd is the end of Claude Sonnet 5's introductory pricing
// ($2/$10 per MTok through 2026-08-31).
var sonnet5IntroEnd = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func priceFor(model string, ts time.Time) (modelPrice, bool) {
	for _, e := range modelPrices {
		if strings.HasPrefix(model, e.prefix) {
			if e.prefix == "claude-sonnet-5" && !ts.IsZero() && ts.Before(sonnet5IntroEnd) {
				return modelPrice{2, 10}, true
			}
			return e.price, true
		}
	}
	return modelPrice{}, false
}

// costUSD estimates one message's API cost at list prices: input and output at
// the model's per-MTok rate, cache writes at 1.25× input (5-minute TTL) or 2×
// (1-hour TTL — the per-TTL breakdown is used when the transcript carries it,
// else all writes are assumed 5-minute), cache reads at 0.1× input. NULL when
// the message has no usage or the model is unknown/synthetic.
//
// It is an ESTIMATE of API list-price value, not a bill: subscription plans
// don't meter per token, batch/priority tiers price differently, and prices
// change over time (the table above is a snapshot).
func costUSD(model, timestamp string, u *usage) engine.Value {
	if u == nil || model == "" {
		return engine.Null()
	}
	var ts time.Time
	if t, err := time.Parse(time.RFC3339, timestamp); err == nil {
		ts = t
	}
	p, ok := priceFor(model, ts)
	if !ok {
		return engine.Null()
	}
	write5m, write1h := u.CacheCreationInputTokens, int64(0)
	if u.CacheCreation != nil {
		write5m, write1h = u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
	}
	cost := (float64(u.InputTokens)*p.in +
		float64(u.OutputTokens)*p.out +
		float64(write5m)*p.in*1.25 +
		float64(write1h)*p.in*2 +
		float64(u.CacheReadInputTokens)*p.in*0.1) / 1e6
	return engine.FloatVal(cost)
}

// ---- helpers -----------------------------------------------------------------

// extractText pulls human-readable text from a message's content (a JSON string
// or an array of content blocks) and counts tool_use blocks. tool_result blocks
// contribute their nested text so tool output is searchable.
func extractText(raw json.RawMessage) (string, int) {
	if len(raw) == 0 {
		return "", 0
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", 0
	}
	return walkContent(v)
}

func walkContent(v any) (string, int) {
	switch c := v.(type) {
	case string:
		return c, 0
	case []any:
		var b strings.Builder
		tools := 0
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if s, ok := m["text"].(string); ok {
					appendLine(&b, s)
				}
			case "tool_use":
				tools++
			case "tool_result":
				if t, _ := walkContent(m["content"]); t != "" {
					appendLine(&b, t)
				}
			}
		}
		return b.String(), tools
	}
	return "", 0
}

type toolCall struct {
	name  string
	id    string
	input string // the tool input object, re-encoded as JSON
}

// extractToolCalls returns the tool_use blocks in a message's content, with the
// input object re-encoded as JSON so it is queryable as a string (LIKE/REGEXP).
func extractToolCalls(raw json.RawMessage) []toolCall {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []toolCall
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "tool_use" {
			continue
		}
		c := toolCall{}
		c.name, _ = m["name"].(string)
		c.id, _ = m["id"].(string)
		if inp, ok := m["input"]; ok && inp != nil {
			if b, err := json.Marshal(inp); err == nil {
				c.input = string(b)
			}
		}
		out = append(out, c)
	}
	return out
}

type toolResult struct {
	useID   string
	isError bool
	text    string
}

// extractToolResults returns the tool_result blocks in a message's content
// (these appear in the user message following a tool call). is_error defaults to
// false when absent or null.
func extractToolResults(raw json.RawMessage) []toolResult {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []toolResult
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "tool_result" {
			continue
		}
		r := toolResult{}
		r.useID, _ = m["tool_use_id"].(string)
		if e, ok := m["is_error"].(bool); ok {
			r.isError = e
		}
		r.text, _ = walkContent(m["content"])
		out = append(out, r)
	}
	return out
}

func appendLine(b *strings.Builder, s string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}

func strOrNull(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	return engine.StringVal(s)
}

func timeVal(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return engine.TimeVal(t)
	}
	return engine.Null()
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
