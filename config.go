package batchbru

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Config is the batchbru configuration file.
type Config struct {
	Region         string   `yaml:"region,omitempty" json:"region,omitempty"`
	JobDefinitions []string `yaml:"job_definitions" json:"job_definitions"`
	Plugins        []Plugin `yaml:"plugins,omitempty" json:"plugins,omitempty"`

	path string
}

// Plugin extends the template functions available to job definition files.
type Plugin struct {
	Name       string         `yaml:"name" json:"name"`
	Config     map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	FuncPrefix string         `yaml:"func_prefix,omitempty" json:"func_prefix,omitempty"`
}

// Dir is the directory the configuration was loaded from. Job definition paths
// are resolved relative to it.
func (c *Config) Dir() string { return filepath.Dir(c.path) }

// LoadConfig reads and validates the configuration file at path.
func LoadConfig(ctx context.Context, path string, funcs *Funcs, opts *JsonnetOptions) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	rendered, err := funcs.Render(path, string(src))
	if err != nil {
		return nil, err
	}

	var c Config
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonnet":
		out, err := funcs.JsonnetVM(opts.ExtStr, opts.ExtCode).EvaluateAnonymousSnippet(path, rendered)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate %s: %w", path, err)
		}
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
	default:
		if err := yaml.Unmarshal([]byte(rendered), &c); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
	}
	c.path = path
	if len(c.JobDefinitions) == 0 {
		return nil, fmt.Errorf("%s: job_definitions is empty", path)
	}
	return &c, nil
}

// ResolveJobDefinitionFiles expands the job_definitions patterns into a sorted,
// deduplicated list of file paths.
func (c *Config) ResolveJobDefinitionFiles() ([]string, error) {
	seen := map[string]bool{}
	var files []string
	for _, pattern := range c.JobDefinitions {
		p := pattern
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Dir(), p)
		}
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no job definition file matches %q", pattern)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

// Save writes the configuration back to its file as YAML.
func (c *Config) Save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0644)
}
