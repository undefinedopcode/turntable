// Per-connector form specs that drive the Add Source modal. Each connector
// declares the fields its dataset needs; file connectors are marked `file: true`
// and offer an upload (the chosen/uploaded path becomes the `path` field).
//
// Field keys map directly onto the JSON sent to POST /api/sources `fields`
// (which the server routes via applySourceField, same as the REPL .use command).

export interface FieldSpec {
  key: string;
  label: string;
  type?: "text" | "password" | "select";
  options?: string[];
  required?: boolean;
  placeholder?: string;
  help?: string;
  // sensitive fields hold credentials: the UI requires an ${ENV_VAR} reference
  // (validated server-side too), so secrets never land in the config. For sql
  // `dsn` this is waived when the driver is sqlite (a local path) — see the
  // modal's fieldSensitive().
  sensitive?: boolean;
}

export interface ConnectorSpec {
  name: string; // connector prefix (csv, sql, ...)
  label: string; // human label for the picker
  file?: boolean; // file connector: offer upload, path is supplied by it
  fileExt?: string; // accept filter for the file input
  fields: FieldSpec[]; // extra option fields beyond the file/path
  note?: string;
}

export const CONNECTOR_SPECS: ConnectorSpec[] = [
  {
    name: "csv",
    label: "CSV file",
    file: true,
    fileExt: ".csv,.tsv,.txt",
    fields: [
      { key: "delimiter", label: "Delimiter", placeholder: "," },
    ],
  },
  { name: "json", label: "JSON file", file: true, fileExt: ".json,.ndjson", fields: [] },
  { name: "yaml", label: "YAML file", file: true, fileExt: ".yaml,.yml", fields: [] },
  {
    name: "excel",
    label: "Excel workbook",
    file: true,
    fileExt: ".xlsx",
    fields: [
      { key: "sheet", label: "Sheet", placeholder: "Sheet1 (or * for all sheets)" },
    ],
  },
  { name: "parquet", label: "Parquet file", file: true, fileExt: ".parquet", fields: [] },
  {
    name: "log",
    label: "Log file (auto-detect)",
    file: true,
    fileExt: ".log,.txt,.json,.jsonl",
    fields: [
      {
        key: "format",
        label: "Format",
        type: "select",
        options: ["auto", "json", "logfmt", "clf", "combined", "syslog", "bracketed", "leveled", "raw"],
      },
      { key: "pattern", label: "Custom pattern", placeholder: "regex with (?P<name>…) groups (overrides format)" },
    ],
  },
  {
    name: "sql",
    label: "SQL database",
    fields: [
      { key: "driver", label: "Driver", type: "select", options: ["sqlite", "postgres", "mysql", "sqlserver"], required: true },
      { key: "dsn", label: "DSN", required: true, sensitive: true, placeholder: "sqlite: ./data.db  |  others: ${DB_DSN}" },
      { key: "table", label: "Table", placeholder: "table name (or * for every table)" },
    ],
  },
  {
    name: "http",
    label: "HTTP / REST (JSON)",
    fields: [
      { key: "url", label: "URL", required: true, placeholder: "https://api.example.com/items" },
      { key: "path", label: "JSON path", placeholder: "data.items (dotted path to the array)" },
      { key: "bearer", label: "Bearer token", sensitive: true, placeholder: "${API_TOKEN}" },
      { key: "method", label: "Method", placeholder: "GET" },
    ],
  },
  {
    name: "linear",
    label: "Linear",
    fields: [
      { key: "dataset", label: "Dataset", type: "select", options: ["issues", "teams", "projects", "users"], required: true },
      { key: "api_key", label: "API key", sensitive: true, placeholder: "${LINEAR_API_KEY}", help: "or use a Bearer token below" },
      { key: "bearer", label: "OAuth token", sensitive: true, placeholder: "${LINEAR_TOKEN}" },
    ],
  },
  {
    name: "trello",
    label: "Trello",
    fields: [
      { key: "dataset", label: "Dataset", type: "select", options: ["boards", "lists", "cards", "members"], required: true },
      { key: "key", label: "API key", sensitive: true, required: true, placeholder: "${TRELLO_KEY}" },
      { key: "token", label: "API token", sensitive: true, required: true, placeholder: "${TRELLO_TOKEN}" },
      { key: "board", label: "Board id", help: "required for lists/cards/members" },
    ],
  },
  {
    name: "azuredevops",
    label: "Azure DevOps Boards",
    fields: [
      { key: "organization", label: "Organization", required: true, placeholder: "myorg (slug or full dev.azure.com URL)" },
      { key: "project", label: "Project", required: true, placeholder: "My Project" },
      { key: "pat", label: "Personal access token", sensitive: true, required: true, placeholder: "${AZDO_PAT}" },
      { key: "type", label: "Work item type", placeholder: "Bug, User Story, … (optional filter)" },
      { key: "wiql", label: "WIQL override", placeholder: "SELECT [System.Id] FROM workitems WHERE …" },
    ],
  },
  {
    name: "honeycomb",
    label: "Honeycomb",
    fields: [
      { key: "kind", label: "Dataset", type: "select", options: ["events", "datasets", "columns", "environments"], required: true, help: "events = query one dataset; datasets/columns/environments = metadata" },
      { key: "dataset", label: "Honeycomb dataset slug", placeholder: "my-service (or * for every dataset)", help: "required for events and columns" },
      { key: "api_key", label: "API key", sensitive: true, placeholder: "${HONEYCOMB_API_KEY}", help: "Configuration key (datasets/columns/events)" },
      { key: "management_key", label: "Management key", sensitive: true, placeholder: "${HONEYCOMB_MGMT_KEY}", help: "keyID:secret, for environments" },
      { key: "team", label: "Team slug", help: "required for environments" },
      { key: "region", label: "Region", type: "select", options: ["us", "eu"] },
      { key: "time_range", label: "Time range (s)", placeholder: "7200", help: "events query window; default 2h" },
    ],
    note: "events supports only aggregate queries (GROUP BY / COUNT / SUM / …) — Honeycomb has no raw-event read API.",
  },
  {
    name: "azmetrics",
    label: "Azure Monitor Metrics",
    fields: [
      { key: "resource", label: "Resource ID", required: true, placeholder: "/subscriptions/…/providers/…/managedClusters/aks1", help: "full ARM resource ID" },
      { key: "metric", label: "Metric(s)", required: true, placeholder: "Percentage CPU (comma-separated)" },
      { key: "aggregation", label: "Aggregation", type: "select", options: ["Average", "Total", "Minimum", "Maximum", "Count"] },
      { key: "interval", label: "Interval", placeholder: "PT5M (ISO-8601 duration)" },
      { key: "timespan", label: "Timespan", placeholder: "startISO/endISO (default: last hour)" },
      { key: "dimension", label: "Split by dimension", placeholder: "node (comma-separated; adds columns)" },
      { key: "namespace", label: "Metric namespace", placeholder: "(optional)" },
    ],
    note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
  },
  {
    name: "azrgraph",
    label: "Azure Resource Graph",
    fields: [
      { key: "table", label: "Table", placeholder: "Resources (default)", help: "Resources, ResourceContainers, …" },
      { key: "subscriptions", label: "Subscriptions", placeholder: "sub-id-1, sub-id-2 (default: all)" },
      { key: "query", label: "Raw KQL", placeholder: "Resources | where … | project … (overrides table)" },
      { key: "top", label: "Row cap", placeholder: "5000" },
    ],
    note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
  },
  {
    name: "azlogs",
    label: "Azure Monitor Logs",
    fields: [
      { key: "workspace", label: "Workspace ID", required: true, placeholder: "Log Analytics workspace GUID" },
      { key: "table", label: "Table", placeholder: "ContainerLogV2, AppRequests, …" },
      { key: "query", label: "Raw KQL", placeholder: "AppRequests | summarize … (overrides table)" },
      { key: "timespan", label: "Timespan", placeholder: "P1D (ISO-8601 duration or start/end)" },
      { key: "top", label: "Row cap", placeholder: "30000" },
    ],
    note: "Auth is ambient via Azure AD (env / managed identity / az login) — no key fields.",
  },
  {
    name: "dynamodb",
    label: "DynamoDB",
    fields: [
      { key: "table", label: "Table", required: true, placeholder: "table name (or * for every table)" },
      { key: "region", label: "Region", placeholder: "us-east-1" },
      { key: "endpoint", label: "Endpoint", placeholder: "http://localhost:8000 (DynamoDB Local)" },
    ],
  },
  {
    name: "athena",
    label: "AWS Athena",
    fields: [
      { key: "table", label: "Table", required: true, placeholder: "table (or db.table, or * for every table)" },
      { key: "database", label: "Database", placeholder: "Glue database (default: default)" },
      { key: "output_location", label: "Output location", placeholder: "s3://bucket/prefix/ (required unless workgroup sets one)" },
      { key: "region", label: "Region", placeholder: "us-east-1" },
      { key: "workgroup", label: "Workgroup", placeholder: "primary" },
      { key: "catalog", label: "Catalog", placeholder: "AwsDataCatalog" },
    ],
  },
  {
    name: "azuretables",
    label: "Azure Table Storage",
    fields: [
      { key: "table", label: "Table", required: true, placeholder: "table name (or * for every table)" },
      { key: "connection_string", label: "Connection string", sensitive: true, placeholder: "${AZURE_TABLES_CONN}", help: "or set account/endpoint for Azure AD auth" },
      { key: "account", label: "Account", placeholder: "storage account name (Azure AD)" },
      { key: "endpoint", label: "Endpoint", placeholder: "override / Azurite" },
    ],
  },
  {
    name: "cloudwatchlogs",
    label: "CloudWatch Logs",
    fields: [
      { key: "region", label: "Region", required: true, placeholder: "us-east-1" },
      { key: "log_group", label: "Log group", required: true, placeholder: "/aws/lambda/my-fn" },
      { key: "filter", label: "Filter pattern" },
      { key: "start", label: "Start", placeholder: "RFC3339 or unix millis" },
      { key: "end", label: "End", placeholder: "RFC3339 or unix millis" },
    ],
  },
  {
    name: "claudelogs",
    label: "Claude Code transcripts",
    fields: [
      { key: "kind", label: "View", type: "select", options: ["messages", "tools", "tool_results"] },
      { key: "path", label: "Path", placeholder: ".jsonl file or dir (default: current project)" },
      { key: "project", label: "Project", placeholder: "project slug or path (optional)" },
    ],
  },
  {
    name: "cloudwatch",
    label: "CloudWatch Metrics",
    fields: [
      { key: "region", label: "Region", required: true, placeholder: "us-east-1" },
      { key: "namespace", label: "Namespace", required: true, placeholder: "AWS/EC2" },
      { key: "metric", label: "Metric", required: true, placeholder: "CPUUtilization" },
      { key: "stat", label: "Statistic", placeholder: "Average" },
      { key: "period", label: "Period (s)", placeholder: "300" },
    ],
  },
];

export function specFor(name: string): ConnectorSpec | undefined {
  return CONNECTOR_SPECS.find((c) => c.name === name);
}
