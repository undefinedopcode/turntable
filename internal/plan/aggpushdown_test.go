package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// aggPusherConn is a connector implementing AggregatePusher for planner tests. It
// returns raw rows for a plain scan and canned aggregated rows for a pushed
// aggregate scan, and records the AggregateRequest it received. decline makes
// PushAggregate return ok=false so the planner falls back to engine aggregation.
type aggPusherConn struct {
	decline    bool
	lastReq    *connector.AggregateRequest
	sawAggScan bool
}

func (*aggPusherConn) Name() string { return "aggpush" }
func (*aggPusherConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}

var aggRawSchema = engine.Schema{Columns: []engine.Column{
	{Name: "svc", Type: engine.TypeString, Nullable: true},
	{Name: "dur", Type: engine.TypeInt, Nullable: true},
}}

func (*aggPusherConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return aggRawSchema, nil
}

func (c *aggPusherConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	if req.Aggregate != nil {
		c.sawAggScan = true
		// Pre-aggregated rows: [svc, n] — deliberately not re-aggregatable (each
		// group already reduced), so a stray engine Aggregate would corrupt them.
		return engine.NewSliceIter([]engine.Row{
			{Values: []engine.Value{engine.StringVal("a"), engine.IntVal(10)}},
			{Values: []engine.Value{engine.StringVal("b"), engine.IntVal(3)}},
		}), nil
	}
	// Raw rows for the engine-aggregation fallback: a appears twice, b once.
	return engine.NewSliceIter([]engine.Row{
		{Values: []engine.Value{engine.StringVal("a"), engine.IntVal(10)}},
		{Values: []engine.Value{engine.StringVal("a"), engine.IntVal(20)}},
		{Values: []engine.Value{engine.StringVal("b"), engine.IntVal(3)}},
	}), nil
}

func (c *aggPusherConn) PushAggregate(ctx context.Context, ds connector.Dataset, agg connector.AggregateRequest) (engine.Schema, bool, error) {
	c.lastReq = &agg
	if c.decline {
		return engine.Schema{}, false, nil
	}
	cols := make([]engine.Column, 0, len(agg.GroupBy)+len(agg.Aggregates))
	for _, g := range agg.GroupBy {
		cols = append(cols, engine.Column{Name: g, Type: engine.TypeString, Nullable: true})
	}
	for _, op := range agg.Aggregates {
		cols = append(cols, engine.Column{Name: op.Alias, Type: engine.TypeInt, Nullable: true})
	}
	return engine.Schema{Columns: cols}, true, nil
}

func aggPusherRegistry(t *testing.T, c *aggPusherConn) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(c)
	if err := reg.RegisterSource("t", c, connector.Dataset{Name: "t"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

func runAgg(t *testing.T, reg *connector.Registry, query string) ([]engine.Row, engine.Schema) {
	t.Helper()
	stmt, err := sql.Parse(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	it, schema, err := Exec(context.Background(), p)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows, schema
}

// TestAggregatePushedToConnector: an AggregatePusher receives the grouping,
// aggregates and predicate, and its already-aggregated rows pass straight through
// (no engine re-aggregation, no WHERE re-filter).
func TestAggregatePushedToConnector(t *testing.T) {
	c := &aggPusherConn{}
	reg := aggPusherRegistry(t, c)
	rows, schema := runAgg(t, reg, "SELECT svc, COUNT(*) AS n FROM t WHERE dur > 5 GROUP BY svc")

	if c.lastReq == nil {
		t.Fatal("PushAggregate was not called")
	}
	if len(c.lastReq.GroupBy) != 1 || c.lastReq.GroupBy[0] != "svc" {
		t.Errorf("GroupBy = %v, want [svc]", c.lastReq.GroupBy)
	}
	if len(c.lastReq.Aggregates) != 1 || c.lastReq.Aggregates[0].Func != "COUNT" {
		t.Errorf("Aggregates = %+v, want one COUNT", c.lastReq.Aggregates)
	}
	if c.lastReq.Predicate == nil {
		t.Error("Predicate was not pushed")
	}
	if !c.sawAggScan {
		t.Error("Scan did not receive the aggregate request")
	}
	// The connector's rows pass through unchanged: a=10, b=3. A stray engine
	// Aggregate (COUNT) would instead yield 1 per group.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if n, _ := rows[0].Values[1].AsInt(); n != 10 {
		t.Errorf("row[0] n = %v, want 10 (connector value, not a re-count)", rows[0].Values[1].V)
	}
	if schema.Columns[1].Name != "n" || schema.Columns[1].Type != engine.TypeInt {
		t.Errorf("output col = %+v, want n int", schema.Columns[1])
	}
}

// TestAggregatePushdownDeclinedFallsBack: when PushAggregate declines, the engine
// aggregates the connector's raw rows itself (a appears twice -> COUNT 2).
func TestAggregatePushdownDeclinedFallsBack(t *testing.T) {
	c := &aggPusherConn{decline: true}
	reg := aggPusherRegistry(t, c)
	rows, _ := runAgg(t, reg, "SELECT svc, COUNT(*) AS n FROM t GROUP BY svc ORDER BY svc")

	if c.sawAggScan {
		t.Error("Scan should have received a raw (non-aggregate) request after decline")
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Engine aggregation over raw rows: a=2, b=1.
	if n, _ := rows[0].Values[1].AsInt(); rows[0].Values[0].V != "a" || n != 2 {
		t.Errorf("row[0] = (%v,%v), want (a,2)", rows[0].Values[0].V, rows[0].Values[1].V)
	}
	if n, _ := rows[1].Values[1].AsInt(); rows[1].Values[0].V != "b" || n != 1 {
		t.Errorf("row[1] = (%v,%v), want (b,1)", rows[1].Values[0].V, rows[1].Values[1].V)
	}
}
