// Package config loads and validates the OctoParser source configuration
// (octoparser.yaml). The config declares named sources mapping to connectors.
package config

import (
	"fmt"
	"os"
)

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
	Delimiter string         `yaml:"delimiter,omitempty"`
	Options   map[string]any `yaml:",inline"`
}

// Defaults holds default CLI behavior overrides.
type Defaults struct {
	Output  string `yaml:"output,omitempty"`
	MaxRows int    `yaml:"max-rows,omitempty"`
}

// Load reads a config file from path. If path is empty and no default file
// exists, an empty config is returned (sources resolved via flags/qualified
// refs). The actual YAML parsing is implemented in v0.1 once a YAML dependency
// is chosen; this skeleton returns a descriptive error so callers know.
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
	return parse(data)
}

// parse is a stub; real YAML parsing lands in v0.1.
func parse(data []byte) (*File, error) {
	return nil, fmt.Errorf("config parsing not yet implemented (v0.1); got %d bytes", len(data))
}