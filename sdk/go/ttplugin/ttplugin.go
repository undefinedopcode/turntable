// Package ttplugin is the Go SDK for writing turntable plugin connectors.
//
// A plugin is a standalone program that turntable launches as a subprocess and
// drives over stdio with JSON-RPC 2.0 (see PLUGINS.md for the wire protocol).
// This package implements all of the protocol plumbing — message framing,
// dispatch, scan cursors, predicate evaluation, and cell encoding — so an author
// only declares datasets and a function that produces rows:
//
//	func main() {
//		ttplugin.Serve(ttplugin.Plugin{
//			Name: "sysinfo",
//			Datasets: map[string]ttplugin.Dataset{
//				"env": {
//					Schema: ttplugin.Schema{Columns: []ttplugin.Column{
//						{Name: "name", Type: "string"},
//						{Name: "value", Type: "string", Nullable: true},
//					}},
//					Rows: func(ttplugin.Request) (ttplugin.Rows, error) {
//						var rows ttplugin.Rows
//						for _, kv := range os.Environ() {
//							k, v, _ := strings.Cut(kv, "=")
//							rows = append(rows, ttplugin.Row{k, v})
//						}
//						return rows, nil
//					},
//				},
//			},
//		})
//	}
//
// By default the SDK applies the pushed-down WHERE and LIMIT to the rows you
// return, so a plugin gets predicate/limit pushdown for free. Set
// ManualPushdown to take over (e.g. to push filters into a remote backend).
package ttplugin

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// protocolVersion is the wire protocol version this SDK implements.
const protocolVersion = 1

// Column describes one column. Type is one of: int, float, string, bool, time,
// duration, bytes, any.
type Column struct {
	Name     string
	Type     string
	Nullable bool
}

// Schema is an ordered set of columns.
type Schema struct {
	Columns []Column
}

// Row is one row: a positional list of cells aligned to a Schema. Each cell is a
// plain Go value matching the column type — int/int64, float64, string, bool,
// time.Time, time.Duration, []byte, nil (NULL), or any JSON-encodable value for
// an "any" column. The SDK encodes them to the wire form.
type Row = []any

// Rows is a batch of rows.
type Rows = []Row

// Request carries the pushed-down scan hints to a dataset's Rows function. In
// automatic mode you may ignore Predicate/Limit (the SDK applies them); they are
// provided so manual-pushdown plugins can honor them at the source.
type Request struct {
	Dataset   string
	Columns   []string // projection hint (advisory)
	Limit     *int
	Predicate *Predicate
	Options   map[string]any
}

// Dataset is one queryable relation: its schema and a function producing rows.
type Dataset struct {
	Schema Schema
	Rows   func(Request) (Rows, error)
}

// Plugin is a whole connector: a name and its datasets.
type Plugin struct {
	// Name is the connector's advertised name (and qualified-ref prefix).
	Name string
	// Datasets maps dataset name to its definition.
	Datasets map[string]Dataset
	// ManualPushdown disables the SDK's automatic predicate/limit filtering.
	// Set it when your Rows function already applies the Request's Predicate and
	// Limit (e.g. pushing them to a database); then report nothing extra — the
	// engine still re-applies the residual, so partial application is safe.
	ManualPushdown bool
}

// Serve runs the plugin over the process's stdin/stdout until shutdown or EOF.
// It is the normal entry point from main.
func Serve(p Plugin) error {
	err := ServeRW(p, os.Stdin, os.Stdout)
	if err == io.EOF {
		return nil
	}
	return err
}

// ServeRW runs the plugin over arbitrary streams. Exposed for testing.
func ServeRW(p Plugin, in io.Reader, out io.Writer) error {
	s := &server{plugin: p, scans: map[string]*cursor{}}
	r := bufio.NewReader(in)
	for {
		payload, err := readMessage(r)
		if err != nil {
			return err
		}
		var req struct {
			ID     *uint64         `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}
		if req.Method == "shutdown" {
			return nil
		}
		if req.ID == nil {
			continue // unknown notification
		}
		result, rerr := s.dispatch(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		if rerr != nil {
			resp["error"] = map[string]any{"code": -32000, "message": rerr.Error()}
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		if err := writeMessage(out, b); err != nil {
			return err
		}
	}
}

type server struct {
	plugin Plugin
	scans  map[string]*cursor
	nextID int
}

type cursor struct {
	rows   Rows
	schema Schema
	pos    int
}

func (s *server) dispatch(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"name":            s.plugin.Name,
			"capabilities": map[string]bool{
				// The SDK implements predicate + limit pushdown itself (or the
				// author does, in manual mode), so advertise them either way.
				"predicatePushdown": true,
				"limitPushdown":     true,
			},
			"datasets": s.datasetList(),
		}, nil
	case "datasets":
		return map[string]any{"datasets": s.datasetList()}, nil
	case "resolve":
		var p struct {
			Dataset struct{ Name string } `json:"dataset"`
		}
		_ = json.Unmarshal(params, &p)
		ds, ok := s.plugin.Datasets[p.Dataset.Name]
		if !ok {
			return nil, fmt.Errorf("unknown dataset %q", p.Dataset.Name)
		}
		return map[string]any{"schema": wireSchema(ds.Schema)}, nil
	case "scan":
		return s.scan(params)
	case "next":
		return s.next(params)
	case "close":
		var p struct {
			ScanID string `json:"scanId"`
		}
		_ = json.Unmarshal(params, &p)
		delete(s.scans, p.ScanID)
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

func (s *server) datasetList() []map[string]string {
	out := make([]map[string]string, 0, len(s.plugin.Datasets))
	for name := range s.plugin.Datasets {
		out = append(out, map[string]string{"name": name})
	}
	return out
}

func (s *server) scan(params json.RawMessage) (any, error) {
	var p struct {
		Dataset   struct{ Name string } `json:"dataset"`
		Columns   []string              `json:"columns"`
		Limit     *int                  `json:"limit"`
		Predicate *Predicate            `json:"predicate"`
		Options   map[string]any        `json:"options"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	ds, ok := s.plugin.Datasets[p.Dataset.Name]
	if !ok {
		return nil, fmt.Errorf("unknown dataset %q", p.Dataset.Name)
	}
	if ds.Rows == nil {
		return nil, fmt.Errorf("dataset %q has no Rows function", p.Dataset.Name)
	}

	rows, err := ds.Rows(Request{
		Dataset:   p.Dataset.Name,
		Columns:   p.Columns,
		Limit:     p.Limit,
		Predicate: p.Predicate,
		Options:   p.Options,
	})
	if err != nil {
		return nil, err
	}

	applied := map[string]bool{}
	if !s.plugin.ManualPushdown {
		if p.Predicate != nil {
			rows = filterRows(rows, ds.Schema, p.Predicate)
			applied["predicate"] = true
		}
		// Limit is safe to apply only because we fully evaluated the predicate.
		if p.Limit != nil && *p.Limit < len(rows) {
			rows = rows[:*p.Limit]
			applied["limit"] = true
		}
	}

	s.nextID++
	id := strconv.Itoa(s.nextID)
	s.scans[id] = &cursor{rows: rows, schema: ds.Schema}
	return map[string]any{"scanId": id, "schema": wireSchema(ds.Schema), "applied": applied}, nil
}

func (s *server) next(params json.RawMessage) (any, error) {
	var p struct {
		ScanID  string `json:"scanId"`
		MaxRows int    `json:"maxRows"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cur, ok := s.scans[p.ScanID]
	if !ok {
		return nil, fmt.Errorf("unknown scanId %q", p.ScanID)
	}
	max := p.MaxRows
	if max <= 0 {
		max = 1000
	}
	end := cur.pos + max
	if end > len(cur.rows) {
		end = len(cur.rows)
	}
	batch := cur.rows[cur.pos:end]
	cur.pos = end

	out := make([][]any, len(batch))
	for i, row := range batch {
		out[i] = encodeRow(row, cur.schema)
	}
	return map[string]any{"rows": out, "done": cur.pos >= len(cur.rows)}, nil
}

// filterRows keeps only rows satisfying the predicate, evaluating it against
// each row by column name.
func filterRows(rows Rows, sc Schema, pred *Predicate) Rows {
	idx := make(map[string]int, len(sc.Columns))
	for i, c := range sc.Columns {
		idx[c.Name] = i
	}
	get := func(row Row) func(string) any {
		return func(col string) any {
			if i, ok := idx[col]; ok && i < len(row) {
				return row[i]
			}
			return nil
		}
	}
	out := rows[:0:0]
	for _, row := range rows {
		if pred.Eval(get(row)) {
			out = append(out, row)
		}
	}
	return out
}

// encodeRow converts a row of Go values into wire cells, applying the per-type
// string encodings (RFC3339 time, base64 bytes, Go-duration strings).
func encodeRow(row Row, sc Schema) []any {
	out := make([]any, len(sc.Columns))
	for i := range sc.Columns {
		if i >= len(row) {
			out[i] = nil
			continue
		}
		out[i] = encodeCell(row[i], sc.Columns[i].Type)
	}
	return out
}

func encodeCell(v any, typ string) any {
	if v == nil {
		return nil
	}
	switch typ {
	case "time":
		switch t := v.(type) {
		case time.Time:
			return t.UTC().Format(time.RFC3339Nano)
		case string:
			return t
		}
	case "duration":
		switch d := v.(type) {
		case time.Duration:
			return d.String()
		}
	case "bytes":
		if b, ok := v.([]byte); ok {
			return base64.StdEncoding.EncodeToString(b)
		}
	}
	return v
}

func wireSchema(sc Schema) map[string]any {
	cols := make([]map[string]any, len(sc.Columns))
	for i, c := range sc.Columns {
		typ := c.Type
		if typ == "" {
			typ = "any"
		}
		cols[i] = map[string]any{"name": c.Name, "type": typ, "nullable": c.Nullable}
	}
	return map[string]any{"columns": cols}
}

// ---- message framing (Content-Length, LSP-style) ----------------------------

func writeMessage(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, "Content-Length: "+strconv.Itoa(len(payload))+"\r\n\r\n"); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, val, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, _ = strconv.Atoi(strings.TrimSpace(val))
		}
	}
	if n < 0 {
		return nil, fmt.Errorf("message missing Content-Length header")
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}
