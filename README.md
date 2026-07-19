# batchbru

A CLI tool for managing AWS Batch job definitions as code.
Think of it as [ecspresso](https://github.com/fujiwara/ecspresso) for AWS Batch job definitions:
write your job definitions in JSON / Jsonnet / YAML, review them with `diff`, then `deploy`.

Compute environments and job queues are out of scope. Manage those with Terraform or similar.
So are scheduling policies and production job submission; `run` exists only to smoke-test a
definition right after deploying it.

## Why batchbru?

### When you need it — and when you don't

`SubmitJob` can override a job's command, environment and resources at submission time, but
**not its container image**. If you use immutable image tags (recommended), every application
release therefore needs a new job definition revision — the definition changes on the
*application's* release cadence, not the infrastructure's. batchbru exists for exactly that
situation: registering revisions from a release pipeline, with a reviewable diff, without
touching anything else.

Conversely, if your job definitions change rarely, or you use a mutable image tag that you
repoint on release, managing the definitions in Terraform alongside the compute environment is
perfectly fine and you may not need this tool.

### Relationship to Terraform

Terraform can manage job definitions too, and batchbru is not a replacement for it. What
batchbru changes is the workflow for the part that moves with application releases:

- **Deploy cadence separation.** Registering a revision from an application release pipeline
  with Terraform means running `terraform apply` — dragging the entire infrastructure state,
  its lock, and its blast radius into every application deploy. batchbru only ever touches job
  definitions.
- **No state file.** Revisions are immutable and the latest ACTIVE one is authoritative, so
  batchbru diffs directly against what is actually in AWS. There is no state to lock, drift,
  or reconcile.
- **Straight mapping to the API.** Terraform models the immutable revision history as one
  updatable resource, which makes the history and rollback awkward. batchbru embraces the API:
  `deploy` is `RegisterJobDefinition`, `rollback` deregisters the newest revision so the
  previous one becomes current, and old revisions remain available.

The two compose: compute environments, job queues, IAM roles and ECR repositories stay in
Terraform, and batchbru reads their outputs through the `tfstate` template function.

### Design focus

batchbru's emphasis is on making unattended CI deploys safe:

- The diff is normalized — API-materialised defaults are pruned, member names are canonical —
  so `deploy` is idempotent and can run unconditionally from CI without piling up redundant
  revisions.
- Typos in a definition are an error, never silently dropped members.
- Destructive commands have guardrails: `rollback` and `deregister` refuse to leave a job
  definition with zero ACTIVE revisions, and `deregister` prompts before acting.
- One configuration file manages many definitions (globs, `--name` narrowing), in JSON,
  Jsonnet, or YAML.

## Installation

```console
$ go install github.com/ikaro1192/batchbru/cmd/batchbru@latest
```

## Usage

### Import an existing job definition

```console
$ batchbru init --job-definition etl-daily
wrote jobdefs/etl-daily.jsonnet
wrote batchbru.yml
```

### Configuration file

```yaml
# batchbru.yml
region: ap-northeast-1

job_definitions:
  - jobdefs/*.jsonnet
  - jobdefs/*.yaml

plugins:
  - name: tfstate
    config:
      url: s3://my-bucket/terraform.tfstate
```

Both `job_definitions` and local tfstate paths are resolved relative to the configuration file.
A single configuration file can manage multiple job definitions, and every command can narrow the
target with `--name` (wildcards allowed, repeatable).

### Job definition files

Job definitions use the same format as the `RegisterJobDefinition` API input
(the same shape as `aws batch register-job-definition --cli-input-json`).
The content is passed to the API as-is, so all types are supported: `container`, `multinode`, ECS, and EKS.

```jsonnet
// jobdefs/etl-daily.jsonnet
{
  jobDefinitionName: 'etl-daily',
  type: 'container',
  platformCapabilities: ['FARGATE'],
  containerProperties: {
    image: '{{ tfstate `aws_ecr_repository.example.repository_url` }}:{{ must_env `IMAGE_TAG` }}',
    jobRoleArn: '{{ tfstate `aws_iam_role.batch_job.arn` }}',
    resourceRequirements: [
      { type: 'VCPU', value: '1' },
      { type: 'MEMORY', value: '2048' },
    ],
  },
  retryStrategy: { attempts: 3 },
}
```

Working examples live in [example/](example/).

### Deploying

```console
$ batchbru diff
$ batchbru deploy
etl-daily: registered revision 12
report: no changes, skipping (use --force to register anyway)
```

`deploy` registers a new revision only when there is a difference from the latest ACTIVE revision.
It never piles up redundant revisions with identical content, so it is safe to run repeatedly from CI.

### Rolling back

AWS Batch has no rollback API, so `rollback` deregisters the latest ACTIVE revision, effectively
making the previous revision current again.

```console
$ batchbru rollback
etl-daily: deregister revision 12, making revision 11 current
```

If only one ACTIVE revision exists, the command fails to avoid leaving the job definition unusable.

### Smoke-testing with run

`run` submits a one-off job using the latest ACTIVE revision — that is, what `deploy` just
registered — and with `--wait` follows it until it succeeds or fails, so a broken definition
stops the pipeline right there.

```console
$ batchbru run --name etl-daily --queue default --wait
etl-daily: submitted job 4f0f8a9c-... to queue default
etl-daily: RUNNABLE
etl-daily: RUNNING
etl-daily: SUCCEEDED
```

`run` targets exactly one job definition and supports no overrides, dependencies, or array
jobs. It is a smoke test, not a job runner: production job submission and scheduling are
intentionally out of scope, because they belong to whatever orchestrates your jobs
(EventBridge Scheduler, Step Functions, your application), not to a deploy tool.

### Cleaning up old revisions

```console
$ batchbru deregister --keep-latest 5
```

`--keep-latest` must be 1 or greater. Specifying 0 would mark every revision INACTIVE and render the
job definition unusable, so it is rejected for the same reason as `rollback`.

## Commands

| Command | Description |
|---|---|
| `init` | Import an existing job definition from AWS and start managing it |
| `render` | Print the JSON produced after template evaluation |
| `diff` | Show the difference against the latest ACTIVE revision (`--exit-code` for CI) |
| `deploy` | Register a new revision for job definitions that have changed |
| `run` | Submit a one-off job to smoke-test the latest ACTIVE revision (`--wait` to follow it) |
| `status` | Show the latest ACTIVE revision of each job definition |
| `revisions` | List revisions (`--all` to include INACTIVE ones) |
| `rollback` | Deregister the latest ACTIVE revision |
| `deregister` | Deregister revisions (`--revision` / `--keep-latest`) |
| `version` | Print the version |

## Template functions

Configuration files and job definition files are evaluated as `text/template`.

| Function | Description |
|---|---|
| `env "NAME" "default"` | Read an environment variable |
| `must_env "NAME"` | Read an environment variable; error if undefined |
| `ssm "/path/to/param"` | Fetch a value from SSM Parameter Store (SecureString is decrypted) |
| `json_escape` | Escape a value as a JSON string |
| `tfstate "TYPE.NAME.ATTR"` | Read a resource attribute from Terraform state (tfstate plugin) |

From Jsonnet, the same functions are available as `std.native('env')` / `std.native('must_env')` /
`std.native('ssm')` / `std.native('tfstate')`.
Use `--ext-str` / `--ext-code` to pass Jsonnet external variables.

When using multiple tfstate sources, set `func_prefix`.
With `func_prefix: shared_`, reference it as `{{ shared_tfstate ... }}`.

## Required IAM permissions

| Command | Actions |
|---|---|
| `diff` / `status` / `revisions` / `init` | `batch:DescribeJobDefinitions` |
| `deploy` | The above + `batch:RegisterJobDefinition` (plus `batch:TagResource` when using `tags`) |
| `rollback` / `deregister` | The above + `batch:DeregisterJobDefinition` |
| `run` | `batch:SubmitJob` (plus `batch:DescribeJobs` with `--wait`) |
| Template functions (when used) | `ssm:GetParameter`, read access to the tfstate location |

## Notes and caveats

### How AWS Batch shapes the behavior

Job definitions are immutable: `RegisterJobDefinition` always creates a new revision and existing
revisions can never be updated. Revisions are `ACTIVE` or `INACTIVE`, and `DeregisterJobDefinition`
makes one `INACTIVE` (actual deletion happens later on the AWS side). Submitting a job by name
without a revision number uses the **latest ACTIVE revision**. Therefore:

- `deploy` means registering a new revision.
- `rollback` means deregistering the latest ACTIVE revision so the previous one becomes current.
- `diff` compares your local definition against the latest ACTIVE revision.

### Configuration

- Job definition names are not written in the configuration file: the `jobDefinitionName` inside each
  job definition file is authoritative. Duplicate names after glob expansion are an error.
- A glob that matches no file is also an error, so a typo cannot silently reduce the target set to zero.
- `job_definitions` paths and tfstate local paths are resolved relative to the configuration file,
  never to the current working directory.
- `region` applies to both `ssm` lookups inside job definition files and Batch API calls. The one
  exception is `ssm` used **inside the configuration file itself**: `region` has not been read yet at
  that point, so the AWS SDK default resolution is used. Set `AWS_REGION` if you do this.
- Unknown members (typos) in a job definition are reported as an error rather than silently dropped.

### `init`

- If the configuration file exists, one line is appended **textually** to the `job_definitions:` block
  list so that comments and template expressions are preserved.
- If it cannot be edited safely (no list found, flow style such as `job_definitions: [a, b]`, or a
  Jsonnet configuration file), the configuration file is left completely untouched and the lines to add
  by hand are printed instead. The job definition file was still written, so this is not an error
  (exit code 0).
- Existing job definition files are never overwritten without `--overwrite`.

### `diff` normalization

Local and remote definitions are normalized through the same path before comparison: both are decoded
into `RegisterJobDefinitionInput` and re-encoded with API member names; the read-only fields
(`jobDefinitionArn`, `revision`, `status`, `containerOrchestrationType`) are stripped from the remote
side; and fields absent locally whose remote value is the API default are not reported as differences.
If a key is present locally it is always compared, even when its value equals the default.

#### False diffs

The list of non-empty API defaults is maintained by hand, so when AWS starts materialising a new
default that batchbru does not know about, it shows up as a **false diff**: a member that exists
only in the remote definition. This matters because `deploy` registers whenever `diff` is
non-empty — a false diff would register a redundant revision on every CI run.

`diff` and `deploy` therefore print a warning whenever a diff contains members that exist only
remotely, naming them. If you removed the member from your file on purpose, the warning is
noise — ignore it. If you never wrote that member, it is a default the API filled in:

- Workaround: add the member with the remote value to your local file; the diff disappears.
- Please also [report it](https://github.com/ikaro1192/batchbru/issues) so the default can be
  added to batchbru and the workaround becomes unnecessary for everyone.

### `deregister`

- `--revision` and `--keep-latest` are mutually exclusive; exactly one must be given.
- Without `--force`, the affected revisions are listed and a confirmation prompt is shown.

### Exit codes

- `0`: success (including `diff` finding no changes)
- `1`: runtime error, or `diff --exit-code` finding changes

When a command targets multiple job definitions and some of them fail, the remaining ones are still
processed; errors are reported together at the end with exit code 1.

## Development

```console
$ make test
$ make build
```
