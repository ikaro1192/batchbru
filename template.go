package batchbru

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fujiwara/tfstate-lookup/tfstate"
	"github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
)

// SSMAPI is the subset of the SSM client used to resolve the ssm template
// function.
type SSMAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Funcs holds the template functions available while reading configuration and
// job definition files.
type Funcs struct {
	funcMap     template.FuncMap
	nativeFuncs []*jsonnet.NativeFunction
}

// baseFuncs builds the functions that are available everywhere, including in
// the configuration file itself.
func baseFuncs(ctx context.Context, ssmClient func() (SSMAPI, error)) *Funcs {
	f := &Funcs{funcMap: template.FuncMap{}}

	env := func(key string, defaults ...string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return strings.Join(defaults, " ")
	}
	mustEnv := func(key string) (string, error) {
		v, ok := os.LookupEnv(key)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not defined", key)
		}
		return v, nil
	}
	getSSM := func(name string) (string, error) {
		c, err := ssmClient()
		if err != nil {
			return "", err
		}
		out, err := c.GetParameter(ctx, &ssm.GetParameterInput{
			Name:           aws.String(name),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			return "", fmt.Errorf("failed to get SSM parameter %s: %w", name, err)
		}
		return aws.ToString(out.Parameter.Value), nil
	}
	// jsonEscape renders a value as it would appear inside a JSON string
	// literal. The quotes json.Marshal adds are removed one at a time: trimming
	// every quote character would eat an escaped quote at the end of the value
	// and leave a dangling backslash.
	jsonEscape := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		s := string(b)
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1 : len(s)-1]
		}
		return s, nil
	}

	f.funcMap["env"] = env
	f.funcMap["must_env"] = mustEnv
	f.funcMap["ssm"] = getSSM
	f.funcMap["json_escape"] = jsonEscape

	f.nativeFuncs = append(f.nativeFuncs,
		&jsonnet.NativeFunction{
			Name:   "env",
			Params: []ast.Identifier{"name", "default"},
			Func: func(args []any) (any, error) {
				name, ok := args[0].(string)
				if !ok {
					return nil, fmt.Errorf("env: name must be a string")
				}
				if v, ok := os.LookupEnv(name); ok && v != "" {
					return v, nil
				}
				return args[1], nil
			},
		},
		&jsonnet.NativeFunction{
			Name:   "must_env",
			Params: []ast.Identifier{"name"},
			Func: func(args []any) (any, error) {
				name, ok := args[0].(string)
				if !ok {
					return nil, fmt.Errorf("must_env: name must be a string")
				}
				return mustEnv(name)
			},
		},
		&jsonnet.NativeFunction{
			Name:   "ssm",
			Params: []ast.Identifier{"name"},
			Func: func(args []any) (any, error) {
				name, ok := args[0].(string)
				if !ok {
					return nil, fmt.Errorf("ssm: name must be a string")
				}
				return getSSM(name)
			},
		},
	)
	return f
}

// withPlugins returns the functions extended with those provided by the
// configured plugins. Relative file locations are resolved against baseDir,
// which is the directory holding the configuration file.
func (f *Funcs) withPlugins(ctx context.Context, plugins []Plugin, baseDir string) (*Funcs, error) {
	out := &Funcs{funcMap: template.FuncMap{}, nativeFuncs: append([]*jsonnet.NativeFunction{}, f.nativeFuncs...)}
	for k, v := range f.funcMap {
		out.funcMap[k] = v
	}
	for _, p := range plugins {
		switch p.Name {
		case "tfstate":
			if err := out.setupTFState(ctx, p, baseDir); err != nil {
				return nil, fmt.Errorf("plugin tfstate: %w", err)
			}
		default:
			return nil, fmt.Errorf("unknown plugin %q", p.Name)
		}
	}
	return out, nil
}

func (f *Funcs) setupTFState(ctx context.Context, p Plugin, baseDir string) error {
	loc, _ := p.Config["url"].(string)
	if loc == "" {
		loc, _ = p.Config["path"].(string)
	}
	if loc == "" {
		return fmt.Errorf("either url or path is required")
	}
	loc = resolveStateLocation(loc, baseDir)
	name := p.FuncPrefix + "tfstate"
	fm, err := tfstate.FuncMapWithName(ctx, name, loc)
	if err != nil {
		return err
	}
	for k, v := range fm {
		if _, exists := f.funcMap[k]; exists {
			return fmt.Errorf("function %s is already defined; set a distinct func_prefix", k)
		}
		f.funcMap[k] = v
	}
	nfs, err := tfstate.JsonnetNativeFuncs(ctx, p.FuncPrefix, loc)
	if err != nil {
		return err
	}
	f.nativeFuncs = append(f.nativeFuncs, nfs...)
	return nil
}

// resolveStateLocation makes a relative local path relative to the
// configuration file rather than to the working directory. Remote locations
// (s3://, https:// and so on) are left alone.
func resolveStateLocation(loc, baseDir string) string {
	if baseDir == "" || filepath.IsAbs(loc) || strings.Contains(loc, "://") {
		return loc
	}
	return filepath.Join(baseDir, loc)
}

// Render evaluates src as a text/template. name is used in error messages.
func (f *Funcs) Render(name, src string) (string, error) {
	tmpl, err := template.New(name).Funcs(f.funcMap).Parse(src)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", name, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, nil); err != nil {
		return "", fmt.Errorf("failed to render template %s: %w", name, err)
	}
	return buf.String(), nil
}

// JsonnetVM builds a Jsonnet VM with the native functions and the external
// variables given on the command line.
func (f *Funcs) JsonnetVM(extStr, extCode map[string]string) *jsonnet.VM {
	vm := jsonnet.MakeVM()
	for _, nf := range f.nativeFuncs {
		vm.NativeFunction(nf)
	}
	for k, v := range extStr {
		vm.ExtVar(k, v)
	}
	for k, v := range extCode {
		vm.ExtCode(k, v)
	}
	return vm
}
