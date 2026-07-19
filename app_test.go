package batchbru

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

// fakeBatch is an in-memory stand-in for the Batch API that keeps the revision
// semantics batchbru depends on: registering appends a revision, deregistering
// marks one inactive.
type fakeBatch struct {
	revisions    map[string][]types.JobDefinition
	registered   []*batch.RegisterJobDefinitionInput
	deregistered []string

	// submitted jobs and the status sequence DescribeJobs plays back for them,
	// one status per call (the last one repeats).
	submitted []*batch.SubmitJobInput
	jobPlan   []types.JobStatus
	jobReason string
	jobCalls  int
}

func newFakeBatch() *fakeBatch {
	return &fakeBatch{revisions: map[string][]types.JobDefinition{}}
}

func (f *fakeBatch) DescribeJobDefinitions(ctx context.Context, in *batch.DescribeJobDefinitionsInput, _ ...func(*batch.Options)) (*batch.DescribeJobDefinitionsOutput, error) {
	var out []types.JobDefinition
	for _, jd := range f.revisions[aws.ToString(in.JobDefinitionName)] {
		if in.Status != nil && aws.ToString(jd.Status) != aws.ToString(in.Status) {
			continue
		}
		out = append(out, jd)
	}
	return &batch.DescribeJobDefinitionsOutput{JobDefinitions: out}, nil
}

func (f *fakeBatch) RegisterJobDefinition(ctx context.Context, in *batch.RegisterJobDefinitionInput, _ ...func(*batch.Options)) (*batch.RegisterJobDefinitionOutput, error) {
	f.registered = append(f.registered, in)
	name := aws.ToString(in.JobDefinitionName)
	rev := int32(len(f.revisions[name]) + 1)
	f.revisions[name] = append(f.revisions[name], f.jobDefinitionFor(in, rev))
	return &batch.RegisterJobDefinitionOutput{
		JobDefinitionName: in.JobDefinitionName,
		Revision:          aws.Int32(rev),
	}, nil
}

func (f *fakeBatch) DeregisterJobDefinition(ctx context.Context, in *batch.DeregisterJobDefinitionInput, _ ...func(*batch.Options)) (*batch.DeregisterJobDefinitionOutput, error) {
	arn := aws.ToString(in.JobDefinition)
	f.deregistered = append(f.deregistered, arn)
	for name, revs := range f.revisions {
		for i, jd := range revs {
			if aws.ToString(jd.JobDefinitionArn) == arn {
				f.revisions[name][i].Status = aws.String("INACTIVE")
				return &batch.DeregisterJobDefinitionOutput{}, nil
			}
		}
	}
	return nil, fmt.Errorf("job definition %s not found", arn)
}

func (f *fakeBatch) SubmitJob(ctx context.Context, in *batch.SubmitJobInput, _ ...func(*batch.Options)) (*batch.SubmitJobOutput, error) {
	f.submitted = append(f.submitted, in)
	return &batch.SubmitJobOutput{
		JobId:   aws.String(fmt.Sprintf("job-%d", len(f.submitted))),
		JobName: in.JobName,
	}, nil
}

func (f *fakeBatch) DescribeJobs(ctx context.Context, in *batch.DescribeJobsInput, _ ...func(*batch.Options)) (*batch.DescribeJobsOutput, error) {
	if len(f.jobPlan) == 0 {
		return &batch.DescribeJobsOutput{}, nil
	}
	i := f.jobCalls
	if i >= len(f.jobPlan) {
		i = len(f.jobPlan) - 1
	}
	f.jobCalls++
	job := types.JobDetail{JobId: aws.String(in.Jobs[0]), Status: f.jobPlan[i]}
	if job.Status == types.JobStatusFailed && f.jobReason != "" {
		job.StatusReason = aws.String(f.jobReason)
	}
	return &batch.DescribeJobsOutput{Jobs: []types.JobDetail{job}}, nil
}

// jobDefinitionFor builds what the API would return for a registration,
// including the members it fills in with defaults.
func (f *fakeBatch) jobDefinitionFor(in *batch.RegisterJobDefinitionInput, rev int32) types.JobDefinition {
	name := aws.ToString(in.JobDefinitionName)
	jd := types.JobDefinition{
		JobDefinitionArn:           aws.String(fmt.Sprintf("arn:aws:batch:ap-northeast-1:123456789012:job-definition/%s:%d", name, rev)),
		JobDefinitionName:          in.JobDefinitionName,
		Revision:                   aws.Int32(rev),
		Status:                     aws.String(statusActive),
		Type:                       aws.String(string(in.Type)),
		ContainerOrchestrationType: types.OrchestrationTypeEcs,
		ContainerProperties:        in.ContainerProperties,
		NodeProperties:             in.NodeProperties,
		EcsProperties:              in.EcsProperties,
		EksProperties:              in.EksProperties,
		Parameters:                 in.Parameters,
		PlatformCapabilities:       in.PlatformCapabilities,
		RetryStrategy:              in.RetryStrategy,
		SchedulingPriority:         in.SchedulingPriority,
		Tags:                       in.Tags,
		Timeout:                    in.Timeout,
	}
	// Members the API materialises even when the registration left them unset.
	if in.PropagateTags == nil {
		jd.PropagateTags = aws.Bool(false)
	} else {
		jd.PropagateTags = in.PropagateTags
	}
	if len(in.PlatformCapabilities) == 0 {
		jd.PlatformCapabilities = []types.PlatformCapability{types.PlatformCapabilityEc2}
	}
	if jd.ContainerProperties != nil {
		cp := *jd.ContainerProperties
		if cp.Environment == nil {
			cp.Environment = []types.KeyValuePair{}
		}
		if cp.MountPoints == nil {
			cp.MountPoints = []types.MountPoint{}
		}
		if cp.Volumes == nil {
			cp.Volumes = []types.Volume{}
		}
		if cp.Ulimits == nil {
			cp.Ulimits = []types.Ulimit{}
		}
		if cp.Secrets == nil {
			cp.Secrets = []types.Secret{}
		}
		if cp.ReadonlyRootFilesystem == nil {
			cp.ReadonlyRootFilesystem = aws.Bool(false)
		}
		if cp.Privileged == nil {
			cp.Privileged = aws.Bool(false)
		}
		jd.ContainerProperties = &cp
	}
	return jd
}

func testApp(t *testing.T, f *fakeBatch, defs ...string) (*App, *bytes.Buffer) {
	t.Helper()
	var jobDefs []*JobDef
	for _, src := range defs {
		var raw map[string]any
		if err := json.Unmarshal([]byte(src), &raw); err != nil {
			t.Fatal(err)
		}
		def, err := Normalize(raw)
		if err != nil {
			t.Fatal(err)
		}
		jobDefs = append(jobDefs, &JobDef{Name: def["jobDefinitionName"].(string), Path: "test.json", Def: def})
	}
	out := &bytes.Buffer{}
	return &App{Config: &Config{}, JobDefs: jobDefs, Batch: f, Stdout: out, Stderr: out}, out
}

// mustDeploy is a setup step. A setup deploy that failed silently would let
// the assertions that follow pass vacuously -- a revision that was never
// registered shows neither a diff nor an INACTIVE entry.
func mustDeploy(t *testing.T, app *App, opt *DeployOption) {
	t.Helper()
	if err := app.Deploy(context.Background(), opt); err != nil {
		t.Fatalf("setup deploy: %v", err)
	}
}

const minimalDef = `{
  "jobDefinitionName": "example",
  "type": "container",
  "containerProperties": {
    "image": "app:v1",
    "resourceRequirements": [{"type": "VCPU", "value": "1"}, {"type": "MEMORY", "value": "2048"}]
  }
}`

// TestDeployIsIdempotent is the central guarantee: deploying a definition that
// is already the latest ACTIVE revision must not create another revision.
func TestDeployIsIdempotent(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()

	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if len(f.registered) != 1 {
		t.Fatalf("the first deploy registered %d revisions, want 1", len(f.registered))
	}

	out.Reset()
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	if len(f.registered) != 1 {
		t.Errorf("the second deploy registered another revision; output was:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no changes") {
		t.Errorf("expected the second deploy to report no changes, got:\n%s", out.String())
	}
}

func TestDeployForceRegistersAnyway(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}
	if err := app.Deploy(ctx, &DeployOption{Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.registered) != 2 {
		t.Errorf("--force registered %d revisions, want 2", len(f.registered))
	}
}

func TestDeployDryRunRegistersNothing(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	if err := app.Deploy(context.Background(), &DeployOption{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.registered) != 0 {
		t.Error("--dry-run registered a revision")
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("expected the output to mention the dry run, got:\n%s", out.String())
	}
}

func TestDiffExitCode(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()

	err := app.Diff(ctx, &DiffOption{ExitCode: true})
	if !IsExitCode(err) {
		t.Fatalf("a definition that does not exist remotely should report a difference, got %v", err)
	}
	if !strings.Contains(out.String(), "no ACTIVE revision") {
		t.Errorf("expected the diff to say the definition is new, got:\n%s", out.String())
	}

	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := app.Diff(ctx, &DiffOption{ExitCode: true}); err != nil {
		t.Fatalf("after deploying there should be no difference, got %v and:\n%s", err, out.String())
	}
	if out.String() != "" {
		t.Errorf("expected no diff output, got:\n%s", out.String())
	}
}

func TestDiffShowsChangedMember(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}

	app.JobDefs[0].Def["containerProperties"].(map[string]any)["image"] = "app:v2"
	out.Reset()
	if err := app.Diff(ctx, &DiffOption{}); err != nil {
		t.Fatal(err)
	}
	// The direction matters: the remote image is removed, the local one added.
	// Swapping the sides of the diff would still contain both strings, so the
	// sign and the value have to be checked on the same line.
	var removedOld, addedNew bool
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "-") && strings.Contains(line, "app:v1") {
			removedOld = true
		}
		if strings.HasPrefix(line, "+") && strings.Contains(line, "app:v2") {
			addedNew = true
		}
	}
	if !removedOld || !addedNew {
		t.Errorf("expected the diff to remove app:v1 and add app:v2, got:\n%s", out.String())
	}
}

// TestRunWaitsForSuccess submits a smoke-test job and follows it to SUCCEEDED.
func TestRunWaitsForSuccess(t *testing.T) {
	f := newFakeBatch()
	f.jobPlan = []types.JobStatus{types.JobStatusSubmitted, types.JobStatusRunning, types.JobStatusSucceeded}
	app, out := testApp(t, f, minimalDef)
	app.WaitInterval = time.Millisecond
	ctx := context.Background()
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}

	if err := app.Run(ctx, &RunOption{Queue: "default", Wait: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.submitted) != 1 {
		t.Fatalf("submitted %d jobs, want 1", len(f.submitted))
	}
	in := f.submitted[0]
	if aws.ToString(in.JobDefinition) != "example" || aws.ToString(in.JobQueue) != "default" {
		t.Errorf("submitted definition=%s queue=%s, want example/default",
			aws.ToString(in.JobDefinition), aws.ToString(in.JobQueue))
	}
	for _, s := range []string{"SUBMITTED", "RUNNING", "SUCCEEDED"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("expected the output to report %s, got:\n%s", s, out.String())
		}
	}
}

// TestRunFailureReportsReason makes sure a failed smoke test surfaces as an
// error carrying the status reason, so CI stops.
func TestRunFailureReportsReason(t *testing.T) {
	f := newFakeBatch()
	f.jobPlan = []types.JobStatus{types.JobStatusRunning, types.JobStatusFailed}
	f.jobReason = "Essential container in task exited"
	app, _ := testApp(t, f, minimalDef)
	app.WaitInterval = time.Millisecond
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})

	err := app.Run(ctx, &RunOption{Queue: "default", Wait: true})
	if err == nil || !strings.Contains(err.Error(), "Essential container") {
		t.Errorf("run reported %v, want the failure reason", err)
	}
}

// TestRunWaitJobDisappeared: DescribeJobs stops returning very old jobs, so an
// empty answer has to surface as an error rather than poll forever.
func TestRunWaitJobDisappeared(t *testing.T) {
	f := newFakeBatch() // an empty jobPlan makes DescribeJobs return no jobs
	app, _ := testApp(t, f, minimalDef)
	app.WaitInterval = time.Millisecond
	err := app.Run(context.Background(), &RunOption{Queue: "default", Wait: true})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("got %v, want a not-found error", err)
	}
}

func TestRunWithoutWaitOnlySubmits(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	if err := app.Run(context.Background(), &RunOption{Queue: "default"}); err != nil {
		t.Fatal(err)
	}
	if len(f.submitted) != 1 || f.jobCalls != 0 {
		t.Errorf("submitted=%d describeJobs calls=%d, want 1 and 0", len(f.submitted), f.jobCalls)
	}
}

// TestRunRequiresSingleTarget guards against accidentally smoke-testing every
// definition in the configuration at once.
func TestRunRequiresSingleTarget(t *testing.T) {
	f := newFakeBatch()
	other := strings.Replace(minimalDef, `"example"`, `"other"`, 1)
	app, _ := testApp(t, f, minimalDef, other)
	err := app.Run(context.Background(), &RunOption{Queue: "default"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("run reported %v, want an error demanding a single target", err)
	}
	if len(f.submitted) != 0 {
		t.Error("a job was submitted despite the ambiguous target")
	}
}

// TestDiffHintsAtFalseDiffs: a member that exists only remotely is either an
// intentional removal or an API default batchbru does not know about. The diff
// must point that out and name the member, so a false diff gets reported
// instead of silently registering redundant revisions from CI forever.
func TestDiffHintsAtFalseDiffs(t *testing.T) {
	f := newFakeBatch()
	withRetry := strings.Replace(minimalDef, `"type": "container",`,
		`"type": "container", "retryStrategy": {"attempts": 3},`, 1)
	app, out := testApp(t, f, withRetry)
	ctx := context.Background()
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}

	// Simulate the API materialising a default we do not know: the remote has
	// retryStrategy, the local definition does not mention it.
	delete(app.JobDefs[0].Def, "retryStrategy")
	out.Reset()
	if err := app.Diff(ctx, &DiffOption{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"only in the remote definition: retryStrategy", "false diff", "issues"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the diff output does not contain %q:\n%s", want, out.String())
		}
	}

	// A diff caused by a local change must not trigger the hint.
	app.JobDefs[0].Def["retryStrategy"] = map[string]any{"attempts": int64(5)}
	out.Reset()
	if err := app.Diff(ctx, &DiffOption{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "false diff") {
		t.Errorf("a genuine local change was flagged as a false diff:\n%s", out.String())
	}
}

func TestRollback(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()

	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}
	// A single revision must not be rolled back: it would leave nothing active.
	if err := app.Rollback(ctx, &RollbackOption{}); err == nil {
		t.Fatal("expected rollback to refuse when there is only one ACTIVE revision")
	}

	app.JobDefs[0].Def["containerProperties"].(map[string]any)["image"] = "app:v2"
	if err := app.Deploy(ctx, &DeployOption{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := app.Rollback(ctx, &RollbackOption{}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 1 || !strings.HasSuffix(f.deregistered[0], ":2") {
		t.Errorf("rollback deregistered %v, want revision 2", f.deregistered)
	}
	latest, err := latestActive(ctx, f, "example")
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(latest.Revision) != 1 {
		t.Errorf("revision %d is current after the rollback, want 1", aws.ToInt32(latest.Revision))
	}
}

func TestRollbackDryRun(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})
	if err := app.Rollback(ctx, &RollbackOption{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 0 {
		t.Error("--dry-run deregistered a revision")
	}
}

func TestDeregisterKeepLatest(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := app.Deploy(ctx, &DeployOption{Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Deregister(ctx, &DeregisterOption{KeepLatest: 2, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 3 {
		t.Fatalf("deregistered %d revisions, want 3", len(f.deregistered))
	}
	revs, err := describeRevisions(ctx, f, "example", statusActive)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || aws.ToInt32(revs[0].Revision) != 5 || aws.ToInt32(revs[1].Revision) != 4 {
		t.Errorf("revisions %v remain active, want 5 and 4", revs)
	}
}

func TestDeregisterSpecificRevision(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})

	if err := app.Deregister(ctx, &DeregisterOption{Revision: 1, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 1 || !strings.HasSuffix(f.deregistered[0], ":1") {
		t.Errorf("deregistered %v, want revision 1", f.deregistered)
	}
}

func TestDeregisterRequiresExactlyOneSelector(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	if err := app.Deregister(ctx, &DeregisterOption{}); err == nil {
		t.Error("expected an error when neither --revision nor --keep-latest is given")
	}
	if err := app.Deregister(ctx, &DeregisterOption{Revision: 1, KeepLatest: 1}); err == nil {
		t.Error("expected an error when both --revision and --keep-latest are given")
	}
	if err := app.Deregister(ctx, &DeregisterOption{Revision: -1}); err == nil || !strings.Contains(err.Error(), "1 or more") {
		t.Errorf("--revision -1 reported %v, want a message about the valid range", err)
	}
}

// TestPartialFailureIsReported checks that one failing definition does not stop
// the others, and that the failure still surfaces.
func TestPartialFailureIsReported(t *testing.T) {
	f := newFakeBatch()
	other := strings.Replace(minimalDef, `"example"`, `"other"`, 1)
	app, _ := testApp(t, f, minimalDef, other)
	app.Batch = &failingDescribe{fakeBatch: f, failFor: "example"}

	err := app.Deploy(context.Background(), &DeployOption{})
	if err == nil {
		t.Fatal("expected the failure for example to be reported")
	}
	if !strings.Contains(err.Error(), "example") {
		t.Errorf("the error does not name the failing definition: %v", err)
	}
	if len(f.registered) != 1 || aws.ToString(f.registered[0].JobDefinitionName) != "other" {
		t.Errorf("the healthy definition was not deployed, registered: %v", f.registered)
	}
}

type failingDescribe struct {
	*fakeBatch
	failFor string
}

func (f *failingDescribe) DescribeJobDefinitions(ctx context.Context, in *batch.DescribeJobDefinitionsInput, opts ...func(*batch.Options)) (*batch.DescribeJobDefinitionsOutput, error) {
	if aws.ToString(in.JobDefinitionName) == f.failFor {
		return nil, fmt.Errorf("access denied")
	}
	return f.fakeBatch.DescribeJobDefinitions(ctx, in, opts...)
}

// TestRender checks that the output is the evaluated definition as parseable
// JSON -- render feeds scripts and dry inspection, so text decoration would
// break it.
func TestRender(t *testing.T) {
	app, out := testApp(t, newFakeBatch(), minimalDef)
	if err := app.Render(context.Background(), &RenderOption{}); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("render did not produce valid JSON: %v\n%s", err, out.String())
	}
	if raw["jobDefinitionName"] != "example" {
		t.Errorf("rendered definition is %v", raw)
	}
}

func TestStatusAndRevisions(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()

	if err := app.Status(ctx, &StatusOption{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NOT REGISTERED") {
		t.Errorf("status should report an unregistered definition, got:\n%s", out.String())
	}

	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})
	if err := app.Deregister(ctx, &DeregisterOption{Revision: 1, Force: true}); err != nil {
		t.Fatalf("setup deregister: %v", err)
	}

	out.Reset()
	if err := app.Revisions(ctx, &RevisionsOption{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "INACTIVE") {
		t.Errorf("revisions should list only ACTIVE revisions by default, got:\n%s", out.String())
	}
	out.Reset()
	if err := app.Revisions(ctx, &RevisionsOption{All: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "INACTIVE") {
		t.Errorf("--all should list INACTIVE revisions, got:\n%s", out.String())
	}
}

// TestAWSLoaderUsesConfiguredRegion guards the wiring that makes the region
// from batchbru.yml reach the AWS clients. Before this was fixed, an ssm lookup
// in a job definition forced the config to load before the region was known,
// and the region in the configuration file was silently ignored.
func TestAWSLoaderUsesConfiguredRegion(t *testing.T) {
	isolateAWSEnv(t)
	l := &awsLoader{}

	// An ssm lookup during config loading forces a load before the region is
	// known; the region falls back to the SDK default, which is empty here.
	early, err := l.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if early.Region != "" {
		t.Fatalf("expected no region before one is configured, got %q", early.Region)
	}

	l.setRegion("ap-northeast-1")
	got, err := l.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Region != "ap-northeast-1" {
		t.Errorf("region is %q after setRegion, want ap-northeast-1", got.Region)
	}
}

func TestAWSLoaderCachesPerRegion(t *testing.T) {
	isolateAWSEnv(t)
	l := &awsLoader{}
	l.setRegion("us-east-1")
	if _, err := l.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := l.cfg
	if _, err := l.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.cfg != first {
		t.Error("the config was rebuilt even though the region did not change")
	}
}

// isolateAWSEnv removes the ambient AWS configuration so that the test observes
// only what the loader was told.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	empty := t.TempDir() + "/empty"
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// TestDebugBatchIsPassThroughWhenDisabled checks that --debug only adds logging
// and never changes which API calls are made.
func TestDebugBatchIsPassThroughWhenDisabled(t *testing.T) {
	f := newFakeBatch()
	if got := newDebugBatch(f, &App{Debug: false}); got != BatchAPI(f) {
		t.Error("the client was wrapped even though --debug is off")
	}

	out := &bytes.Buffer{}
	app := &App{Debug: true, Stdout: out, Stderr: out}
	wrapped := newDebugBatch(f, app)
	if _, err := wrapped.DescribeJobDefinitions(context.Background(), &batch.DescribeJobDefinitionsInput{
		JobDefinitionName: aws.String("example"),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "DescribeJobDefinitions name=example") {
		t.Errorf("expected the call to be logged, got:\n%s", out.String())
	}
}

// TestDeregisterContinuesAfterFailure checks the SPEC rule that one failing job
// definition must not stop the others: the revisions that could be listed are
// still deregistered, and the failure is reported at the end.
func TestDeregisterContinuesAfterFailure(t *testing.T) {
	f := newFakeBatch()
	other := strings.Replace(minimalDef, `"example"`, `"other"`, 1)
	app, _ := testApp(t, f, minimalDef, other)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})

	app.Batch = &failingDescribe{fakeBatch: f, failFor: "example"}
	err := app.Deregister(ctx, &DeregisterOption{KeepLatest: 1, Force: true})
	if err == nil {
		t.Fatal("the failure for example should be reported")
	}
	if !strings.Contains(err.Error(), "example") {
		t.Errorf("the error does not name the failing definition: %v", err)
	}
	if len(f.deregistered) != 1 || !strings.Contains(f.deregistered[0], "/other:") {
		t.Errorf("the healthy definition was not cleaned up, deregistered: %v", f.deregistered)
	}
}

// TestDeregisterDryRunReportsListingFailure makes sure --dry-run does not
// swallow the error for a definition whose revisions could not be listed.
func TestDeregisterDryRunReportsListingFailure(t *testing.T) {
	f := newFakeBatch()
	other := strings.Replace(minimalDef, `"example"`, `"other"`, 1)
	app, _ := testApp(t, f, minimalDef, other)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})

	app.Batch = &failingDescribe{fakeBatch: f, failFor: "example"}
	err := app.Deregister(ctx, &DeregisterOption{KeepLatest: 1, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "example") {
		t.Errorf("the dry run reported %v, want the failure for example", err)
	}
	if len(f.deregistered) != 0 {
		t.Error("--dry-run deregistered a revision")
	}
}

// TestDeregisterConfirmation covers the prompt: declining leaves every
// revision registered, agreeing deregisters them.
func TestDeregisterConfirmation(t *testing.T) {
	f := newFakeBatch()
	app, out := testApp(t, f, minimalDef)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})
	mustDeploy(t, app, &DeployOption{Force: true})

	app.Stdin = strings.NewReader("n\n")
	if err := app.Deregister(ctx, &DeregisterOption{KeepLatest: 1}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 0 {
		t.Error("revisions were deregistered although the prompt was declined")
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("expected the abort to be reported, got:\n%s", out.String())
	}

	app.Stdin = strings.NewReader("y\n")
	if err := app.Deregister(ctx, &DeregisterOption{KeepLatest: 1}); err != nil {
		t.Fatal(err)
	}
	if len(f.deregistered) != 1 {
		t.Errorf("deregistered %d revisions after agreeing, want 1", len(f.deregistered))
	}
}

// TestDeregisterRejectsKeepingNone guards the operation that would deregister
// every revision and leave the job definition unusable.
func TestDeregisterRejectsKeepingNone(t *testing.T) {
	f := newFakeBatch()
	app, _ := testApp(t, f, minimalDef)
	ctx := context.Background()
	mustDeploy(t, app, &DeployOption{})

	err := app.Deregister(ctx, &DeregisterOption{KeepLatest: -1, Force: true})
	if err == nil || !strings.Contains(err.Error(), "1 or more") {
		t.Errorf("--keep-latest -1 reported %v, want a message about keeping at least one", err)
	}
	if len(f.deregistered) != 0 {
		t.Error("revisions were deregistered despite the rejected option")
	}
}
