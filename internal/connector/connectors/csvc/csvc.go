// Package csvc is the CSV file connector. It streams a CSV file as rows,
// using the first row as headers (unless configured otherwise) and inferring
// column types from a sample.
package csvc

import (
	"context"
	"fmt"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "csv" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{}, fmt.Errorf("csvc.Resolve not yet implemented (v0.1)")
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	return nil, fmt.Errorf("csvc.Scan not yet implemented (v0.1)")
}