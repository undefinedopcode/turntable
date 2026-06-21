// Package config loads and validates the OctoParser source configuration
// (octoparser.yaml). The config declares named sources mapping to connectors.
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// envVarPattern matches ${VAR} or ${VAR:-default}.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// File is the top-level config structure.
type File struct {
	Sources  map[string]Source `yaml:"sources"`
	Defaults Defaults          `yaml:"defaults"`
}

// Source declares one named queryable source.
type Source struct {
	Connector string         `yaml:"connector"`
	Path      string         `yaml:"path,omitempty"`
	DSN       string         `yaml:"dsn,omitempty"`
	Driver    string         `yaml:"driver,omitempty"`
	Table     string         `yaml:"table,omitempty"`
	Sheet     string         `yaml:"sheet,omitempty"`
	Delimiter string         `yaml:"delimiter,omitempty"`
	Options   map[string]any `yaml:"options,omitempty"`
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
		if _, err := os.Stat("octoparser.yaml"); err == nil {
			path = "octoparser.yaml"
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
		src.Path = interpolate(src.Path)
		src.DSN = interpolate(src.DSN)
		src.Driver = interpolate(src.Driver)
		src.Table = interpolate(src.Table)
		src.Sheet = interpolate(src.Sheet)
		f.Sources[name] = src
	}
	return &f, nil
}

// interpolate replaces ${VAR} and ${VAR:-default} placeholders.
func interpolate(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[2 : len(match)-1]
		var key, def string
		if i := os.Getenv("OCTOPARSER_" + inner); i != "" {
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