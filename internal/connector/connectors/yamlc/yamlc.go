// Package yamlc is the YAML file connector. It reads a YAML file (a single
// document mapping or a sequence of mappings) as rows.
package yamlc

import (
	"context"
	"fmt"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "yaml" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{}, fmt.Errorf("yamlc.Resolve not yet implemented (v0.1)")
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	return nil, fmt.Errorf("yamlc.Scan not yet implemented (v0.1)")
}