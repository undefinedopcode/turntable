// Package config loads and validates the Turntable source configuration
// (turntable.yaml). The config declares named sources mapping to connectors.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// envVarPattern matches ${VAR} or ${VAR:-default}.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// envRefPattern matches a value that is *exactly* a single ${VAR} or
// ${VAR:-default} reference — used to require that a sensitive field carries an
// environment-variable reference rather than a literal secret.
var envRefPattern = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`)

// File is the top-level config structure.
type File struct {
	Sources  map[string]Source `yaml:"sources"`
	Defaults Defaults          `yaml:"defaults"`
}

// Source declares one named queryable source.
type Source struct {
	Connector string `yaml:"connector"`
	Path      string `yaml:"path,omitempty"`
	URL       string `yaml:"url,omitempty"`
	DSN       string `yaml:"dsn,omitempty"`
	Driver    string `yaml:"driver,omitempty"`
	Table     string `yaml:"table,omitempty"`
	Sheet     string `yaml:"sheet,omitempty"`
	Delimiter string `yaml:"delimiter,omitempty"`
	// Command is the executable + args for a `plugin` connector (an external
	// program speaking the stdio JSON-RPC protocol; see PLUGINS.md).
	Command []string       `yaml:"command,omitempty"`
	Options map[string]any `yaml:"options,omitempty"`
}

// Defaults holds default CLI behavior overrides.
type Defaults struct {
	Output  string `yaml:"output,omitempty"`
	MaxRows int    `yaml:"max-rows,omitempty"`
}

// Load reads a config file from path. If path is empty and no default file
// exists, an empty config is returned (sources resolved via flags/qualified
// refs).
func Load(path string) (*File, error) {
	if path == "" {
		// default location
		if _, err := os.Stat("turntable.yaml"); err == nil {
			path = "turntable.yaml"
		} else {
			return &File{Sources: map[string]Source{}}, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes YAML config bytes and interpolates environment variables in
// string fields (DSNs, paths, etc.).
func Parse(data []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if f.Sources == nil {
		f.Sources = map[string]Source{}
	}
	for name, src := range f.Sources {
		f.Sources[name] = InterpolateSource(src)
	}
	return &f, nil
}

// InterpolateSource returns a copy of src with ${ENV_VAR} references in its
// string fields and options resolved from the environment. Connector options
// often carry secrets (API keys, tokens) that should come from the environment,
// so string-valued options are interpolated too. The input is not mutated (the
// Options map is copied), so a caller can keep the declared form for persistence.
func InterpolateSource(src Source) Source {
	src.Path = interpolate(src.Path)
	src.URL = interpolate(src.URL)
	src.DSN = interpolate(src.DSN)
	src.Driver = interpolate(src.Driver)
	src.Table = interpolate(src.Table)
	src.Sheet = interpolate(src.Sheet)
	if len(src.Command) > 0 {
		cmd := make([]string, len(src.Command))
		for i, a := range src.Command {
			cmd[i] = interpolate(a)
		}
		src.Command = cmd
	}
	if src.Options != nil {
		opts := make(map[string]any, len(src.Options))
		for k, v := range src.Options {
			if s, ok := v.(string); ok {
				opts[k] = interpolate(s)
			} else {
				opts[k] = v
			}
		}
		src.Options = opts
	}
	return src
}

// interpolate replaces ${VAR} and ${VAR:-default} placeholders.
func interpolate(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[2 : len(match)-1]
		var key, def string
		if i := os.Getenv("TURNTABLE_" + inner); i != "" {
			return i
		}
		if pos := 0; pos >= 0 {
			// split key:-default
			for i := 0; i < len(inner)-1; i++ {
				if inner[i:i+2] == ":-" {
					key = inner[:i]
					def = inner[i+2:]
					break
				}
			}
		}
		if key == "" {
			key = inner
		}
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	})
}

// sensitiveOptionFields lists, per connector, the option keys whose values are
// credentials and so must be an ${ENV_VAR} reference rather than a literal. The
// SQL "dsn" field is handled specially in IsSensitive (exempt for sqlite, whose
// DSN is just a local path). Keys match the web form / .use field names.
var sensitiveOptionFields = map[string][]string{
	"http":        {"bearer"},
	"linear":      {"api_key", "bearer"},
	"trello":      {"key", "token"},
	"azuredevops": {"pat"},
	"azuretables": {"connection_string"},
	"honeycomb":   {"api_key", "management_key"},
}

// IsSensitive reports whether a field of src (by its form/option key) holds a
// credential that must come from the environment, not a literal.
func IsSensitive(src Source, field string) bool {
	if src.Connector == "sql" && field == "dsn" {
		// A non-sqlite DSN embeds a password; sqlite's "dsn" is a local file path.
		return !strings.EqualFold(src.Driver, "sqlite")
	}
	for _, f := range sensitiveOptionFields[src.Connector] {
		if strings.EqualFold(f, field) {
			return true
		}
	}
	return false
}

// IsEnvRef reports whether v is exactly an ${ENV_VAR} / ${ENV_VAR:-default}
// reference (the required form for a sensitive field).
func IsEnvRef(v string) bool {
	return envRefPattern.MatchString(v)
}

// ValidateSourceSecrets checks that every sensitive field of src is an
// environment-variable reference rather than a literal — so credentials stay out
// of the config file (and out of anything persisted from it). It runs on the
// declared form (before interpolation), e.g. when a source is added via .use or
// the web add-source dialog.
func ValidateSourceSecrets(src Source) error {
	check := func(field, val string) error {
		if val == "" || !IsSensitive(src, field) {
			return nil
		}
		if !IsEnvRef(val) {
			return fmt.Errorf("field %q is sensitive: enter an environment-variable reference like ${MY_SECRET} (set it in your shell or a .env file), not a literal value", field)
		}
		return nil
	}
	if err := check("dsn", src.DSN); err != nil {
		return err
	}
	for k, v := range src.Options {
		if s, ok := v.(string); ok {
			if err := check(k, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadDotEnv reads KEY=VALUE lines from path (default ".env") into the process
// environment, without overriding variables already set (the real environment
// wins). A missing file is a no-op. Lines may be blank, "# comments", or
// KEY=VALUE with optional surrounding quotes on the value.
func LoadDotEnv(path string) error {
	if path == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return nil
}

// AppendSource writes src under `sources:` in the YAML file at path, preserving
// the file's existing comments, formatting, and other entries (via a yaml.Node
// round-trip). The file (and a `sources:` map) is created if absent; an existing
// source of the same name is replaced. src is written in its declared form — so
// when its sensitive fields are ${ENV_VAR} references (enforced by
// ValidateSourceSecrets), no secret reaches disk.
func AppendSource(path, name string, src Source) error {
	if path == "" {
		return fmt.Errorf("no config file in use; start turntable with -c <file> to enable saving sources")
	}
	var root yaml.Node
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %q: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	doc := root.Content[0]

	var sources *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "sources" {
			sources = doc.Content[i+1]
			break
		}
	}
	if sources == nil {
		sources = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "sources"}, sources,
		}, doc.Content...)
	}
	// Force block style: an empty `sources: {}` (or any flow mapping) would
	// otherwise grow inline as `{a: {...}, b: {...}}`.
	sources.Style = 0

	var val yaml.Node
	if err := val.Encode(src); err != nil {
		return err
	}
	replaced := false
	for i := 0; i+1 < len(sources.Content); i += 2 {
		if sources.Content[i].Value == name {
			sources.Content[i+1] = &val
			replaced = true
			break
		}
	}
	if !replaced {
		sources.Content = append(sources.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: name}, &val)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return err
	}
	enc.Close()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
