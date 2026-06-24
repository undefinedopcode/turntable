// Package claudelogsc is the Claude Code transcripts connector. It reads the
// JSONL session logs Claude Code writes under ~/.claude/projects/<slug>/<id>.jsonl
// and exposes the conversation messages (user/assistant/system events) as
// flattened, typed rows. Bookkeeping events (titles, mode changes, snapshots,
// etc.) are skipped.
//
// Two views are available via the `kind` option:
//
//	messages (default)  one row per conversation message (text + tool_uses count)
//	tools               one row per tool_use block (tool_name, tool_id, the input
//	                    re-encoded as JSON), so a message that calls N tools is N
//	                    rows. Query tool usage, or grep tool inputs as strings.
//
// Options (path wins over project wins over the default):
//
//	kind     "messages" (default) or "tools".
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

// messageColumns: one row per conversation message.
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
	default:
		return engine.Schema{}, fmt.Errorf("claudelogs: unknown kind %q (messages|tools)", kind)
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
	} `json:"message"`
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
	text, tools := extractText(ev.Message.Content)
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
		strOrNull(ev.Cwd),
		strOrNull(ev.GitBranch),
	}}}
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
