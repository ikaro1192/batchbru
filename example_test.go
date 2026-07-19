package batchbru

import (
	"context"
	"testing"
)

// TestExampleProject keeps the example under example/ working: it is the first
// thing a reader copies, and it also exercises the tfstate plugin end to end
// against a checked-in state file.
func TestExampleProject(t *testing.T) {
	ctx := context.Background()
	funcs := testFuncs(t)
	cfg, err := LoadConfig(ctx, "example/batchbru.yml", funcs, &JsonnetOptions{})
	if err != nil {
		t.Fatalf("failed to load the example config: %v", err)
	}
	withPlugins, err := funcs.withPlugins(ctx, cfg.Plugins, cfg.Dir())
	if err != nil {
		t.Fatalf("failed to set up the example plugins: %v", err)
	}
	defs, err := LoadJobDefs(cfg, withPlugins, &JsonnetOptions{})
	if err != nil {
		t.Fatalf("failed to load the example job definitions: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("loaded %d job definitions, want 2", len(defs))
	}
	for _, jd := range defs {
		cp, ok := jd.Def["containerProperties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no containerProperties", jd.Name)
		}
		// The tfstate lookups must have been resolved, not left as templates.
		if cp["jobRoleArn"] != "arn:aws:iam::123456789012:role/batch-job" {
			t.Errorf("%s: jobRoleArn is %v, want the ARN from the example state", jd.Name, cp["jobRoleArn"])
		}
	}
}

// TestTFStateFuncPrefix checks that a second state file can be attached under a
// distinct prefix, which is how several Terraform workspaces are combined.
func TestTFStateFuncPrefix(t *testing.T) {
	ctx := context.Background()
	funcs, err := testFuncs(t).withPlugins(ctx, []Plugin{
		{Name: "tfstate", Config: map[string]any{"url": "terraform.tfstate"}},
		{Name: "tfstate", Config: map[string]any{"url": "terraform.tfstate"}, FuncPrefix: "shared_"},
	}, "example")
	if err != nil {
		t.Fatal(err)
	}
	got, err := funcs.Render("test", "{{ shared_tfstate `aws_iam_role.batch_job.arn` }}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "arn:aws:iam::123456789012:role/batch-job" {
		t.Errorf("the prefixed function returned %q", got)
	}
}

// TestTFStateDuplicateFuncName checks that attaching two state files without a
// prefix is reported instead of one silently shadowing the other.
func TestTFStateDuplicateFuncName(t *testing.T) {
	ctx := context.Background()
	_, err := testFuncs(t).withPlugins(ctx, []Plugin{
		{Name: "tfstate", Config: map[string]any{"url": "terraform.tfstate"}},
		{Name: "tfstate", Config: map[string]any{"url": "terraform.tfstate"}},
	}, "example")
	if err == nil {
		t.Fatal("expected an error when two tfstate plugins share a function name")
	}
}

func TestUnknownPlugin(t *testing.T) {
	_, err := testFuncs(t).withPlugins(context.Background(), []Plugin{{Name: "cloudformation"}}, "")
	if err == nil {
		t.Fatal("expected an error for an unknown plugin")
	}
}
