# OctoParser

Query heterogeneous data sources — JSON, CSV, YAML, SQL databases, and (later)
CloudWatch, Prometheus, REST APIs — using a single SQL-style query language.

> **Status:** early scaffold. The architecture and supported dialect are
> defined in [DESIGN.md](./DESIGN.md). This repository currently contains the
> package skeleton and stub implementations; the v0.1 milestone implements
> the JSON/CSV/YAML connectors and the in-memory engine.

## Install

```bash
go build ./cmd/octoparser
```

## Usage

```bash
# Query a registered source (see examples/octoparser.yaml)
octoparser 'SELECT region, COUNT(*) AS n FROM sales
            WHERE amount > 100 GROUP BY region ORDER BY n DESC LIMIT 10'

# Qualified inline source (no config needed)
octoparser 'SELECT * FROM csv:./events.csv LIMIT 5'

# Explain the plan (pushdown per connector) instead of running
octoparser --explain 'SELECT * FROM users'

# Choose an output format
octoparser -o json 'SELECT * FROM users'
```

## Layout

```
DESIGN.md            Architecture and SQL dialect
cmd/octoparser/      CLI entrypoint
internal/cli         flag handling, wiring, REPL
internal/sql         lexer, parser, AST
internal/plan        resolution, validation, pushdown
internal/engine      types, rows, operator pipeline
internal/connector   Connector interface + Registry
internal/connector/connectors/{jsonc,csvc,yamlc,sqlc}
internal/render       output formatters
internal/config       octoparser.yaml loader
examples/             sample config
```

See [DESIGN.md](./DESIGN.md) for the full design, roadmap, and extension model.