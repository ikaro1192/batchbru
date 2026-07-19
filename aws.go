package batchbru

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

// BatchAPI is the subset of the Batch client batchbru uses.
type BatchAPI interface {
	DescribeJobDefinitions(ctx context.Context, params *batch.DescribeJobDefinitionsInput, optFns ...func(*batch.Options)) (*batch.DescribeJobDefinitionsOutput, error)
	RegisterJobDefinition(ctx context.Context, params *batch.RegisterJobDefinitionInput, optFns ...func(*batch.Options)) (*batch.RegisterJobDefinitionOutput, error)
	DeregisterJobDefinition(ctx context.Context, params *batch.DeregisterJobDefinitionInput, optFns ...func(*batch.Options)) (*batch.DeregisterJobDefinitionOutput, error)
	SubmitJob(ctx context.Context, params *batch.SubmitJobInput, optFns ...func(*batch.Options)) (*batch.SubmitJobOutput, error)
	DescribeJobs(ctx context.Context, params *batch.DescribeJobsInput, optFns ...func(*batch.Options)) (*batch.DescribeJobsOutput, error)
}

const statusActive = "ACTIVE"

// describeRevisions returns every revision of name known to the API, newest
// first. status may be empty to include inactive revisions.
func describeRevisions(ctx context.Context, c BatchAPI, name, status string) ([]types.JobDefinition, error) {
	in := &batch.DescribeJobDefinitionsInput{
		JobDefinitionName: aws.String(name),
		MaxResults:        aws.Int32(100),
	}
	if status != "" {
		in.Status = aws.String(status)
	}
	var all []types.JobDefinition
	p := batch.NewDescribeJobDefinitionsPaginator(c, in)
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to describe job definition %s: %w", name, err)
		}
		all = append(all, out.JobDefinitions...)
	}
	slices.SortFunc(all, func(a, b types.JobDefinition) int {
		return cmp.Compare(aws.ToInt32(b.Revision), aws.ToInt32(a.Revision))
	})
	return all, nil
}

// latestActive returns the highest-numbered ACTIVE revision of name, or nil if
// the job definition does not exist yet.
func latestActive(ctx context.Context, c BatchAPI, name string) (*types.JobDefinition, error) {
	revs, err := describeRevisions(ctx, c, name, statusActive)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, nil
	}
	return &revs[0], nil
}

// debugBatch logs each Batch API call when --debug is set. It is a pass-through
// otherwise.
type debugBatch struct {
	BatchAPI
	app *App
}

func newDebugBatch(c BatchAPI, app *App) BatchAPI {
	if app == nil || !app.Debug {
		return c
	}
	return &debugBatch{BatchAPI: c, app: app}
}

func (d *debugBatch) DescribeJobDefinitions(ctx context.Context, in *batch.DescribeJobDefinitionsInput, optFns ...func(*batch.Options)) (*batch.DescribeJobDefinitionsOutput, error) {
	d.app.debugf("DescribeJobDefinitions name=%s status=%s", aws.ToString(in.JobDefinitionName), aws.ToString(in.Status))
	return d.BatchAPI.DescribeJobDefinitions(ctx, in, optFns...)
}

func (d *debugBatch) RegisterJobDefinition(ctx context.Context, in *batch.RegisterJobDefinitionInput, optFns ...func(*batch.Options)) (*batch.RegisterJobDefinitionOutput, error) {
	d.app.debugf("RegisterJobDefinition name=%s", aws.ToString(in.JobDefinitionName))
	return d.BatchAPI.RegisterJobDefinition(ctx, in, optFns...)
}

func (d *debugBatch) DeregisterJobDefinition(ctx context.Context, in *batch.DeregisterJobDefinitionInput, optFns ...func(*batch.Options)) (*batch.DeregisterJobDefinitionOutput, error) {
	d.app.debugf("DeregisterJobDefinition %s", aws.ToString(in.JobDefinition))
	return d.BatchAPI.DeregisterJobDefinition(ctx, in, optFns...)
}

func (d *debugBatch) SubmitJob(ctx context.Context, in *batch.SubmitJobInput, optFns ...func(*batch.Options)) (*batch.SubmitJobOutput, error) {
	d.app.debugf("SubmitJob name=%s queue=%s", aws.ToString(in.JobDefinition), aws.ToString(in.JobQueue))
	return d.BatchAPI.SubmitJob(ctx, in, optFns...)
}

func (d *debugBatch) DescribeJobs(ctx context.Context, in *batch.DescribeJobsInput, optFns ...func(*batch.Options)) (*batch.DescribeJobsOutput, error) {
	d.app.debugf("DescribeJobs jobs=%v", in.Jobs)
	return d.BatchAPI.DescribeJobs(ctx, in, optFns...)
}
