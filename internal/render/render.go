// Package render formats a result row set for the terminal or stdout.
package render

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/april/turntable/internal/engine"
)

// Format selects an output renderer.
type Format string

const (
	FormatTable  Format = "table"
	FormatCSV    Format = "csv"
	FormatJSON   Format = "json"
	FormatNDJSON Format = "ndjson"
	FormatYAML   Format = "yaml"
	FormatRaw    Format = "raw"
)

// Renderer writes a schema + rows to an output stream.
type Renderer interface {
	Render(w io.Writer, schema engine.Schema, rows []engine.Row) error
}

// StreamRenderer writes rows from an iterator without buffering the entire
// result set, keeping memory bounded for large results. RenderStream returns
// the number of rows written.
type StreamRenderer interface {
	RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error)
}

// New returns a Renderer for the given format.
func New(f Format) (Renderer, error) {
	switch f {
	case FormatTable:
		return tableRenderer{}, nil
	case FormatCSV:
		return csvRenderer{}, nil
	case FormatJSON:
		return jsonRenderer{}, nil
	case FormatNDJSON:
		return ndjsonRenderer{}, nil
	case FormatYAML:
		return yamlRenderer{}, nil
	case FormatRaw:
		return rawRenderer{}, nil
	}
	return nil, fmt.Errorf("unknown output format %q", f)
}

// NewStream returns a StreamRenderer for the given format. Table format is not
// streamable (it needs column widths up front), so it returns an error; callers
// should fall back to materialize-then-render for table.
func NewStream(f Format) (StreamRenderer, error) {
	switch f {
	case FormatCSV:
		return csvRenderer{}, nil
	case FormatJSON:
		return jsonRenderer{}, nil
	case FormatNDJSON:
		return ndjsonRenderer{}, nil
	case FormatYAML:
		return yamlRenderer{}, nil
	case FormatRaw:
		return rawRenderer{}, nil
	}
	return nil, fmt.Errorf("format %q does not support streaming", f)
}

// colNames returns the schema's column names.
func colNames(schema engine.Schema) []string {
	names := make([]string, len(schema.Columns))
	for i, c := range schema.Columns {
		names[i] = c.Name
	}
	return names
}

// valString converts a Value to its display string for table/raw output.
func valString(v engine.Value) string {
	if v.IsNull() {
		return ""
	}
	if v.Type == engine.TypeAny {
		b, _ := json.Marshal(v.V)
		return string(b)
	}
	return engine.FormatValue(v)
}

// ---- table ------------------------------------------------------------------

type tableRenderer struct{}

func (tableRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	names := colNames(schema)
	if len(names) == 0 && len(rows) == 0 {
		return nil
	}
	// Compute column widths from headers and cell values.
	widths := make([]int, len(names))
	for i, n := range names {
		widths[i] = utf8.RuneCountInString(n)
	}
	cellStrs := make([][]string, len(rows))
	for r, row := range rows {
		cellStrs[r] = make([]string, len(names))
		for c := 0; c < len(names); c++ {
			var s string
			if c < len(row.Values) {
				s = valString(row.Values[c])
			}
			cellStrs[r][c] = s
			if l := utf8.RuneCountInString(s); l > widths[c] {
				widths[c] = l
			}
		}
	}
	// Header.
	writeRow(w, names, widths)
	writeSep(w, widths)
	for _, cells := range cellStrs {
		writeRow(w, cells, widths)
	}
	return nil
}

func writeRow(w io.Writer, cells []string, widths []int) {
	var b strings.Builder
	for i, c := range cells {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(c)
		if pad := widths[i] - utf8.RuneCountInString(c); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
	b.WriteString("\n")
	fmt.Fprint(w, b.String())
}

func writeSep(w io.Writer, widths []int) {
	var b strings.Builder
	for i, wd := range widths {
		if i > 0 {
			b.WriteString("-+-")
		}
		b.WriteString(strings.Repeat("-", wd))
	}
	b.WriteString("\n")
	fmt.Fprint(w, b.String())
}

// ---- csv --------------------------------------------------------------------

type csvRenderer struct{}

func (csvRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	names := colNames(schema)
	if err := cw.Write(names); err != nil {
		return err
	}
	for _, row := range rows {
		rec := make([]string, len(names))
		for i := 0; i < len(names); i++ {
			if i < len(row.Values) {
				rec[i] = valString(row.Values[i])
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	return nil
}

func (csvRenderer) RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error) {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	names := colNames(schema)
	if err := cw.Write(names); err != nil {
		return 0, err
	}
	n := 0
	for {
		row, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		rec := make([]string, len(names))
		for i := 0; i < len(names); i++ {
			if i < len(row.Values) {
				rec[i] = valString(row.Values[i])
			}
		}
		if err := cw.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---- json -------------------------------------------------------------------

type jsonRenderer struct{}

func (jsonRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	names := colNames(schema)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		obj := map[string]any{}
		for i, name := range names {
			if i < len(row.Values) {
				obj[name] = jsonValue(row.Values[i])
			}
		}
		out = append(out, obj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func (jsonRenderer) RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error) {
	names := colNames(schema)
	if _, err := w.Write([]byte("[\n")); err != nil {
		return 0, err
	}
	n := 0
	for {
		row, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		obj := map[string]any{}
		for i, name := range names {
			if i < len(row.Values) {
				obj[name] = jsonValue(row.Values[i])
			}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return n, err
		}
		if n > 0 {
			if _, err := w.Write([]byte(",\n")); err != nil {
				return n, err
			}
		}
		if _, err := w.Write(b); err != nil {
			return n, err
		}
		n++
	}
	if _, err := w.Write([]byte("\n]\n")); err != nil {
		return n, err
	}
	return n, nil
}

// ---- ndjson -----------------------------------------------------------------

type ndjsonRenderer struct{}

func (ndjsonRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	names := colNames(schema)
	enc := json.NewEncoder(w)
	for _, row := range rows {
		obj := map[string]any{}
		for i, name := range names {
			if i < len(row.Values) {
				obj[name] = jsonValue(row.Values[i])
			}
		}
		if err := enc.Encode(obj); err != nil {
			return err
		}
	}
	return nil
}

func (ndjsonRenderer) RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error) {
	names := colNames(schema)
	enc := json.NewEncoder(w)
	n := 0
	for {
		row, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		obj := map[string]any{}
		for i, name := range names {
			if i < len(row.Values) {
				obj[name] = jsonValue(row.Values[i])
			}
		}
		if err := enc.Encode(obj); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// jsonValue converts an engine.Value to a JSON-encodable Go value. NULL maps
// to nil so json emits `null`.
func jsonValue(v engine.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Type {
	case engine.TypeAny:
		return v.V
	}
	return v.V
}

// ---- yaml -------------------------------------------------------------------

type yamlRenderer struct{}

func (yamlRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	names := colNames(schema)
	// Emit a YAML sequence of mappings, mirroring ndjson.
	for _, row := range rows {
		fmt.Fprintln(w, "-")
		for i, name := range names {
			if i < len(row.Values) {
				fmt.Fprintf(w, "  %s: %s\n", yamlKey(name), yamlScalar(row.Values[i]))
			}
		}
	}
	return nil
}

func (yamlRenderer) RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error) {
	names := colNames(schema)
	n := 0
	for {
		row, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		fmt.Fprintln(w, "-")
		for i, name := range names {
			if i < len(row.Values) {
				fmt.Fprintf(w, "  %s: %s\n", yamlKey(name), yamlScalar(row.Values[i]))
			}
		}
		n++
	}
	return n, nil
}

func yamlKey(s string) string {
	// simple quoting for keys with special chars; otherwise bare
	if strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func yamlScalar(v engine.Value) string {
	if v.IsNull() {
		return "null"
	}
	if v.Type == engine.TypeAny {
		b, _ := json.Marshal(v.V)
		return string(b)
	}
	return engine.FormatValue(v)
}

// ---- raw --------------------------------------------------------------------

type rawRenderer struct{}

func (rawRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	for _, row := range rows {
		var b strings.Builder
		for i, v := range row.Values {
			if i > 0 {
				b.WriteString("\t")
			}
			b.WriteString(valString(v))
		}
		b.WriteString("\n")
		fmt.Fprint(w, b.String())
	}
	return nil
}

func (rawRenderer) RenderStream(w io.Writer, schema engine.Schema, it engine.RowIterator) (int, error) {
	n := 0
	for {
		row, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		var b strings.Builder
		for i, v := range row.Values {
			if i > 0 {
				b.WriteString("\t")
			}
			b.WriteString(valString(v))
		}
		b.WriteString("\n")
		if _, err := w.Write([]byte(b.String())); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}