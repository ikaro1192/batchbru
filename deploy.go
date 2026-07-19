package batchbru

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// errExitCode is returned to signal a non-zero exit without printing an error.
var errExitCode = errors.New("differences found")

// DeployOption configures the deploy command.
type DeployOption struct {
	Names  []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
	DryRun bool     `help:"show what would be registered without calling the API"`
	Force  bool     `help:"register a new revision even when there is no difference"`
}

// Deploy registers a new revision for each job definition that differs from the
// latest ACTIVE revision.
func (a *App) Deploy(ctx context.Context, opt *DeployOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	var ec errorCollector
	for _, jd := range targets {
		if err := a.deployOne(ctx, jd, opt); err != nil {
			ec.add(jd.Name, err)
		}
	}
	return ec.err()
}

func (a *App) deployOne(ctx context.Context, jd *JobDef, opt *DeployOption) error {
	if !opt.Force {
		d, err := a.diffOne(ctx, jd)
		if err != nil {
			return err
		}
		if d == "" {
			a.printf("%s: no changes, skipping (use --force to register anyway)", jd.Name)
			return nil
		}
		fmt.Fprint(a.Stdout, colorizeDiff(d))
	}
	if opt.DryRun {
		a.printf("%s: would register a new revision (dry run)", jd.Name)
		return nil
	}
	in, err := decodeRegisterInput(jd.Def)
	if err != nil {
		return err
	}
	out, err := a.Batch.RegisterJobDefinition(ctx, in)
	if err != nil {
		return fmt.Errorf("failed to register: %w", err)
	}
	a.printf("%s: registered revision %d", jd.Name, aws.ToInt32(out.Revision))
	return nil
}
