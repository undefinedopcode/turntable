package linearc

import (
	"context"
	"encoding/json"
	"io"
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

func TestResolveSchema(t *testing.T) {
	c := New()
	sc, err := c.Resolve(context.Background(), connector.Dataset{Source: "issues"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "identifier", "title", "priority", "state", "assignee", "team", "created_at", "updated_at", "url"}
	if len(sc.Columns) != len(want) {
		t.Fatalf("got %d cols, want %d", len(sc.Columns), len(want))
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
}

func TestUnknownDataset(t *testing.T) {
	c := New()
	if _, err := c.Resolve(context.Background(), connector.Dataset{Source: "bogus"}); err == nil {
		t.Fatal("expected error for unknown dataset")
	}
}

func TestScanIssuesPaginationAndFlatten(t *testing.T) {
	// Two pages: the first reports hasNextPage, the second ends.
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Errorf("Authorization = %q, want raw api key", got)
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			if _, ok := req.Variables["after"]; ok {
				t.Errorf("first request should not send after cursor")
			}
			w.Write([]byte(`{"data":{"issues":{"nodes":[
				{"id":"1","identifier":"ENG-1","title":"first","priority":2,
				 "createdAt":"2024-01-02T03:04:05Z","updatedAt":"2024-01-02T03:04:05Z","url":"http://x/1",
				 "state":{"name":"Todo"},"assignee":{"name":"Ada"},"team":{"key":"ENG"}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`))
			return
		}
		if req.Variables["after"] != "c1" {
			t.Errorf("second request after = %v, want c1", req.Variables["after"])
		}
		w.Write([]byte(`{"data":{"issues":{"nodes":[
			{"id":"2","identifier":"ENG-2","title":"second","priority":0,
			 "createdAt":"2024-01-03T00:00:00Z","updatedAt":"2024-01-03T00:00:00Z","url":"http://x/2",
			 "state":{"name":"Done"},"assignee":null,"team":{"key":"ENG"}}
		],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
	}))
	defer srv.Close()

	c := New()
	ds := connector.Dataset{Source: "issues", Options: map[string]any{"url": srv.URL, "api_key": "lin_api_test"}}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	// Row 0: priority coerced float64->int64; nested state flattened.
	r0 := rows[0].Values
	if r0[3].Type != engine.TypeInt || r0[3].V.(int64) != 2 {
		t.Errorf("priority = %+v, want int 2", r0[3])
	}
	if r0[4].V != "Todo" {
		t.Errorf("state = %v, want Todo", r0[4].V)
	}
	if r0[5].V != "Ada" {
		t.Errorf("assignee = %v, want Ada", r0[5].V)
	}
	if r0[7].Type != engine.TypeTime {
		t.Errorf("created_at type = %v, want time", r0[7].Type)
	}
	// Row 1: null assignee flattens to NULL.
	if !rows[1].Values[5].IsNull() {
		t.Errorf("row1 assignee = %+v, want NULL", rows[1].Values[5])
	}
}

func TestMissingAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := New()
	ds := connector.Dataset{Source: "teams", Options: map[string]any{"url": srv.URL}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Fatal("expected error when neither api_key nor bearer is set")
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"users":{"nodes":[
			{"id":"1","name":"a","displayName":"A","email":"a@x","active":true,"admin":false,"createdAt":"2024-01-01T00:00:00Z"},
			{"id":"2","name":"b","displayName":"B","email":"b@x","active":true,"admin":true,"createdAt":"2024-01-01T00:00:00Z"}
		],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
	}))
	defer srv.Close()

	one := 1
	c := New()
	ds := connector.Dataset{Source: "users", Options: map[string]any{"url": srv.URL, "bearer": "tok"}}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Limit: &one})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (limit)", len(rows))
	}
}
