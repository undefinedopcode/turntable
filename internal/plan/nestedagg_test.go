package plan

import (
	"testing"

	"github.com/april/turntable/internal/engine"
)

// Aggregates may appear nested inside scalar expressions (the projection runs on
// top of the aggregate output), and in HAVING / ORDER BY. The ordersConn fixture
// is customer 1 -> {100, 19.49} USD and customer 2 -> {50} EUR.

func num(t *testing.T, v engine.Value) float64 {
	t.Helper()
	f, ok := v.AsFloat()
	if !ok {
		t.Fatalf("value %v is not numeric", v.V)
	}
	return f
}

func TestNestedAggregateScalarWrapper(t *testing.T) {
	// SUM(amount) wrapped in arithmetic: customer 1 -> 119.49*2, customer 2 -> 100.
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, SUM(amount) * 2 AS d FROM orders GROUP BY customer_id ORDER BY customer_id")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if got := num(t, rows[0].Values[1]); got != 238.98 {
		t.Errorf("customer 1 SUM*2 = %v, want 238.98", got)
	}
	if got := num(t, rows[1].Values[1]); got != 100 {
		t.Errorf("customer 2 SUM*2 = %v, want 100", got)
	}
}

func TestRatioOfTwoAggregates(t *testing.T) {
	// SUM(amount) / COUNT(*): customer 1 -> 119.49/2 = 59.745.
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, SUM(amount) / COUNT(*) AS avg FROM orders GROUP BY customer_id ORDER BY customer_id")
	if got := num(t, rows[0].Values[1]); got != 59.745 {
		t.Errorf("customer 1 avg = %v, want 59.745", got)
	}
}

func TestHavingWithAggregateExpression(t *testing.T) {
	// HAVING SUM(amount) > 60 keeps only customer 1 (119.49); customer 2 is 50.
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id FROM orders GROUP BY customer_id HAVING SUM(amount) > 60")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if n, _ := rows[0].Values[0].AsInt(); n != 1 {
		t.Errorf("kept customer %v, want 1", rows[0].Values[0].V)
	}
}

func TestOrderByAggregateExpression(t *testing.T) {
	// ORDER BY COUNT(*) DESC: customer 1 (2 orders) before customer 2 (1 order).
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, COUNT(*) AS n FROM orders GROUP BY customer_id ORDER BY COUNT(*) DESC, customer_id")
	if n, _ := rows[0].Values[1].AsInt(); n != 2 {
		t.Errorf("first group count = %v, want 2", rows[0].Values[1].V)
	}
}

func TestAggregateInCase(t *testing.T) {
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, CASE WHEN SUM(amount) > 100 THEN 'big' ELSE 'small' END AS sz FROM orders GROUP BY customer_id ORDER BY customer_id")
	if rows[0].Values[1].V != "big" { // 119.49
		t.Errorf("customer 1 -> %v, want big", rows[0].Values[1].V)
	}
	if rows[1].Values[1].V != "small" { // 50
		t.Errorf("customer 2 -> %v, want small", rows[1].Values[1].V)
	}
}

// Regression: the pre-existing alias references in HAVING/ORDER BY must keep
// working alongside the new aggregate-expression support.

func TestHavingByAliasPreserved(t *testing.T) {
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, SUM(amount) AS total FROM orders GROUP BY customer_id HAVING total > 60")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestOrderByAliasPreserved(t *testing.T) {
	rows := runQuery(t, ordersRegistry(t), "SELECT customer_id, SUM(amount) AS total FROM orders GROUP BY customer_id ORDER BY total DESC")
	if got := num(t, rows[0].Values[1]); got != 119.49 {
		t.Errorf("first total = %v, want 119.49 (descending)", got)
	}
}
