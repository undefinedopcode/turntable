package httpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	var rows []engine.Row
	for {
		r, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		rows = append(rows, r)
	}
	it.Close()
	return rows
}

func ds(url string, opts map[string]any) connector.Dataset {
	if opts == nil {
		opts = map[string]any{}
	}
	return connector.Dataset{Name: "t", Source: url, Options: opts}
}

func TestScanRootArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`))
	}))
	defer srv.Close()

	c := New()
	sc, err := c.Resolve(context.Background(), ds(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := colNames(sc); len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("schema = %v", got)
	}

	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(srv.URL, nil)})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// id column is index 0; values arrive as JSON numbers (float64 -> any).
	if rows[0].Values[1].V != "a" {
		t.Fatalf("row0 name = %v", rows[0].Values[1].V)
	}
}

func TestScanNestedPathAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xyz" {
			t.Errorf("missing bearer header, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Api-Key") != "k1" {
			t.Errorf("missing x-api-key, got %q", r.Header.Get("X-Api-Key"))
		}
		w.Write([]byte(`{"data":{"rows":[{"x":1},{"x":2},{"x":3}]}}`))
	}))
	defer srv.Close()

	opts := map[string]any{"path": "data.rows", "bearer": "xyz", "header_x_api_key": "k1"}
	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(srv.URL, opts)})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
}

func TestScanLimitWithoutPredicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"x":1},{"x":2},{"x":3},{"x":4}]`))
	}))
	defer srv.Close()

	two := 2
	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(srv.URL, nil), Limit: &two})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (limit honored)", len(rows))
	}
}

func TestScanSingleObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":7,"ok":true}`))
	}))
	defer srv.Close()

	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(srv.URL, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New()
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(srv.URL, nil)}); err == nil {
		t.Fatal("expected error on 500 status")
	}
}

func colNames(s engine.Schema) []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}
