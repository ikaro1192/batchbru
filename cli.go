package batchbru

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
)

// Version is set at build time.
var Version = "dev"

// CLI is the command line interface of batchbru.
type CLI struct {
	Config  string            `help:"path to the configuration file" short:"c" default:"batchbru.yml" env:"BATCHBRU_CONFIG"`
	Debug   bool              `help:"print debug logs"`
	ExtStr  map[string]string `help:"Jsonnet external variable (string), repeatable as KEY=VALUE" name:"ext-str"`
	ExtCode map[string]string `help:"Jsonnet external variable (code), repeatable as KEY=EXPR" name:"ext-code"`

	Init       InitOption       `cmd:"" help:"import an existing job definition and start tracking it"`
	Render     RenderOption     `cmd:"" help:"print the fully evaluated job definitions"`
	Diff       DiffOption       `cmd:"" help:"show the difference against the latest ACTIVE revision"`
	Deploy     DeployOption     `cmd:"" help:"register a new revision for changed job definitions"`
	Run        RunOption        `cmd:"" help:"submit a one-off job to smoke-test the latest ACTIVE revision"`
	Status     StatusOption     `cmd:"" help:"show the latest ACTIVE revision of each job definition"`
	Revisions  RevisionsOption  `cmd:"" help:"list the revisions of each job definition"`
	Rollback   RollbackOption   `cmd:"" help:"deregister the latest ACTIVE revision"`
	Deregister DeregisterOption `cmd:"" help:"deregister revisions"`
	Version    struct{}         `cmd:"" help:"print the version"`
}

// Run parses the arguments and executes the selected command.
func Run(ctx context.Context, args []string) error {
	var cli CLI
	parser, err := kong.New(&cli,
		kong.Name("batchbru"),
		kong.Description("Manage AWS Batch job definitions as code."),
		kong.UsageOnError(),
	)
	if err != nil {
		return err
	}
	kctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	command := kctx.Command()
	opts := &Options{
		ConfigPath: cli.Config,
		Debug:      cli.Debug,
		ExtStr:     cli.ExtStr,
		ExtCode:    cli.ExtCode,
	}

	switch command {
	case "version":
		fmt.Printf("batchbru %s\n", Version)
		return nil
	case "init":
		return Init(ctx, cli.Config, &cli.Init, os.Stdout)
	}

	app, err := New(ctx, opts)
	if err != nil {
		return err
	}
	switch command {
	case "render":
		return app.Render(ctx, &cli.Render)
	case "diff":
		return app.Diff(ctx, &cli.Diff)
	case "deploy":
		return app.Deploy(ctx, &cli.Deploy)
	case "run":
		return app.Run(ctx, &cli.Run)
	case "status":
		return app.Status(ctx, &cli.Status)
	case "revisions":
		return app.Revisions(ctx, &cli.Revisions)
	case "rollback":
		return app.Rollback(ctx, &cli.Rollback)
	case "deregister":
		return app.Deregister(ctx, &cli.Deregister)
	}
	return fmt.Errorf("unknown command %q", command)
}

// IsExitCode reports whether err only signals a non-zero exit status.
func IsExitCode(err error) bool {
	return errors.Is(err, errExitCode)
}
