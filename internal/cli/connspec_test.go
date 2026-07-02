package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectorSpecsSanity(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range connectorSpecs {
		if c.Name == "" || c.Label == "" {
			t.Errorf("spec %+v needs name and label", c)
		}
		if seen[c.Name] {
			t.Errorf("duplicate connector spec %q", c.Name)
		}
		seen[c.Name] = true
		if c.Name == "plugin" {
			t.Error("plugin must not be in the runtime-add spec table (arbitrary exec)")
		}
		for _, f := range c.Fields {
			if f.Key == "" || f.Label == "" {
				t.Errorf("%s: field %+v needs key and label", c.Name, f)
			}
			if f.Type == "select" && len(f.Options) == 0 {
				t.Errorf("%s.%s: select field needs options", c.Name, f.Key)
			}
		}
	}
	// The file connectors drive isFileConnector (path routing in .use and the
	// web form); this is the set the REPL previously hardcoded.
	for _, name := range []string{"csv", "json", "yaml", "excel", "parquet", "log"} {
		if !isFileConnector(name) {
			t.Errorf("isFileConnector(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"sql", "http", "claudelogs", "plugin", "nope"} {
		if isFileConnector(name) {
			t.Errorf("isFileConnector(%q) = true, want false", name)
		}
	}
}

func TestHandleConnectors(t *testing.T) {
	a := NewApp()
	req := httptest.NewRequest(http.MethodGet, "/api/connectors", nil)
	rec := httptest.NewRecorder()
	a.handleConnectors(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var specs []ConnectorSpec
	if err := json.Unmarshal(rec.Body.Bytes(), &specs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(specs) != len(connectorSpecs) {
		t.Fatalf("specs = %d, want %d", len(specs), len(connectorSpecs))
	}
	// Fields must round-trip with the keys the frontend types expect.
	if specs[0].Name != "csv" || !specs[0].File || len(specs[0].Fields) != 1 {
		t.Errorf("first spec = %+v, want csv file connector", specs[0])
	}
}
