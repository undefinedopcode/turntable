// Package yamlc is the YAML file connector. It reads a YAML file containing
// either a sequence of mappings (each a row) or a single mapping (one row).
// Multi-document YAML (--- separated) is also supported; documents that are
// sequences are flattened into the row stream.
package yamlc

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "yaml" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	docs, err := sampleDocs(ctx, ds.Source, 64)
	if err != nil {
		return engine.Schema{}, err
	}
	order := []string{}
	seen := map[string]bool{}
	for _, d := range docs {
		for _, r := range d {
			for k := range r {
				if !seen[k] {
					seen[k] = true
					order = append(order, k)
				}
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
	schema, err := (Connector{}).Resolve(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(req.Dataset.Source)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", req.Dataset.Source, err)
	}
	return &yamlIter{f: f, dec: yaml.NewDecoder(f), schema: schema}, nil
}

type yamlIter struct {
	f       *os.File
	dec     *yaml.Decoder
	schema  engine.Schema
	pending []map[string]any // extra rows from a multi-row document
	closed  bool
}

func (y *yamlIter) Next() (engine.Row, bool, error) {
	// Drain pending rows from a previous multi-row document first.
	if len(y.pending) > 0 {
		r := y.pending[0]
		y.pending = y.pending[1:]
		return rowFromObject(r, y.schema), true, nil
	}
	for {
		var doc any
		if err := y.dec.Decode(&doc); err != nil {
			if err == io.EOF {
				return engine.Row{}, false, nil
			}
			return engine.Row{}, false, err
		}
		rows := docToRows(doc)
		if len(rows) == 0 {
			continue
		}
		if len(rows) > 1 {
			y.pending = append(y.pending, rows[1:]...)
		}
		return rowFromObject(rows[0], y.schema), true, nil
	}
}

func (y *yamlIter) Close() error {
	if y.closed {
		return nil
	}
	y.closed = true
	return y.f.Close()
}

// docToRows converts a decoded YAML document into a list of row objects.
// A mapping becomes a single-row slice; a sequence of mappings becomes many;
// anything else is dropped (yielding an empty slice so Next() continues).
func docToRows(doc any) []map[string]any {
	switch x := doc.(type) {
	case map[string]any:
		return []map[string]any{x}
	case []any:
		var out []map[string]any
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func rowFromObject(obj map[string]any, schema engine.Schema) engine.Row {
	if len(schema.Columns) == 0 {
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

func sampleDocs(ctx context.Context, path string, n int) ([][]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var out [][]map[string]any
	count := 0
	for {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		rows := docToRows(doc)
		out = append(out, rows)
		count += len(rows)
		if count >= n {
			break
		}
	}
	return out, nil
}