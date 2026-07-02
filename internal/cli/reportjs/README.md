Vendored Chart.js UMD bundle + date adapter, embedded into `turntable
dashboard render` reports so they are fully self-contained (viewable offline,
emailable). Copied from the webui's node_modules — keep versions in step with
internal/cli/webui/package.json (chart.js is pinned ~4.4 there; see the note
in Chart.tsx).
