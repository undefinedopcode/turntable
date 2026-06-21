// Package config loads and validates the OctoParser source configuration
// (octoparser.yaml). The config declares named sources mapping to connectors.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
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

// Parse decodes YAML config bytes.
func Parse(data []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if f.Sources == nil {
		f.Sources = map[string]Source{}
	}
	return &f, nil
}