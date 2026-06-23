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
│   ├── main.tsx        React root
│   ├── App.tsx         layout + top-level state
│   ├── api.ts          typed client for the /api/* endpoints
│   ├── csv.ts          client-side CSV export
│   ├── styles.css      dark theme
│   └── components/     Sidebar, AddSourceForm, Editor, Results
└── dist/               built output (committed; embedded by Go)
```

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
