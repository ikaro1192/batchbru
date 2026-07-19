package batchbru

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// StatusOption configures the status command.
type StatusOption struct {
	Names []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
}

// Status prints the latest ACTIVE revision of each job definition.
func (a *App) Status(ctx context.Context, opt *StatusOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREVISION\tTYPE\tSTATUS\tARN")
	var ec errorCollector
	for _, jd := range targets {
		remote, err := latestActive(ctx, a.Batch, jd.Name)
		if err != nil {
			ec.add(jd.Name, err)
			continue
		}
		if remote == nil {
			fmt.Fprintf(w, "%s\t-\t-\tNOT REGISTERED\t-\n", jd.Name)
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
			jd.Name,
			aws.ToInt32(remote.Revision),
			aws.ToString(remote.Type),
			aws.ToString(remote.Status),
			aws.ToString(remote.JobDefinitionArn),
		)
	}
	w.Flush()
	return ec.err()
}

// RevisionsOption configures the revisions command.
type RevisionsOption struct {
	Names []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
	All   bool     `help:"include INACTIVE revisions"`
}

// Revisions lists the revisions of each job definition, newest first.
func (a *App) Revisions(ctx context.Context, opt *RevisionsOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	status := statusActive
	if opt.All {
		status = ""
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREVISION\tSTATUS")
	var ec errorCollector
	for _, jd := range targets {
		revs, err := describeRevisions(ctx, a.Batch, jd.Name, status)
		if err != nil {
			ec.add(jd.Name, err)
			continue
		}
		for _, r := range revs {
			fmt.Fprintf(w, "%s\t%d\t%s\n", jd.Name, aws.ToInt32(r.Revision), aws.ToString(r.Status))
		}
	}
	w.Flush()
	return ec.err()
}

// RenderOption configures the render command.
type RenderOption struct {
	Names []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
}

// Render prints the fully evaluated job definitions as JSON.
func (a *App) Render(ctx context.Context, opt *RenderOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	var ec errorCollector
	for _, jd := range targets {
		s, err := MarshalIndent(jd.Def)
		if err != nil {
			ec.add(jd.Name, err)
			continue
		}
		fmt.Fprint(a.Stdout, s)
	}
	return ec.err()
}
