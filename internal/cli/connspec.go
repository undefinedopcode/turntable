package cli

// Per-connector field specs: which fields each connector's source needs, which
// are required or sensitive, and which connectors are file-based. This is the
// single source of truth — the web add-source modal fetches it from
// GET /api/connectors (webui/src/connectorSpecs.ts is just the fetch + types),
// the MCP list_connectors tool serializes it, and isFileConnector (repl.go)
// derives from the File flag.
//
// Field keys map onto applySourceField routing (same as the REPL .use command
// and POST /api/sources fields). The `plugin` connector is deliberately absent:
// its command field is arbitrary exec, so runtime source adds never expose it —
// declare plugin sources in turntable.yaml instead.

// FieldSpec describes one add-source form field / add_source tool field.
type FieldSpec struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Type is "text" (default), "password", or "select" (see Options).
	Type    string   `json:"type,omitempty"`
	Options []string `json:"options,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	// Sensitive fields hold credentials and must be given as a sole ${ENV_VAR}
	// reference, never a literal — enforced by config.ValidateSourceSecrets.
	// For sql `dsn` this is waived when the driver is sqlite (a local path).
	Sensitive bool `json:"sensitive,omitempty"`
}

// ConnectorSpec describes one connector for runtime source registration.
type ConnectorSpec struct {
	Name  string `json:"name"`  // connector prefix (csv, sql, …)
	Label string `json:"label"` // human label for pickers
	// File connectors locate data by a local path: the web UI offers an upload
	// and routes `path` to config.Source.Path (see isFileConnector).
	File    bool        `json:"file,omitempty"`
	FileExt string      `json:"fileExt,omitempty"` // accept filter for the file input
	Fields  []FieldSpec `json:"fields"`
	Note    string      `json:"note,omitempty"`
}

func connectorSpecFor(name string) *ConnectorSpec {
	for i := range connectorSpecs {
		if connectorSpecs[i].Name == name {
			return &connectorSpecs[i]
		}
	}
	return nil
}

var connectorSpecs = []ConnectorSpec{
	{
		Name: "csv", Label: "CSV file", File: true, FileExt: ".csv,.tsv,.txt",
		Fields: []FieldSpec{
			{Key: "delimiter", Label: "Delimiter", Placeholder: ","},
		},
	},
	{Name: "json", Label: "JSON file", File: true, FileExt: ".json,.ndjson", Fields: []FieldSpec{}},
	{Name: "yaml", Label: "YAML file", File: true, FileExt: ".yaml,.yml", Fields: []FieldSpec{}},
	{
		Name: "excel", Label: "Excel workbook", File: true, FileExt: ".xlsx",
		Fields: []FieldSpec{
			{Key: "sheet", Label: "Sheet", Placeholder: "Sheet1 (or * for all sheets)"},
		},
	},
	{Name: "parquet", Label: "Parquet file", File: true, FileExt: ".parquet", Fields: []FieldSpec{}},
	{
		Name: "log", Label: "Log file (auto-detect)", File: true, FileExt: ".log,.txt,.json,.jsonl",
		Fields: []FieldSpec{
			{Key: "format", Label: "Format", Type: "select",
				Options: []string{"auto", "json", "logfmt", "clf", "combined", "syslog", "bracketed", "leveled", "raw"}},
			{Key: "pattern", Label: "Custom pattern", Placeholder: "regex with (?P<name>…) groups (overrides format)"},
		},
	},
	{
		Name: "sql", Label: "SQL database",
		Fields: []FieldSpec{
			{Key: "driver", Label: "Driver", Type: "select", Options: []string{"sqlite", "postgres", "mysql", "sqlserver"}, Required: true},
			{Key: "dsn", Label: "DSN", Required: true, Sensitive: true, Placeholder: "sqlite: ./data.db  |  others: ${DB_DSN}"},
			{Key: "table", Label: "Table", Placeholder: "table name (or * for every table)"},
		},
	},
	{
		Name: "http", Label: "HTTP / REST (JSON)",
		Fields: []FieldSpec{
			{Key: "url", Label: "URL", Required: true, Placeholder: "https://api.example.com/items"},
			{Key: "path", Label: "JSON path", Placeholder: "data.items (dotted path to the array)"},
			{Key: "bearer", Label: "Bearer token", Sensitive: true, Placeholder: "${API_TOKEN}"},
			{Key: "method", Label: "Method", Placeholder: "GET"},
		},
	},
	{
		Name: "linear", Label: "Linear",
		Fields: []FieldSpec{
			{Key: "dataset", Label: "Dataset", Type: "select", Options: []string{"issues", "teams", "projects", "users"}, Required: true},
			{Key: "api_key", Label: "API key", Sensitive: true, Placeholder: "${LINEAR_API_KEY}", Help: "or use a Bearer token below"},
			{Key: "bearer", Label: "OAuth token", Sensitive: true, Placeholder: "${LINEAR_TOKEN}"},
		},
	},
	{
		Name: "trello", Label: "Trello",
		Fields: []FieldSpec{
			{Key: "dataset", Label: "Dataset", Type: "select", Options: []string{"boards", "lists", "cards", "members"}, Required: true},
			{Key: "key", Label: "API key", Sensitive: true, Required: true, Placeholder: "${TRELLO_KEY}"},
			{Key: "token", Label: "API token", Sensitive: true, Required: true, Placeholder: "${TRELLO_TOKEN}"},
			{Key: "board", Label: "Board id", Help: "required for lists/cards/members"},
		},
	},
	{
		Name: "azuredevops", Label: "Azure DevOps Boards",
		Fields: []FieldSpec{
			{Key: "organization", Label: "Organization", Required: true, Placeholder: "myorg (slug or full dev.azure.com URL)"},
			{Key: "project", Label: "Project", Required: true, Placeholder: "My Project"},
			{Key: "pat", Label: "Personal access token", Sensitive: true, Required: true, Placeholder: "${AZDO_PAT}"},
			{Key: "type", Label: "Work item type", Placeholder: "Bug, User Story, … (optional filter)"},
			{Key: "wiql", Label: "WIQL override", Placeholder: "SELECT [System.Id] FROM workitems WHERE …"},
		},
	},
	{
		Name: "honeycomb", Label: "Honeycomb",
		Fields: []FieldSpec{
			{Key: "kind", Label: "Dataset", Type: "select", Options: []string{"events", "datasets", "columns", "environments"}, Required: true,
				Help: "events = query one dataset; datasets/columns/environments = metadata"},
			{Key: "dataset", Label: "Honeycomb dataset slug", Placeholder: "my-service (or * for every dataset)", Help: "required for events and columns"},
			{Key: "api_key", Label: "API key", Sensitive: true, Placeholder: "${HONEYCOMB_API_KEY}", Help: "Configuration key (datasets/columns/events)"},
			{Key: "management_key", Label: "Management key", Sensitive: true, Placeholder: "${HONEYCOMB_MGMT_KEY}", Help: "keyID:secret, for environments"},
			{Key: "team", Label: "Team slug", Help: "required for environments"},
			{Key: "region", Label: "Region", Type: "select", Options: []string{"us", "eu"}},
			{Key: "time_range", Label: "Time range (s)", Placeholder: "7200", Help: "events query window; default 2h"},
		},
		Note: "events supports only aggregate queries (GROUP BY / COUNT / SUM / …) — Honeycomb has no raw-event read API.",
	},
	{
		Name: "prom", Label: "Prometheus",
		Fields: []FieldSpec{
			{Key: "url", Label: "Server URL", Required: true, Placeholder: "http://localhost:9090"},
			{Key: "metric", Label: "Metric", Placeholder: "node_cpu_seconds_total", Help: "a plain metric selector (or use Query)"},
			{Key: "query", Label: "Query (PromQL)", Placeholder: "rate(http_requests_total[5m])", Help: "takes precedence over Metric"},
			{Key: "time_range", Label: "Time range (s)", Placeholder: "3600", Help: "lookback window; default 1h"},
			{Key: "step", Label: "Step (s)", Placeholder: "auto", Help: "sample resolution; default ~250 points across the window"},
			{Key: "bearer", Label: "Bearer token", Sensitive: true, Placeholder: "${PROM_TOKEN}", Help: "optional Authorization: Bearer"},
		},
		Note: "rows are (ts, one column per label, value) — one row per series sample. Reduce at the source with PromQL (rate, sum by (…)).",
	},
	{
		Name: "azmetrics", Label: "Azure Monitor Metrics",
		Fields: []FieldSpec{
			{Key: "resource", Label: "Resource ID", Placeholder: "/subscriptions/…/managedClusters/aks1", Help: "one resource (or use Resources for a batch)"},
			{Key: "resources", Label: "Resources (batch)", Placeholder: "id1, id2, … (≤50/call, same region+subscription)", Help: "many resources in one query; requires Region"},
			{Key: "region", Label: "Region", Placeholder: "eastus (required for batch)"},
			{Key: "metric", Label: "Metric(s)", Required: true, Placeholder: "Percentage CPU (comma-separated)"},
			{Key: "aggregation", Label: "Aggregation", Type: "select", Options: []string{"Average", "Total", "Minimum", "Maximum", "Count"}},
			{Key: "interval", Label: "Interval", Placeholder: "PT5M (ISO-8601 duration)"},
			{Key: "timespan", Label: "Timespan", Placeholder: "startISO/endISO (default: last hour)"},
			{Key: "dimension", Label: "Split by dimension", Placeholder: "node (comma-separated; adds columns)"},
			{Key: "namespace", Label: "Metric namespace", Placeholder: "(optional)"},
		},
		Note: "One of Resource or Resources (batch) is required. Auth is ambient via Azure AD — no key fields.",
	},
	{
		Name: "azrgraph", Label: "Azure Resource Graph",
		Fields: []FieldSpec{
			{Key: "table", Label: "Table", Placeholder: "Resources (default)", Help: "Resources, ResourceContainers, …"},
			{Key: "subscriptions", Label: "Subscriptions", Placeholder: "sub-id-1, sub-id-2 (default: all)"},
			{Key: "query", Label: "Raw KQL", Placeholder: "Resources | where … | project … (overrides table)"},
			{Key: "top", Label: "Row cap", Placeholder: "5000"},
		},
		Note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
	},
	{
		Name: "azlogs", Label: "Azure Monitor Logs",
		Fields: []FieldSpec{
			{Key: "workspace", Label: "Workspace ID", Required: true, Placeholder: "Log Analytics workspace GUID"},
			{Key: "table", Label: "Table", Placeholder: "ContainerLogV2, AppRequests, …"},
			{Key: "query", Label: "Raw KQL", Placeholder: "AppRequests | summarize … (overrides table)"},
			{Key: "timespan", Label: "Timespan", Placeholder: "P1D (ISO-8601 duration or start/end)"},
			{Key: "top", Label: "Row cap", Placeholder: "30000"},
		},
		Note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
	},
	{
		Name: "awsconfig", Label: "AWS Config (inventory)",
		Fields: []FieldSpec{
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "profile", Label: "Profile", Placeholder: "shared-config profile (optional)"},
			{Key: "aggregator", Label: "Aggregator", Placeholder: "config aggregator name (multi-account)"},
			{Key: "query", Label: "Raw Config SQL", Placeholder: "SELECT resourceId, resourceType WHERE … (overrides table)"},
			{Key: "top", Label: "Row cap", Placeholder: "5000"},
		},
		Note: "Requires AWS Config enabled/recording. Auth is ambient via the AWS SDK (env / profile / role).",
	},
	{
		Name: "awscost", Label: "AWS Cost Explorer",
		Fields: []FieldSpec{
			{Key: "granularity", Label: "Granularity", Type: "select", Options: []string{"DAILY", "MONTHLY", "HOURLY"}},
			{Key: "metric", Label: "Metric(s)", Placeholder: "UnblendedCost (comma-separated)"},
			{Key: "group_by", Label: "Group by", Placeholder: "SERVICE, REGION, TAG:env (≤2)"},
			{Key: "start", Label: "Start", Placeholder: "YYYY-MM-DD (default: 30d ago)"},
			{Key: "end", Label: "End", Placeholder: "YYYY-MM-DD (exclusive; default: today)"},
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "profile", Label: "Profile", Placeholder: "shared-config profile (optional)"},
		},
		Note: "Auth is ambient via the AWS SDK (env / profile / role).",
	},
	{
		Name: "azcost", Label: "Azure Cost Management",
		Fields: []FieldSpec{
			{Key: "subscription", Label: "Subscription", Placeholder: "subscription id (or use Scope)"},
			{Key: "scope", Label: "Scope", Placeholder: "full ARM scope (management group / billing account)"},
			{Key: "metric", Label: "Metric", Placeholder: "Cost (or PreTaxCost)"},
			{Key: "group_by", Label: "Group by", Placeholder: "ServiceName, ResourceGroup, TAG:env"},
			{Key: "granularity", Label: "Granularity", Type: "select", Options: []string{"None", "Daily"}},
			{Key: "timeframe", Label: "Timeframe", Placeholder: "MonthToDate / TheLastMonth / Custom"},
			{Key: "start", Label: "Start", Placeholder: "YYYY-MM-DD (sets Custom)"},
			{Key: "end", Label: "End", Placeholder: "YYYY-MM-DD"},
		},
		Note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
	},
	{
		Name: "dynamodb", Label: "DynamoDB",
		Fields: []FieldSpec{
			{Key: "table", Label: "Table", Required: true, Placeholder: "table name (or * for every table)"},
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "endpoint", Label: "Endpoint", Placeholder: "http://localhost:8000 (DynamoDB Local)"},
		},
	},
	{
		Name: "athena", Label: "AWS Athena",
		Fields: []FieldSpec{
			{Key: "table", Label: "Table", Required: true, Placeholder: "table (or db.table, or * for every table)"},
			{Key: "database", Label: "Database", Placeholder: "Glue database (default: default)"},
			{Key: "output_location", Label: "Output location", Placeholder: "s3://bucket/prefix/ (required unless workgroup sets one)"},
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "workgroup", Label: "Workgroup", Placeholder: "primary"},
			{Key: "catalog", Label: "Catalog", Placeholder: "AwsDataCatalog"},
		},
	},
	{
		Name: "azuretables", Label: "Azure Table Storage",
		Fields: []FieldSpec{
			{Key: "table", Label: "Table", Required: true, Placeholder: "table name (or * for every table)"},
			{Key: "connection_string", Label: "Connection string", Sensitive: true, Placeholder: "${AZURE_TABLES_CONN}", Help: "or set account/endpoint for Azure AD auth"},
			{Key: "account", Label: "Account", Placeholder: "storage account name (Azure AD)"},
			{Key: "endpoint", Label: "Endpoint", Placeholder: "override / Azurite"},
		},
	},
	{
		Name: "cloudwatchlogs", Label: "CloudWatch Logs",
		Fields: []FieldSpec{
			{Key: "region", Label: "Region", Required: true, Placeholder: "us-east-1"},
			{Key: "log_group", Label: "Log group", Required: true, Placeholder: "/aws/lambda/my-fn"},
			{Key: "filter", Label: "Filter pattern"},
			{Key: "start", Label: "Start", Placeholder: "RFC3339 or unix millis"},
			{Key: "end", Label: "End", Placeholder: "RFC3339 or unix millis"},
		},
	},
	{
		Name: "claudelogs", Label: "Claude Code transcripts",
		Fields: []FieldSpec{
			{Key: "kind", Label: "View", Type: "select", Options: []string{"messages", "tools", "tool_results"}},
			{Key: "path", Label: "Path", Placeholder: ".jsonl file or dir (default: current project)"},
			{Key: "project", Label: "Project", Placeholder: "project slug or path (optional)"},
		},
	},
	{
		Name: "cloudwatch", Label: "CloudWatch Metrics",
		Fields: []FieldSpec{
			{Key: "region", Label: "Region", Required: true, Placeholder: "us-east-1"},
			{Key: "namespace", Label: "Namespace", Required: true, Placeholder: "AWS/EC2"},
			{Key: "metric", Label: "Metric", Required: true, Placeholder: "CPUUtilization"},
			{Key: "stat", Label: "Statistic", Placeholder: "Average"},
			{Key: "period", Label: "Period (s)", Placeholder: "300"},
		},
	},
}
