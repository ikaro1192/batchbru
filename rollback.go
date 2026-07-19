package batchbru

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/batch"
)

// RollbackOption configures the rollback command.
type RollbackOption struct {
	Names  []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
	DryRun bool     `help:"show what would be deregistered without calling the API"`
}

// Rollback deregisters the latest ACTIVE revision so that the one before it
// becomes the revision jobs resolve to.
func (a *App) Rollback(ctx context.Context, opt *RollbackOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	var ec errorCollector
	for _, jd := range targets {
		if err := a.rollbackOne(ctx, jd, opt); err != nil {
			ec.add(jd.Name, err)
		}
	}
	return ec.err()
}

func (a *App) rollbackOne(ctx context.Context, jd *JobDef, opt *RollbackOption) error {
	revs, err := describeRevisions(ctx, a.Batch, jd.Name, statusActive)
	if err != nil {
		return err
	}
	if len(revs) < 2 {
		return fmt.Errorf("there is no ACTIVE revision to roll back to (found %d)", len(revs))
	}
	current, previous := revs[0], revs[1]
	a.printf("%s: deregister revision %d, making revision %d current",
		jd.Name, aws.ToInt32(current.Revision), aws.ToInt32(previous.Revision))
	if opt.DryRun {
		a.printf("%s: dry run, nothing was changed", jd.Name)
		return nil
	}
	_, err = a.Batch.DeregisterJobDefinition(ctx, &batch.DeregisterJobDefinitionInput{
		JobDefinition: current.JobDefinitionArn,
	})
	if err != nil {
		return fmt.Errorf("failed to deregister revision %d: %w", aws.ToInt32(current.Revision), err)
	}
	return nil
}

// DeregisterOption configures the deregister command.
type DeregisterOption struct {
	Names      []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
	Revision   int32    `help:"deregister this revision"`
	KeepLatest int32    `help:"deregister every ACTIVE revision except the newest N"`
	DryRun     bool     `help:"show what would be deregistered without calling the API"`
	Force      bool     `help:"do not ask for confirmation"`
}

// Deregister makes revisions INACTIVE, either a specific one or everything
// older than the newest N.
func (a *App) Deregister(ctx context.Context, opt *DeregisterOption) error {
	switch {
	case opt.Revision != 0 && opt.KeepLatest != 0:
		return fmt.Errorf("--revision and --keep-latest cannot be combined")
	case opt.Revision == 0 && opt.KeepLatest == 0:
		return fmt.Errorf("either --revision or --keep-latest is required")
	case opt.Revision < 0:
		return fmt.Errorf("--revision must be 1 or more")
	case opt.KeepLatest < 0:
		// Keeping none would deregister every revision and leave the job
		// definition unusable, which is what rollback also refuses to do.
		return fmt.Errorf("--keep-latest must be 1 or more; keeping none would deregister every revision")
	}
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}

	type victim struct {
		name     string
		arn      string
		revision int32
	}
	var (
		victims []victim
		ec      errorCollector
	)
	for _, jd := range targets {
		revs, err := describeRevisions(ctx, a.Batch, jd.Name, statusActive)
		if err != nil {
			ec.add(jd.Name, err)
			continue
		}
		for i, r := range revs {
			keep := true
			switch {
			case opt.Revision > 0:
				keep = aws.ToInt32(r.Revision) != opt.Revision
			case opt.KeepLatest > 0:
				keep = int32(i) < opt.KeepLatest
			}
			if !keep {
				victims = append(victims, victim{jd.Name, aws.ToString(r.JobDefinitionArn), aws.ToInt32(r.Revision)})
			}
		}
	}
	// A definition whose revisions could not be listed contributes no victims;
	// the ones that were listed are still processed, and the failure is
	// reported at the end.
	if len(victims) == 0 {
		a.printf("no revision to deregister")
		return ec.err()
	}
	for _, v := range victims {
		a.printf("will deregister %s revision %d", v.name, v.revision)
	}
	if opt.DryRun {
		a.printf("dry run, nothing was changed")
		return ec.err()
	}
	if !opt.Force {
		ok, err := a.confirm(fmt.Sprintf("deregister %d revision(s)?", len(victims)))
		if err != nil {
			return err
		}
		if !ok {
			a.printf("aborted")
			return nil
		}
	}
	for _, v := range victims {
		_, err := a.Batch.DeregisterJobDefinition(ctx, &batch.DeregisterJobDefinitionInput{
			JobDefinition: aws.String(v.arn),
		})
		ec.add(fmt.Sprintf("%s revision %d", v.name, v.revision), err)
	}
	return ec.err()
}

// confirm asks the user a yes/no question. Anything but an explicit yes -- a
// closed stdin included, as in CI -- counts as no.
func (a *App) confirm(prompt string) (bool, error) {
	fmt.Fprintf(a.Stdout, "%s [y/N]: ", prompt)
	in := a.Stdin
	if in == nil {
		in = os.Stdin
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("failed to read the answer: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
