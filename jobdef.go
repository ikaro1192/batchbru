package batchbru

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

// JsonnetOptions carries the external variables passed on the command line.
type JsonnetOptions struct {
	ExtStr  map[string]string
	ExtCode map[string]string
}

// JobDef is a job definition loaded from a local file.
type JobDef struct {
	// Name is the jobDefinitionName declared in the file.
	Name string
	// Path is the file the definition was loaded from.
	Path string
	// Def is the definition in canonical form.
	Def map[string]any
}

// LoadJobDefs reads every job definition file listed in the configuration.
func LoadJobDefs(c *Config, funcs *Funcs, opts *JsonnetOptions) ([]*JobDef, error) {
	files, err := c.ResolveJobDefinitionFiles()
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	defs := make([]*JobDef, 0, len(files))
	for _, f := range files {
		jd, err := LoadJobDefFile(f, funcs, opts)
		if err != nil {
			return nil, err
		}
		if prev, dup := byName[jd.Name]; dup {
			return nil, fmt.Errorf("job definition %q is declared in both %s and %s", jd.Name, prev, f)
		}
		byName[jd.Name] = f
		defs = append(defs, jd)
	}
	return defs, nil
}

// LoadJobDefFile reads a single job definition file: template functions are
// evaluated first, then the result is parsed according to the file extension
// and normalized.
func LoadJobDefFile(path string, funcs *Funcs, opts *JsonnetOptions) (*JobDef, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	rendered, err := funcs.Render(path, string(src))
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonnet":
		out, err := funcs.JsonnetVM(opts.ExtStr, opts.ExtCode).EvaluateAnonymousSnippet(path, rendered)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate %s: %w", path, err)
		}
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal([]byte(rendered), &raw); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
	default:
		if err := json.Unmarshal([]byte(rendered), &raw); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
	}

	def, err := Normalize(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	name, _ := def["jobDefinitionName"].(string)
	if name == "" {
		return nil, fmt.Errorf("%s: jobDefinitionName is required", path)
	}
	return &JobDef{Name: name, Path: path, Def: def}, nil
}

// Filter returns the definitions whose name matches one of the given patterns.
// Patterns accept shell-style wildcards. An empty pattern list matches all.
func Filter(defs []*JobDef, patterns []string) ([]*JobDef, error) {
	if len(patterns) == 0 {
		return defs, nil
	}
	var out []*JobDef
	matched := map[string]bool{}
	for _, d := range defs {
		selected := false
		for _, p := range patterns {
			ok, err := filepath.Match(p, d.Name)
			if err != nil {
				return nil, fmt.Errorf("invalid --name pattern %q: %w", p, err)
			}
			if ok {
				if !selected {
					out = append(out, d)
					selected = true
				}
				matched[p] = true
			}
		}
	}
	for _, p := range patterns {
		if !matched[p] {
			return nil, fmt.Errorf("no job definition matches --name %q", p)
		}
	}
	return out, nil
}
