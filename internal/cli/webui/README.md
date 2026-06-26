# turntable web UI

The React + Vite single-page app served by `turntable --serve`. It is built to
`dist/`, which is **committed** and embedded into the Go binary via `//go:embed`
in `internal/cli/serve.go` — so `go build ./...` needs no Node toolchain.

## Layout

```
webui/
├── index.html          Vite entry
├── vite.config.ts      base:'./' (relative assets) + dev proxy of /api
├── src/
│   ├── main.tsx          React root
│   ├── App.tsx           layout + top-level state
│   ├── api.ts            typed client for the /api/* endpoints (functions, upload, …)
│   ├── connectorSpecs.ts per-connector form field specs (drives Add Source)
│   ├── export.ts         client-side CSV/JSON/NDJSON/TSV export + clipboard
│   ├── storage.ts        localStorage: query history, saved queries, last query
│   ├── completions.ts    CodeMirror autocomplete (sources, columns, functions)
│   ├── styles.css        dark theme
│   └── components/       Sidebar, Modal, AddSourceModal, Editor (CodeMirror),
│                         Results (sort/filter/export), Chart
└── dist/                 built output (committed; embedded by Go)
```

## Features

- **Editor** — CodeMirror with SQL highlighting and autocomplete over the live
  sources, their columns, and the dialect functions (`GET /api/functions`).
  Ctrl/⌘+Enter runs.
- **Add source → Log file** — picking a log file auto-analyzes it
  (`POST /api/loginfer`): a recognized format shows a parsed preview, while an
  unrecognized one shows **inferred templates** (from `internal/loginfer`) as
  pickable patterns whose columns you can rename inline (rewriting the `pattern`)
  before registering.
- **Sidebar** — source search, per-source schema, one-click **preview**
  (`SELECT * … LIMIT 100`), plus **history** (click to reload) and **saved**
  queries (localStorage). The editor content is restored on reload.
- **Results** — click-to-sort columns, a row filter, click a cell to copy (or
  expand a JSON cell), export to CSV/JSON/NDJSON or copy as TSV, and a **Chart**
  view (category + numeric column → bars).

## Develop

Run the Go API and the Vite dev server side by side. Vite serves the UI with
hot-reload on :5173 and proxies `/api` to the Go server on :8080:

```bash
# terminal 1 — the API (any sources you want to query)
go run ./cmd/turntable --serve -c examples/turntable.yaml

# terminal 2 — the UI with HMR
cd internal/cli/webui
npm install      # first time only
npm run dev      # http://localhost:5173
```

## Build (regenerate the embedded bundle)

After changing anything under `src/`, rebuild and commit `dist/`:

```bash
cd internal/cli/webui
npm run build            # tsc -b && vite build -> dist/
# or, from the repo root:
go generate ./internal/cli
```

Then `go build ./cmd/turntable` embeds the updated UI. `node_modules/` and
`*.tsbuildinfo` are git-ignored; `dist/` is not.
