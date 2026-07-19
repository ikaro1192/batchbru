package batchbru

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddToConfigCreatesFile covers the first init in a fresh directory.
func TestAddToConfigCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "batchbru.yml")
	if _, err := addToConfig(p, "jobdefs/a.jsonnet", "ap-northeast-1"); err != nil {
		t.Fatal(err)
	}
	funcs := testFuncs(t)
	cfg, err := LoadConfig(t.Context(), p, funcs, &JsonnetOptions{})
	if err != nil {
		t.Fatalf("the generated config does not load back: %v", err)
	}
	if cfg.Region != "ap-northeast-1" || len(cfg.JobDefinitions) != 1 {
		t.Errorf("generated config is %+v", cfg)
	}
}

// TestAddToConfigPreservesComments is the reason the list is edited as text:
// rewriting the YAML would drop comments and expand template expressions.
func TestAddToConfigPreservesComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "batchbru.yml")
	original := `# managed by the platform team
region: ap-northeast-1

job_definitions:
  - jobdefs/existing.jsonnet

plugins:
  - name: tfstate
    config:
      url: s3://bucket/{{ must_env ` + "`ENV`" + ` }}/terraform.tfstate
`
	if err := os.WriteFile(p, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := addToConfig(p, "jobdefs/new.jsonnet", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"# managed by the platform team",
		"  - jobdefs/existing.jsonnet",
		"  - jobdefs/new.jsonnet",
		"url: s3://bucket/{{ must_env `ENV` }}/terraform.tfstate",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the edited config lost %q:\n%s", want, s)
		}
	}
	// The new entry has to join the existing list, not land after the plugins.
	if strings.Index(s, "- jobdefs/new.jsonnet") > strings.Index(s, "plugins:") {
		t.Errorf("the new entry was inserted outside job_definitions:\n%s", s)
	}
}

func TestAddToConfigIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "batchbru.yml")
	if err := os.WriteFile(p, []byte("job_definitions:\n  - jobdefs/a.jsonnet\n"), 0644); err != nil {
		t.Fatal(err)
	}
	msg, err := addToConfig(p, "jobdefs/a.jsonnet", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "already lists") {
		t.Errorf("re-adding the same path reported %q", msg)
	}
	got, _ := os.ReadFile(p)
	if strings.Count(string(got), "jobdefs/a.jsonnet") != 1 {
		t.Errorf("the entry was duplicated:\n%s", got)
	}
}

// TestAddToConfigLeavesUneditableConfigsAlone covers the shapes that cannot be
// edited by inserting a block list entry. Each must leave the file byte for
// byte unchanged and tell the user what to add, rather than corrupting the
// file or failing after the job definition has already been written.
func TestAddToConfigLeavesUneditableConfigsAlone(t *testing.T) {
	for _, c := range []struct{ name, file, body string }{
		{"no list", "batchbru.yml", "region: ap-northeast-1\n"},
		{"flow style list", "batchbru.yml", "job_definitions: [jobdefs/a.jsonnet]\n"},
		{"jsonnet config", "batchbru.jsonnet", "{ job_definitions: ['jobdefs/a.jsonnet'] }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), c.file)
			if err := os.WriteFile(p, []byte(c.body), 0644); err != nil {
				t.Fatal(err)
			}
			msg, err := addToConfig(p, "jobdefs/new.jsonnet", "")
			if err != nil {
				t.Fatalf("this should be reported, not an error: %v", err)
			}
			if !strings.Contains(msg, "by hand") {
				t.Errorf("the user was not told to edit by hand: %q", msg)
			}
			got, _ := os.ReadFile(p)
			if string(got) != c.body {
				t.Errorf("the config was modified:\n%s", got)
			}
		})
	}
}

func TestRenderInitFileFormats(t *testing.T) {
	def := map[string]any{"jobDefinitionName": "x", "type": "container"}
	for _, c := range []struct{ format, ext string }{
		{"jsonnet", ".jsonnet"},
		{"json", ".json"},
		{"yaml", ".yaml"},
	} {
		body, ext, err := renderInitFile(def, c.format)
		if err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		if ext != c.ext {
			t.Errorf("%s produced extension %q, want %q", c.format, ext, c.ext)
		}
		// Whatever init writes has to load back through the normal path.
		dir := t.TempDir()
		p := write(t, dir, "out"+ext, body)
		jd, err := LoadJobDefFile(p, testFuncs(t), &JsonnetOptions{})
		if err != nil {
			t.Fatalf("%s: the written file does not load back: %v", c.format, err)
		}
		if jd.Name != "x" {
			t.Errorf("%s: loaded name is %q", c.format, jd.Name)
		}
	}
}
