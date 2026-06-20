// Package jsonc is the JSON file connector. It exposes a single JSON file
// (object or array of objects) as a dataset of rows. Schema is inferred from
// the first array element or object keys.
package jsonc

import (
	"context"
	"fmt"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

// Connector reads JSON files.
type Connector struct{}

// New constructs a JSON connector.
func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "json" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil // file connectors expose datasets ad-hoc via qualified refs
}

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	// TODO(v0.1): read & infer schema from the JSON file at ds.Source.
	return engine.Schema{}, fmt.Errorf("jsonc.Resolve not yet implemented (v0.1)")
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	// TODO(v0.1): stream rows from the JSON file.
	return nil, fmt.Errorf("jsonc.Scan not yet implemented (v0.1)")
}