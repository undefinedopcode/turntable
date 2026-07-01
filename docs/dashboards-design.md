# Design: dashboards / stories

Status: **v1 implemented** (server store + API in `internal/cli/dashboard.go`;
frontend in `webui/src/components/DashboardView.tsx` / `PinModal.tsx` /
`Markdown.tsx`). All four sequencing steps below shipped: YAML store +
endpoints, read-only rendering, the variables toolbar, and pin-to-dashboard
authoring. The "Later" section remains future work.

The prerequisite shipped first: the web UI's results-pane view configuration
(table/chart/pivot mode + each view's settings) is a serializable
`ViewConfig` (`webui/src/view.ts`), persisted per tab and keyed by **column
name** rather than index. A dashboard panel is exactly *a query plus one of
those frozen view configs*, so the authoring UX is "pin this tab".

Goal: let a user assemble the results of several queries — row tables, pivot
tables, charts, prose — into a named, persistent, shareable document, for two
overlapping uses:

- **Dashboard**: an at-a-glance operational surface (sensor fleet health,
  cost overview) that re-runs its queries on open/refresh.
- **Story**: an analysis narrative — markdown interleaved with evidence
  panels — that can be handed to someone else ("here's why station N-04's
  night-time flow is anomalous").

The motivating scenario is a time-series platform (e.g. water-flow sensors)
where panels draw on *multiple* sources: readings in Timescale/Parquet, station
metadata in a CSV, alarms from an API connector — joined by turntable, often
via a materialized view.

## Model

A **dashboard** is a named, *ordered* list of **panels**. Stored as one YAML
file per dashboard under `.turntable/dashboards/<slug>.yaml` (sibling of the
upload dir; project-relative, git-committable, hand-editable).

```yaml
name: Station Overview
description: Daily flow analysis across the north district
variables:
  station:
    default: "N-04"
    options_query: SELECT DISTINCT station FROM stations ORDER BY station
  range:
    default: 24h            # relative range; substituted into INTERVAL '…'
panels:
  - kind: markdown
    text: |
      ## Flow anomalies, {{station}}
      Readings above the trailing P99 for the selected window.
  - kind: stat
    title: Current flow (L/s)
    query: SELECT flow FROM readings WHERE station = {{station}} ORDER BY ts DESC LIMIT 1
  - kind: chart
    title: Flow, 5-minute average
    width: full              # full (default) | half
    query: |
      SELECT DATE_BIN('5 minutes', ts) AS t, AVG(flow) AS flow
      FROM readings
      WHERE station = {{station}} AND ts > NOW() - INTERVAL '{{range}}'
      GROUP BY t ORDER BY t
    view: { chart: { type: line, x: t, y: [flow] } }
  - kind: pivot
    title: Daily volume by station
    width: half
    query: SELECT station, DATE(ts) AS day, SUM(volume) AS vol FROM readings GROUP BY station, day
    view: { pivot: { rows: station, cols: day, value: vol, agg: sum, color: true } }
  - kind: table
    title: Latest anomalies
    query: SELECT * FROM anomalies LIMIT 50
```

Decisions baked into that shape:

1. **Panel `view` is the existing `ViewConfig`** (`webui/src/view.ts`) — the
   exact object the results pane already persists per tab. Column refs are by
   name, so a panel keeps working when a query gains/reorders columns; an
   unresolvable name falls back to the view's default. No second config format.
2. **File-backed, not localStorage.** Dashboards are meant to be shared,
   committed, and eventually rendered headlessly; browser-profile storage can
   do none of that. (Tabs stay in localStorage — they are personal scratch
   state.)
3. **Vertical story layout first.** Panels render top-to-bottom in list order;
   `width: half` lets two adjacent panels share a row. Markdown panels make it
   a narrative. No drag-and-drop grid in v1 — order-in-file *is* the layout.
4. **Variables are client-side text substitution** of `{{name}}` before the
   query is POSTed, with values from a per-dashboard toolbar (a dropdown when
   `options_query` is set, a text input otherwise). Values are substituted as
   SQL string literals with quote-escaping (`'` → `''`); `{{name:raw}}` opts
   out for interval/identifier positions. This mirrors the `${ENV}` config
   idiom users already know. Server-side parameterization can come later
   without changing the file format.
5. **Panels compose with views/matviews.** The intended pattern for expensive
   multi-source joins: `CREATE MATERIALIZED VIEW` once, point several panels at
   it. `REFRESH MATERIALIZED VIEW` then updates the whole dashboard from one
   consistent snapshot — which is the matview roadmap's motivating use case
   paying off. Regular views give the always-fresh variant.
6. **`stat` panel** renders the first cell of the first row, big, with the
   column name as its caption (and, later, an optional delta vs. a second
   column). It's the only panel kind without a `view` config.

## Server API

`serve.go` gains a small CRUD surface, same conventions as `/api/sources`:

- `GET /api/dashboards` — list `{slug, name, description}` (scan the dir).
- `GET /api/dashboards/{slug}` — the parsed dashboard (YAML → JSON).
- `POST /api/dashboards` — create/update (JSON in, written as YAML via
  `yaml.Node` round-trip like `config.AppendSource`, preserving hand edits
  where practical; slug derived from name, sanitized like `createUpload`).
- `DELETE /api/dashboards/{slug}`.

The server does **not** execute dashboards; the client runs each panel's query
through the existing `POST /api/query` (row caps, error shape, and session
statements all behave identically to a tab). A future headless
`turntable dashboard render <slug>` reuses the same file + planner directly.

## Frontend

- **Dashboard view**: a new top-level mode (sidebar entry listing dashboards;
  selecting one replaces the tab workspace with the panel list). Each panel
  fetches on mount, shows its title + status line (rows · ms), and renders via
  the *existing* components: `Results`' table for `table`, `Chart` for `chart`,
  `PivotTable` for `pivot` — passing the panel's `view` as the (read-only)
  config. A markdown panel renders sanitized markdown (small dep, e.g.
  `marked` + DOMPurify, or a minimal in-house subset).
- **Variables toolbar** at the top; changing a value re-runs affected panels
  (those whose query mentions the variable).
- **Refresh**: a manual "run all" button in v1; an optional per-dashboard
  `refresh: 30s` interval later.
- **Authoring = "pin to dashboard"**: a button in the results bar of any tab
  opens a picker (existing dashboard or new), appends
  `{kind: <current mode>, title: <tab name>, query, view}` to it via the API.
  Editing beyond that (reorder, retitle, markdown) is v1-acceptable to do by
  editing the YAML; an in-app panel editor is a fast follow.

## Later (explicitly out of v1)

- Grid layout / drag-and-drop; panel-level time-range picker synced across
  panels (variables cover this in v1 via `{{range}}`).
- Headless rendering: `turntable dashboard render <slug> --out report.html`
  producing a static, self-contained HTML report (charts pre-rendered via the
  same Chart.js configs), turning a story into an emailable artifact.
- Server-side scheduled refresh of matviews feeding a dashboard.

## Sequencing

1. YAML schema + load/save + `GET`/`POST` endpoints, with tests mirroring
   `config`'s round-trip tests.
2. Read-only dashboard rendering in the web UI (hand-written YAML).
3. Variables toolbar.
4. "Pin to dashboard" authoring.
