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
      { key: "driver", label: "Driver", type: "select", options: ["sqlite", "postgres", "mysql"], required: true },
      { key: "dsn", label: "DSN", required: true, placeholder: "./data.db  |  postgres://user:pass@host/db" },
      { key: "table", label: "Table", placeholder: "table name (or * for every table)" },
    ],
  },
  {
    name: "http",
    label: "HTTP / REST (JSON)",
    fields: [
      { key: "url", label: "URL", required: true, placeholder: "https://api.example.com/items" },
      { key: "path", label: "JSON path", placeholder: "data.items (dotted path to the array)" },
      { key: "bearer", label: "Bearer token", type: "password" },
      { key: "method", label: "Method", placeholder: "GET" },
    ],
  },
  {
    name: "linear",
    label: "Linear",
    fields: [
      { key: "dataset", label: "Dataset", type: "select", options: ["issues", "teams", "projects", "users"], required: true },
      { key: "api_key", label: "API key", type: "password", help: "or use a Bearer token below" },
      { key: "bearer", label: "OAuth token", type: "password" },
    ],
  },
  {
    name: "trello",
    label: "Trello",
    fields: [
      { key: "dataset", label: "Dataset", type: "select", options: ["boards", "lists", "cards", "members"], required: true },
      { key: "key", label: "API key", type: "password", required: true },
      { key: "token", label: "API token", type: "password", required: true },
      { key: "board", label: "Board id", help: "required for lists/cards/members" },
    ],
  },
  {
    name: "azuredevops",
    label: "Azure DevOps Boards",
    fields: [
      { key: "organization", label: "Organization", required: true, placeholder: "myorg (slug or full dev.azure.com URL)" },
      { key: "project", label: "Project", required: true, placeholder: "My Project" },
      { key: "pat", label: "Personal access token", type: "password", required: true },
      { key: "type", label: "Work item type", placeholder: "Bug, User Story, … (optional filter)" },
      { key: "wiql", label: "WIQL override", placeholder: "SELECT [System.Id] FROM workitems WHERE …" },
    ],
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
    name: "azuretables",
    label: "Azure Table Storage",
    fields: [
      { key: "table", label: "Table", required: true, placeholder: "table name (or * for every table)" },
      { key: "connection_string", label: "Connection string", type: "password", help: "or set account/endpoint for Azure AD auth" },
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
