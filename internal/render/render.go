// Package render formats a result row set for the terminal or stdout.
package render

import (
	"fmt"
	"io"

	"github.com/april/octoparser/internal/engine"
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

// New returns a Renderer for the given format.
func New(f Format) (Renderer, error) {
	switch f {
	case FormatTable, FormatCSV, FormatJSON, FormatNDJSON, FormatYAML, FormatRaw:
		// Concrete renderers land in v0.1; return a stub for now.
		return stubRenderer{}, nil
	}
	return nil, fmt.Errorf("unknown output format %q", f)
}

type stubRenderer struct{}

func (stubRenderer) Render(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	_, err := fmt.Fprintf(w, "(rendering not yet implemented; got %d rows)\n", len(rows))
	_ = schema
	return err
}