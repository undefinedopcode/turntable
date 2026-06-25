package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// Window functions planned end to end. ordersConn: customer 1 -> {100, 19.49}
// USD, customer 2 -> {50} EUR, returned in that row order. rowsConn: s1 =
// {1,2,2} (one column n) is handy for ranking ties.

func TestWindowRowNumberAndPartitionSum(t *testing.T) {
	rows := runQuery(t, ordersRegistry(t),
		"SELECT customer_id, amount, "+
			"ROW_NUMBER() OVER (PARTITION BY customer_id ORDER BY amount) AS rn, "+
			"SUM(amount) OVER (PARTITION BY customer_id) AS total FROM orders")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (window preserves input order)", len(rows))
	}
	// Input order: (1,100),(1,19.49),(2,50).
	check := func(i int, cust int64, rn int64, total float64) {
		if c, _ := rows[i].Values[0].AsInt(); c != cust {
			t.Errorf("row%d customer = %v, want %d", i, rows[i].Values[0].V, cust)
		}
		if n, _ := rows[i].Values[2].AsInt(); n != rn {
			t.Errorf("row%d rn = %v, want %d", i, rows[i].Values[2].V, rn)
		}
		if f, _ := rows[i].Values[3].AsFloat(); f != total {
			t.Errorf("row%d total = %v, want %v", i, rows[i].Values[3].V, total)
		}
	}
	check(0, 1, 2, 119.49) // amount 100 is the 2nd-smallest in customer 1
	check(1, 1, 1, 119.49) // amount 19.49 is the smallest
	check(2, 2, 1, 50)
}

func TestWindowRankWithTies(t *testing.T) {
	// s1 = {1,2,2} ordered ascending: ranks 1,2,2; dense 1,2,2; row_number 1,2,3.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT n, "+
			"ROW_NUMBER() OVER (ORDER BY n) AS rn, "+
			"RANK() OVER (ORDER BY n) AS rk, "+
			"DENSE_RANK() OVER (ORDER BY n) AS dr FROM s1 ORDER BY rn")
	want := [][4]int64{{1, 1, 1, 1}, {2, 2, 2, 2}, {2, 3, 2, 2}}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, w := range want {
		for j, exp := range w {
			if got, _ := rows[i].Values[j].AsInt(); got != exp {
				t.Errorf("row%d col%d = %d, want %d", i, j, got, exp)
			}
		}
	}
}

func TestWindowRunningSumRange(t *testing.T) {
	// s1 = {1,2,2} ordered: running SUM with the default RANGE frame gives the two
	// tied 2s the same cumulative value (1+2+2=5), not 3 then 5.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT n, SUM(n) OVER (ORDER BY n) AS running FROM s1 ORDER BY n, running")
	wantRun := []float64{1, 5, 5}
	for i, w := range wantRun {
		if got, _ := rows[i].Values[1].AsFloat(); got != w {
			t.Errorf("row%d running = %v, want %v", i, rows[i].Values[1].V, w)
		}
	}
}

func TestWindowLagLead(t *testing.T) {
	// Over s1 = {1,2,2} ordered by n: LAG(n) is the prior row's n (NULL first),
	// LEAD(n,1,-1) the next (-1 past the end).
	rows := runQuery(t, rowsRegistry(t),
		"SELECT n, LAG(n) OVER (ORDER BY n) AS prev, LEAD(n, 1, -1) OVER (ORDER BY n) AS nxt FROM s1 ORDER BY n, prev")
	if !rows[0].Values[1].IsNull() {
		t.Errorf("first prev = %v, want NULL", rows[0].Values[1].V)
	}
	if v, _ := rows[2].Values[2].AsInt(); v != -1 {
		t.Errorf("last nxt = %v, want -1 (default past end)", rows[2].Values[2].V)
	}
}

func TestWindowWithGroupByRejected(t *testing.T) {
	reg := ordersRegistry(t)
	stmt, _ := sql.Parse("SELECT customer_id, COUNT(*), ROW_NUMBER() OVER (ORDER BY customer_id) FROM orders GROUP BY customer_id")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected error combining window functions with GROUP BY")
	}
}
