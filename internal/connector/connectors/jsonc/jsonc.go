// Package jsonc is the JSON file connector. It exposes a JSON file (object,
// array of objects, or newline-delimited JSON) as a dataset of rows. The
// schema is inferred from the first object's keys; columns are ordered by
// first appearance and all columns are nullable (missing keys yield NULL).
package jsonc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

// Connector reads JSON files.
type Connector struct{}

// New constructs a JSON connector.
func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "json" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	path := ds.Source
	records, err := sampleRecords(ctx, path, 64)
	if err != nil {
		return engine.Schema{}, err
	}
	if len(records) == 0 {
		return engine.Schema{}, nil
	}
	// Collect column names in sorted order for determinism (Go map iteration is
	// randomized, so first-appearance order would be non-deterministic across
	// Resolve calls). v0.1 trades insertion order for correctness.
	order := []string{}
	seen := map[string]bool{}
	for _, r := range records {
		for k := range r {
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	sort.Strings(order)
	cols := make([]engine.Column, len(order))
	for i, name := range order {
		cols[i] = engine.Column{Name: name, Type: engine.TypeAny, Nullable: true}
	}
	return engine.Schema{Columns: cols}, nil
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	path := req.Dataset.Source
	// Resolve schema once so rows are aligned to a stable column order.
	schema, err := (Connector{}).Resolve(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	br := bufio.NewReader(f)
	b, err := br.Peek(1)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	switch strings.TrimSpace(string(b)) {
	case "[":
		return newArrayIterator(br, schema)
	default:
		return newNDJSONIterator(br, schema)
	}
}

// ---- array iterator ---------------------------------------------------------

type arrayIter struct {
	dec    *json.Decoder
	schema engine.Schema
	row    json.RawMessage
	idx    int
	err    error
	closed bool
}

func newArrayIterator(br *bufio.Reader, schema engine.Schema) (*arrayIter, error) {
	dec := json.NewDecoder(br)
	// read opening [
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected JSON array, got %v", tok)
	}
	return &arrayIter{dec: dec, schema: schema}, nil
}

func (a *arrayIter) Next() (engine.Row, bool, error) {
	if !a.dec.More() {
		return engine.Row{}, false, nil
	}
	var obj map[string]any
	if err := a.dec.Decode(&obj); err != nil {
		return engine.Row{}, false, err
	}
	return rowFromObject(obj, a.schema), true, nil
}

func (a *arrayIter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	return nil
}

// ---- NDJSON iterator --------------------------------------------------------

type ndjsonIter struct {
	scanner *bufio.Scanner
	schema  engine.Schema
	closed  bool
}

func newNDJSONIterator(br *bufio.Reader, schema engine.Schema) (*ndjsonIter, error) {
	s := bufio.NewScanner(br)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &ndjsonIter{scanner: s, schema: schema}, nil
}

func (n *ndjsonIter) Next() (engine.Row, bool, error) {
	for {
		if !n.scanner.Scan() {
			if err := n.scanner.Err(); err != nil {
				return engine.Row{}, false, err
			}
			return engine.Row{}, false, nil
		}
		line := strings.TrimSpace(n.scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return engine.Row{}, false, fmt.Errorf("invalid JSON line: %w", err)
		}
		return rowFromObject(obj, n.schema), true, nil
	}
}

func (n *ndjsonIter) Close() error {
	if n.closed {
		return nil
	}
	n.closed = true
	return nil
}

// rowFromObject builds a row aligned to schema. Missing keys -> NULL. Extra
// keys are dropped (schema is the source of truth). When schema is empty we
// use the object's key order.
func rowFromObject(obj map[string]any, schema engine.Schema) engine.Row {
	if len(schema.Columns) == 0 {
		// schema not known: emit in sorted key order for determinism
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vals := make([]engine.Value, len(keys))
		for i, k := range keys {
			vals[i] = connector.FromAny(obj[k])
		}
		return engine.Row{Values: vals}
	}
	vals := make([]engine.Value, len(schema.Columns))
	for i, c := range schema.Columns {
		v, ok := obj[c.Name]
		if !ok {
			vals[i] = engine.Null()
		} else {
			vals[i] = connector.FromAny(v)
		}
	}
	return engine.Row{Values: vals}
}

// sampleRecords reads up to n objects from the file to infer schema order.
// It handles arrays and NDJSON transparently.
func sampleRecords(ctx context.Context, path string, n int) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	b, err := br.Peek(1)
	if err != nil {
		// empty file -> no records
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(b))
	var out []map[string]any
	if trimmed == "[" {
		dec := json.NewDecoder(br)
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		for dec.More() && len(out) < n {
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				return nil, err
			}
			out = append(out, obj)
		}
		return out, nil
	}
	// NDJSON
	s := bufio.NewScanner(br)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for s.Scan() && len(out) < n {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return nil, fmt.Errorf("invalid JSON line: %w", err)
		}
		out = append(out, obj)
	}
	return out, s.Err()
}