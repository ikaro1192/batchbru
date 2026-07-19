package batchbru

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// App holds everything the commands operate on.
type App struct {
	Config  *Config
	JobDefs []*JobDef
	Batch   BatchAPI

	Debug  bool
	Stdout io.Writer
	Stderr io.Writer
}

// Options are the global command line options.
type Options struct {
	ConfigPath string
	Debug      bool
	ExtStr     map[string]string
	ExtCode    map[string]string
}

func (o *Options) jsonnet() *JsonnetOptions {
	return &JsonnetOptions{ExtStr: o.ExtStr, ExtCode: o.ExtCode}
}

// New loads the configuration and job definitions and builds the AWS clients.
func New(ctx context.Context, opts *Options) (*App, error) {
	app := &App{Debug: opts.Debug, Stdout: os.Stdout, Stderr: os.Stderr}
	loader := &awsLoader{ctx: ctx}
	base := baseFuncs(ctx, func() (SSMAPI, error) {
		c, err := loader.config()
		if err != nil {
			return nil, err
		}
		return ssm.NewFromConfig(c), nil
	})

	app.debugf("loading config %s", opts.ConfigPath)
	cfg, err := LoadConfig(ctx, opts.ConfigPath, base, opts.jsonnet())
	if err != nil {
		return nil, err
	}
	// The region has to be known before anything else can build an AWS client,
	// otherwise an ssm lookup in a job definition would silently fall back to
	// the SDK default region.
	loader.setRegion(cfg.Region)

	funcs, err := base.withPlugins(ctx, cfg.Plugins, cfg.Dir())
	if err != nil {
		return nil, err
	}
	defs, err := LoadJobDefs(cfg, funcs, opts.jsonnet())
	if err != nil {
		return nil, err
	}
	for _, jd := range defs {
		app.debugf("loaded job definition %s from %s", jd.Name, jd.Path)
	}
	awsCfg, err := loader.config()
	if err != nil {
		return nil, err
	}
	app.debugf("using region %s", awsCfg.Region)

	app.Config = cfg
	app.JobDefs = defs
	app.Batch = newDebugBatch(batch.NewFromConfig(awsCfg), app)
	return app, nil
}

// awsLoader builds the shared AWS configuration on demand, so that a run which
// needs no AWS access does not require credentials. The configuration is
// rebuilt if the region changes, which happens when the configuration file
// names a region after an ssm lookup has already forced a load.
type awsLoader struct {
	ctx context.Context

	mu        sync.Mutex
	region    string
	cfg       *aws.Config
	cfgRegion string
}

func (l *awsLoader) setRegion(region string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.region = region
}

func (l *awsLoader) config() (aws.Config, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cfg != nil && l.cfgRegion == l.region {
		return *l.cfg, nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if l.region != "" {
		opts = append(opts, awsconfig.WithRegion(l.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(l.ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load AWS config: %w", err)
	}
	l.cfg, l.cfgRegion = &cfg, l.region
	return cfg, nil
}

// Target returns the job definitions selected by --name.
func (a *App) Target(names []string) ([]*JobDef, error) {
	return Filter(a.JobDefs, names)
}

// debugf writes a diagnostic line to stderr when --debug is set.
func (a *App) debugf(format string, args ...any) {
	if a == nil || !a.Debug {
		return
	}
	fmt.Fprintf(a.Stderr, "[debug] "+format+"\n", args...)
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format+"\n", args...)
}

// errorsFor collects per-definition failures so that one broken definition does
// not stop the rest from being processed.
type errorCollector struct {
	errs []error
}

func (c *errorCollector) add(name string, err error) {
	if err != nil {
		c.errs = append(c.errs, fmt.Errorf("%s: %w", name, err))
	}
}

func (c *errorCollector) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return errors.Join(c.errs...)
}
