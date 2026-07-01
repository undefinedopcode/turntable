package cli

// Dashboards: named, ordered lists of panels (markdown / table / pivot / chart /
// stat) persisted as one YAML file each under .turntable/dashboards/ — see
// docs/dashboards-design.md. The server only stores and serves the definitions;
// the web client runs each panel's query through the normal /api/query path, so
// row caps, error shapes, and session statements behave exactly like a tab.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// dashDirPath is the project-relative directory holding dashboard definitions.
// Like the upload dir it is persistent and git-committable (dashboards are meant
// to be shared); it is created lazily on first save.
const dashDirPath = ".turntable/dashboards"

// Dashboard is one dashboard/story definition. Panels render top-to-bottom in
// list order (`width: half` lets two adjacent panels share a row).
type Dashboard struct {
	Name        string                  `yaml:"name" json:"name"`
	Description string                  `yaml:"description,omitempty" json:"description,omitempty"`
	Variables   map[string]DashboardVar `yaml:"variables,omitempty" json:"variables,omitempty"`
	Panels      []DashboardPanel        `yaml:"panels" json:"panels"`
}

// DashboardVar is a `{{name}}` substitution variable: a default value and an
// optional query whose first column populates a dropdown of choices.
type DashboardVar struct {
	Default      string `yaml:"default,omitempty" json:"default,omitempty"`
	OptionsQuery string `yaml:"options_query,omitempty" json:"options_query,omitempty"`
}

// DashboardPanel is one panel. View is the frontend's serialized ViewConfig
// (webui/src/view.ts) and is deliberately opaque to the server.
type DashboardPanel struct {
	Kind  string         `yaml:"kind" json:"kind"`
	Title string         `yaml:"title,omitempty" json:"title,omitempty"`
	Text  string         `yaml:"text,omitempty" json:"text,omitempty"`
	Width string         `yaml:"width,omitempty" json:"width,omitempty"`
	Query string         `yaml:"query,omitempty" json:"query,omitempty"`
	View  map[string]any `yaml:"view,omitempty" json:"view,omitempty"`
}

var panelKinds = map[string]bool{
	"markdown": true, "table": true, "pivot": true, "chart": true, "stat": true,
}

func (d *Dashboard) validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("dashboard name is required")
	}
	for i, p := range d.Panels {
		if !panelKinds[p.Kind] {
			return fmt.Errorf("panel %d: unknown kind %q", i+1, p.Kind)
		}
		if p.Kind == "markdown" && strings.TrimSpace(p.Text) == "" {
			return fmt.Errorf("panel %d: a markdown panel needs text", i+1)
		}
		if p.Kind != "markdown" && strings.TrimSpace(p.Query) == "" {
			return fmt.Errorf("panel %d: a %s panel needs a query", i+1, p.Kind)
		}
		switch p.Width {
		case "", "full", "half":
		default:
			return fmt.Errorf("panel %d: width must be full or half, not %q", i+1, p.Width)
		}
	}
	return nil
}

// slugRe is the only shape a slug may take in a URL or filename — no dots or
// separators, so a slug can never escape the dashboards directory.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// slugify derives a filename-safe slug from a dashboard name ("Station
// Overview" -> "station-overview").
func slugify(name string) string {
	var b strings.Builder
	dash := true // swallow leading separators
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 100 {
		s = strings.TrimRight(s[:100], "-")
	}
	if s == "" {
		return "dashboard"
	}
	return s
}

func (a *App) dashPath(slug string) string {
	return filepath.Join(a.dashDir, slug+".yaml")
}

func (a *App) loadDashboard(slug string) (*Dashboard, error) {
	data, err := os.ReadFile(a.dashPath(slug))
	if err != nil {
		return nil, err
	}
	var d Dashboard
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s.yaml: %w", slug, err)
	}
	return &d, nil
}

func (a *App) saveDashboard(slug string, d *Dashboard) error {
	if err := os.MkdirAll(a.dashDir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(a.dashPath(slug), data, 0o644)
}

// dashboardSummary is one list entry. A file that fails to parse is still
// listed (name = slug) with its error, so a bad hand edit is visible rather
// than silently vanishing.
type dashboardSummary struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Panels      int    `json:"panels"`
	Error       string `json:"error,omitempty"`
}

func (a *App) listDashboards() ([]dashboardSummary, error) {
	entries, err := os.ReadDir(a.dashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []dashboardSummary{}, nil
		}
		return nil, err
	}
	out := []dashboardSummary{}
	for _, e := range entries {
		name := e.Name()
		slug := strings.TrimSuffix(name, ".yaml")
		if e.IsDir() || slug == name || !slugRe.MatchString(slug) {
			continue
		}
		d, err := a.loadDashboard(slug)
		if err != nil {
			out = append(out, dashboardSummary{Slug: slug, Name: slug, Error: err.Error()})
			continue
		}
		out = append(out, dashboardSummary{
			Slug: slug, Name: d.Name, Description: d.Description, Panels: len(d.Panels),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// handleDashboards is GET /api/dashboards (list) and POST /api/dashboards
// (create or update — an upsert keyed by slug).
func (a *App) handleDashboards(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.listDashboards()
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, list)
	case http.MethodPost:
		var req struct {
			// Slug set = update that dashboard; empty = derive from the name
			// (a create, or an overwrite of the same-named dashboard).
			Slug string `json:"slug"`
			Dashboard
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := req.Dashboard.validate(); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		slug := req.Slug
		if slug == "" {
			slug = slugify(req.Dashboard.Name)
		} else if !slugRe.MatchString(slug) {
			writeJSON(w, map[string]any{"error": fmt.Sprintf("bad dashboard slug %q", slug)})
			return
		}
		if err := a.saveDashboard(slug, &req.Dashboard); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"slug": slug, "saved": filepath.ToSlash(a.dashPath(slug))})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleDashboardItem is GET /api/dashboards/{slug} and DELETE
// /api/dashboards/{slug}.
func (a *App) handleDashboardItem(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/dashboards/")
	if !slugRe.MatchString(slug) {
		http.Error(w, "bad dashboard slug", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		d, err := a.loadDashboard(slug)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "unknown dashboard", http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, struct {
			Slug string `json:"slug"`
			*Dashboard
		}{slug, d})
	case http.MethodDelete:
		if err := os.Remove(a.dashPath(slug)); err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "unknown dashboard", http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"deleted": slug})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}
