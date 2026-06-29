// Command sysinfo is a reference turntable plugin connector. It is a standalone
// program — it imports nothing from turntable — that speaks the stdio JSON-RPC
// protocol documented in PLUGINS.md, exposing live system state as queryable
// relations:
//
//	env       one row per environment variable (name, value)
//	runtime   a single row of live Go-runtime stats (recomputed every scan)
//
// Run it through turntable with a config source:
//
//	sources:
//	  sys:
//	    connector: plugin
//	    command: ["go", "run", "./examples/plugins/sysinfo"]
//	    options: {dataset: "*"}   # expose every dataset the plugin advertises
//
// then query it like any other source:  SELECT * FROM env WHERE name = 'PATH'
//
// It demonstrates predicate and limit pushdown: when turntable sends a WHERE it
// can express, the plugin filters at the source and reports it via the scan's
// `applied` flags (turntable re-applies anything the plugin leaves, so partial
// support is always safe).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const protocolVersion = 1

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "sysinfo:", err)
		os.Exit(1)
	}
}

// ---- JSON-RPC plumbing -------------------------------------------------------

type request struct {
	ID     *uint64         `json:"id"` // nil for notifications
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      uint64  `json:"id"`
	Result  any     `json:"result,omitempty"`
	Error   *rpcErr `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serve(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	srv := &server{scans: map[string]*cursor{}}
	for {
		payload, err := readMessage(r)
		if err != nil {
			return err
		}
		var req request
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}
		if req.Method == "shutdown" {
			return nil
		}
		if req.ID == nil {
			continue // unknown notification
		}
		result, rerr := srv.dispatch(req.Method, req.Params)
		resp := response{JSONRPC: "2.0", ID: *req.ID}
		if rerr != nil {
			resp.Error = &rpcErr{Code: -32000, Message: rerr.Error()}
		} else {
			resp.Result = result
		}
		b, _ := json.Marshal(resp)
		if err := writeMessage(out, b); err != nil {
			return err
		}
	}
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
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func writeMessage(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ---- the connector -----------------------------------------------------------

type column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

type schema struct {
	Columns []column `json:"columns"`
}

type dataset struct {
	Name string `json:"name"`
}

// schemas defines the two datasets this plugin serves.
var schemas = map[string]schema{
	"env": {Columns: []column{
		{Name: "name", Type: "string"},
		{Name: "value", Type: "string", Nullable: true},
	}},
	"runtime": {Columns: []column{
		{Name: "goroutines", Type: "int"},
		{Name: "cpus", Type: "int"},
		{Name: "alloc_bytes", Type: "int"},
		{Name: "heap_objects", Type: "int"},
		{Name: "now", Type: "time"},
	}},
}

type server struct {
	scans  map[string]*cursor
	nextID int
}

type cursor struct {
	rows [][]any
	pos  int
}

func (s *server) dispatch(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"name":            "sysinfo",
			"capabilities": map[string]bool{
				"predicatePushdown": true,
				"limitPushdown":     true,
			},
			"datasets": []dataset{{Name: "env"}, {Name: "runtime"}},
		}, nil
	case "datasets":
		return map[string]any{"datasets": []dataset{{Name: "env"}, {Name: "runtime"}}}, nil
	case "resolve":
		var p struct {
			Dataset dataset `json:"dataset"`
		}
		_ = json.Unmarshal(params, &p)
		sc, ok := schemas[p.Dataset.Name]
		if !ok {
			return nil, fmt.Errorf("unknown dataset %q", p.Dataset.Name)
		}
		return map[string]any{"schema": sc}, nil
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

func (s *server) scan(params json.RawMessage) (any, error) {
	var p struct {
		Dataset   dataset         `json:"dataset"`
		Limit     *int            `json:"limit"`
		Predicate json.RawMessage `json:"predicate"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	sc, ok := schemas[p.Dataset.Name]
	if !ok {
		return nil, fmt.Errorf("unknown dataset %q", p.Dataset.Name)
	}
	rows := buildRows(p.Dataset.Name)

	applied := map[string]bool{}

	// Predicate pushdown: only if we can fully evaluate the tree; otherwise we
	// leave it to turntable (which always re-applies the WHERE anyway).
	if len(p.Predicate) > 0 {
		var node predNode
		if err := json.Unmarshal(p.Predicate, &node); err == nil && canHandle(&node) {
			cols := colIndex(sc)
			filtered := rows[:0:0]
			for _, row := range rows {
				if evalPred(&node, row, cols) {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
			applied["predicate"] = true
		}
	}

	// Limit pushdown is only safe once the predicate is fully applied.
	if p.Limit != nil && applied["predicate"] == (len(p.Predicate) > 0) && *p.Limit < len(rows) {
		rows = rows[:*p.Limit]
		applied["limit"] = true
	}

	s.nextID++
	id := strconv.Itoa(s.nextID)
	s.scans[id] = &cursor{rows: rows}
	return map[string]any{"scanId": id, "schema": sc, "applied": applied}, nil
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
	done := cur.pos >= len(cur.rows)
	return map[string]any{"rows": batch, "done": done}, nil
}

// buildRows materializes a dataset's rows fresh on every scan, so the data is
// always current — that is the whole point of a live system-state connector.
func buildRows(name string) [][]any {
	switch name {
	case "env":
		environ := os.Environ()
		rows := make([][]any, 0, len(environ))
		for _, kv := range environ {
			k, v, _ := strings.Cut(kv, "=")
			rows = append(rows, []any{k, v})
		}
		return rows
	case "runtime":
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return [][]any{{
			runtime.NumGoroutine(),
			runtime.NumCPU(),
			int64(m.Alloc),
			int64(m.HeapObjects),
			time.Now().UTC().Format(time.RFC3339),
		}}
	}
	return nil
}

func colIndex(sc schema) map[string]int {
	idx := map[string]int{}
	for i, c := range sc.Columns {
		idx[c.Name] = i
	}
	return idx
}

// ---- predicate evaluation (the documented JSON subset) -----------------------

type predLit struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type predNode struct {
	Kind   string     `json:"kind"`
	Op     string     `json:"op"`
	Column string     `json:"column"`
	Value  *predLit   `json:"value"`
	Values []predLit  `json:"values"`
	Negate bool       `json:"negate"`
	Args   []predNode `json:"args"`
	Arg    *predNode  `json:"arg"`
}

// canHandle reports whether this plugin understands every node in the tree. We
// implement the common kinds; anything else makes us decline the whole
// predicate (turntable then applies it).
func canHandle(n *predNode) bool {
	switch n.Kind {
	case "compare", "in", "isnull":
		return true
	case "and", "or":
		for i := range n.Args {
			if !canHandle(&n.Args[i]) {
				return false
			}
		}
		return true
	case "not":
		return n.Arg != nil && canHandle(n.Arg)
	}
	return false
}

func evalPred(n *predNode, row []any, cols map[string]int) bool {
	switch n.Kind {
	case "and":
		for i := range n.Args {
			if !evalPred(&n.Args[i], row, cols) {
				return false
			}
		}
		return true
	case "or":
		for i := range n.Args {
			if evalPred(&n.Args[i], row, cols) {
				return true
			}
		}
		return false
	case "not":
		return !evalPred(n.Arg, row, cols)
	case "isnull":
		v := cell(row, cols, n.Column)
		isNull := v == nil
		return isNull != n.Negate
	case "in":
		v := cell(row, cols, n.Column)
		for _, lit := range n.Values {
			if compare(v, "=", lit) {
				return !n.Negate
			}
		}
		return n.Negate
	case "compare":
		if n.Value == nil {
			return false
		}
		return compare(cell(row, cols, n.Column), n.Op, *n.Value)
	}
	return false
}

func cell(row []any, cols map[string]int, name string) any {
	if i, ok := cols[name]; ok && i < len(row) {
		return row[i]
	}
	return nil
}

// compare evaluates `cellValue OP literal`. Strings compare lexically; numbers
// numerically; everything else falls back to string form.
func compare(v any, op string, lit predLit) bool {
	if v == nil {
		return false // NULL compares false to everything
	}
	switch lit.Type {
	case "int", "float":
		a, aok := toFloat(v)
		b, bok := toFloat(lit.Value)
		if !aok || !bok {
			return false
		}
		return numCmp(a, b, op)
	default:
		return strCmp(fmt.Sprint(v), fmt.Sprint(lit.Value), op)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func numCmp(a, b float64, op string) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func strCmp(a, b, op string) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}
