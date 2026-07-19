package batchbru

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testFuncs(t *testing.T) *Funcs {
	t.Helper()
	return baseFuncs(context.Background(), func() (SSMAPI, error) {
		t.Fatal("the test should not need SSM")
		return nil, nil
	})
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadJobDefFileFormats checks that the three supported formats produce the
// same canonical definition.
func TestLoadJobDefFileFormats(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMAGE_TAG", "v1.2.3")

	files := map[string]string{
		"a.json": `{
  "jobDefinitionName": "example",
  "type": "container",
  "containerProperties": {"image": "app:{{ must_env ` + "`IMAGE_TAG`" + ` }}"}
}`,
		"b.jsonnet": `{
  jobDefinitionName: 'example',
  type: 'container',
  containerProperties: { image: 'app:' + std.native('must_env')('IMAGE_TAG') },
}`,
		"c.yaml": `jobDefinitionName: example
type: container
containerProperties:
  image: "app:{{ must_env ` + "`IMAGE_TAG`" + ` }}"
`,
	}
	want := `{
  "containerProperties": {
    "image": "app:v1.2.3"
  },
  "jobDefinitionName": "example",
  "type": "container"
}
`
	for name, body := range files {
		p := write(t, dir, name, body)
		jd, err := LoadJobDefFile(p, testFuncs(t), &JsonnetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if jd.Name != "example" {
			t.Errorf("%s: name is %q, want example", name, jd.Name)
		}
		got, err := MarshalIndent(jd.Def)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s produced:\n%s\nwant:\n%s", name, got, want)
		}
	}
}

func TestLoadJobDefFileMustEnvUnset(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.json", `{"jobDefinitionName":"x","type":"container","containerProperties":{"image":"{{ must_env `+"`BATCHBRU_TEST_UNSET`"+` }}"}}`)
	if _, err := LoadJobDefFile(p, testFuncs(t), &JsonnetOptions{}); err == nil {
		t.Fatal("expected an error for the undefined environment variable")
	}
}

func TestLoadJobDefFileRequiresName(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.json", `{"type":"container"}`)
	if _, err := LoadJobDefFile(p, testFuncs(t), &JsonnetOptions{}); err == nil {
		t.Fatal("expected an error for the missing jobDefinitionName")
	}
}

func TestJsonnetExtVar(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.jsonnet", `{
  jobDefinitionName: 'example',
  type: 'container',
  containerProperties: { image: std.extVar('image') },
}`)
	jd, err := LoadJobDefFile(p, testFuncs(t), &JsonnetOptions{ExtStr: map[string]string{"image": "app:v9"}})
	if err != nil {
		t.Fatal(err)
	}
	cp := jd.Def["containerProperties"].(map[string]any)
	if cp["image"] != "app:v9" {
		t.Errorf("image is %v, want app:v9", cp["image"])
	}
}

// TestLoadJobDefsRejectsDuplicateNames guards against two files registering the
// same job definition, where deploy order would decide the winner.
func TestLoadJobDefsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	body := `{"jobDefinitionName":"dup","type":"container","containerProperties":{"image":"app"}}`
	write(t, dir, "jobdefs/one.json", body)
	write(t, dir, "jobdefs/two.json", body)
	cfg := &Config{JobDefinitions: []string{"jobdefs/*.json"}, path: filepath.Join(dir, "batchbru.yml")}
	if _, err := LoadJobDefs(cfg, testFuncs(t), &JsonnetOptions{}); err == nil {
		t.Fatal("expected an error for the duplicated jobDefinitionName")
	}
}

func TestResolveJobDefinitionFilesGlob(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "jobdefs/etl-a.json", "{}")
	write(t, dir, "jobdefs/etl-b.json", "{}")
	write(t, dir, "jobdefs/other.json", "{}")
	cfg := &Config{JobDefinitions: []string{"jobdefs/etl-*.json"}, path: filepath.Join(dir, "batchbru.yml")}
	files, err := cfg.ResolveJobDefinitionFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("matched %v, want the two etl files", files)
	}
}

func TestResolveJobDefinitionFilesReportsNoMatch(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{JobDefinitions: []string{"jobdefs/*.json"}, path: filepath.Join(dir, "batchbru.yml")}
	if _, err := cfg.ResolveJobDefinitionFiles(); err == nil {
		t.Fatal("expected an error when a pattern matches nothing")
	}
}

func TestFilter(t *testing.T) {
	defs := []*JobDef{{Name: "etl-daily"}, {Name: "etl-hourly"}, {Name: "report"}}

	all, err := Filter(defs, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("an empty pattern list should match everything, got %d (%v)", len(all), err)
	}
	got, err := Filter(defs, []string{"etl-*"})
	if err != nil || len(got) != 2 {
		t.Fatalf("etl-* matched %d definitions (%v)", len(got), err)
	}
	if _, err := Filter(defs, []string{"nope"}); err == nil {
		t.Fatal("expected an error when --name matches nothing")
	}

	// Overlapping patterns: a definition already selected by an earlier pattern
	// still counts as a match for the later one, and is not selected twice.
	got, err = Filter(defs, []string{"etl-*", "etl-daily"})
	if err != nil {
		t.Fatalf("overlapping patterns reported an error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("overlapping patterns selected %d definitions, want 2", len(got))
	}
}

// TestJSONEscape covers the values that make naive quote trimming produce
// invalid JSON: a value ending in a quote, and one containing a newline.
func TestJSONEscape(t *testing.T) {
	for _, in := range []string{
		`plain`,
		`he said "hi"`,
		`trailing"`,
		"line\nbreak",
		`back\slash`,
	} {
		// The escaped value has to survive being embedded in a job definition
		// and parsed back, which is what it is used for.
		t.Setenv("BATCHBRU_TEST_VALUE", in)
		escaped, err := testFuncs(t).Render("test", "{{ json_escape (env `BATCHBRU_TEST_VALUE`) }}")
		if err != nil {
			t.Errorf("json_escape(%q): %v", in, err)
			continue
		}
		src := `{"jobDefinitionName":"x","type":"container","containerProperties":{"image":"` + escaped + `"}}`
		var raw map[string]any
		if err := json.Unmarshal([]byte(src), &raw); err != nil {
			t.Errorf("json_escape(%q) produced invalid JSON: %v", in, err)
			continue
		}
		cp := raw["containerProperties"].(map[string]any)
		if cp["image"] != in {
			t.Errorf("json_escape(%q) round-tripped to %q", in, cp["image"])
		}
	}
}
