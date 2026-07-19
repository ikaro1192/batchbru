package batchbru

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

// defaultWaitInterval is how often run --wait polls the job status.
const defaultWaitInterval = 5 * time.Second

// RunOption configures the run command.
type RunOption struct {
	Names []string `help:"job definition name to run (wildcards allowed, must match exactly one)" name:"name" short:"n"`
	Queue string   `help:"job queue to submit to" required:""`
	Wait  bool     `help:"poll until the job reaches SUCCEEDED or FAILED"`
}

// Run submits a one-off job using the latest ACTIVE revision, meant as a smoke
// test right after a deploy. Production job submission -- schedules, overrides,
// dependencies, array jobs -- is out of scope; use SubmitJob directly for that.
func (a *App) Run(ctx context.Context, opt *RunOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	if len(targets) != 1 {
		return fmt.Errorf("run targets exactly one job definition, but %d matched; narrow it down with --name", len(targets))
	}
	jd := targets[0]

	out, err := a.Batch.SubmitJob(ctx, &batch.SubmitJobInput{
		JobName: aws.String(jd.Name),
		// The bare name resolves to the latest ACTIVE revision, i.e. what the
		// preceding deploy registered.
		JobDefinition: aws.String(jd.Name),
		JobQueue:      aws.String(opt.Queue),
	})
	if err != nil {
		return fmt.Errorf("failed to submit: %w", err)
	}
	jobID := aws.ToString(out.JobId)
	a.printf("%s: submitted job %s to queue %s", jd.Name, jobID, opt.Queue)
	if !opt.Wait {
		return nil
	}
	return a.waitJob(ctx, jd.Name, jobID)
}

// waitJob polls the job until it reaches a terminal status. SUCCEEDED returns
// nil; FAILED returns an error carrying the status reason.
func (a *App) waitJob(ctx context.Context, name, jobID string) error {
	interval := a.WaitInterval
	if interval <= 0 {
		interval = defaultWaitInterval
	}
	var last types.JobStatus
	for {
		out, err := a.Batch.DescribeJobs(ctx, &batch.DescribeJobsInput{Jobs: []string{jobID}})
		if err != nil {
			return fmt.Errorf("failed to describe job %s: %w", jobID, err)
		}
		if len(out.Jobs) == 0 {
			return fmt.Errorf("job %s was not found", jobID)
		}
		job := out.Jobs[0]
		if job.Status != last {
			a.printf("%s: %s", name, job.Status)
			last = job.Status
		}
		if job.Status == types.JobStatusSucceeded || job.Status == types.JobStatusFailed {
			if c := job.Container; c != nil && aws.ToString(c.LogStreamName) != "" {
				a.printf("%s: log stream %s", name, aws.ToString(c.LogStreamName))
			}
			if job.Status == types.JobStatusFailed {
				reason := aws.ToString(job.StatusReason)
				if reason == "" {
					reason = "no status reason was reported"
				}
				return fmt.Errorf("job %s failed: %s", jobID, reason)
			}
			return nil
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
