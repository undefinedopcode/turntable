package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// ordersConn returns a fixed orders relation (customer_id, amount, currency) so
// aggregate column ordering can be exercised end to end.
type ordersConn struct{}

func (ordersConn) Name() string { return "orders" }
func (ordersConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (ordersConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{
		{Name: "customer_id", Type: engine.TypeInt, Nullable: true},
		{Name: "amount", Type: engine.TypeFloat, Nullable: true},
		{Name: "currency", Type: engine.TypeString, Nullable: true},
	}}, nil
}
func (ordersConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.FloatVal(100), engine.StringVal("USD")}},
		{Values: []engine.Value{engine.IntVal(1), engine.FloatVal(19.49), engine.StringVal("USD")}},
		{Values: []engine.Value{engine.IntVal(2), engine.FloatVal(50), engine.StringVal("EUR")}},
	}
	return engine.NewSliceIter(rows), nil
}

func ordersRegistry(t *testing.T) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(ordersConn{})
	if err := reg.RegisterSource("orders", ordersConn{}, connector.Dataset{Name: "orders"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestAggregateColumnOrdering is a regression test for a bug where a GROUP BY
// column appearing AFTER an aggregate in the SELECT list got the aggregate's
// value (and vice versa): the aggregate output schema was in SELECT-list order
// while the engine emits rows as [keys..., aggregates...].
func TestAggregateColumnOrdering(t *testing.T) {
	reg := ordersRegistry(t)
	stmt, err := sql.Parse(
		"SELECT customer_id, SUM(amount) AS total, currency FROM orders " +
			"GROUP BY customer_id, currency ORDER BY customer_id")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatal(err)
	}
	if names := colNames(p.OutputSchema); names[0] != "customer_id" || names[1] != "total" || names[2] != "currency" {
		t.Fatalf("output columns = %v, want [customer_id total currency]", names)
	}
	it, _, err := Exec(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Row 0: customer 1 -> total 119.49, currency USD. The value under each
	// header must match the header, not be shuffled into key-then-agg order.
	r0 := rows[0].Values
	if v, _ := r0[0].AsInt(); v != 1 {
		t.Errorf("customer_id = %v, want 1", r0[0].V)
	}
	if f, _ := r0[1].AsFloat(); f != 119.49 {
		t.Errorf("total = %v, want 119.49 (the SUM, not the currency)", r0[1].V)
	}
	if r0[2].AsString() != "USD" {
		t.Errorf("currency = %v, want USD (the group key, not the total)", r0[2].V)
	}
}

func colNames(s engine.Schema) []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}
