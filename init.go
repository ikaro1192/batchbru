package batchbru

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/goccy/go-yaml"
)

// jobDefNameRe is the character set AWS Batch allows in a job definition name.
// Validating up front gives a clear error instead of an API failure, and --
// because the name becomes part of the file path init writes to -- keeps a
// name like ../../x from escaping the target directory.
var jobDefNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// InitOption configures the init command.
type InitOption struct {
	JobDefinition string `help:"name of the existing job definition to import" required:""`
	Format        string `help:"output format of the job definition file" enum:"jsonnet,json,yaml" default:"jsonnet"`
	Path          string `help:"directory to write the job definition file into" default:"jobdefs"`
	Region        string `help:"AWS region (defaults to the SDK resolution)"`
	Overwrite     bool   `help:"overwrite the job definition file if it already exists"`
}

// Init imports an existing job definition from AWS and starts tracking it in
// the configuration file, creating the file if it does not exist yet.
func Init(ctx context.Context, configPath string, opt *InitOption, stdout io.Writer) error {
	if !jobDefNameRe.MatchString(opt.JobDefinition) {
		return fmt.Errorf("invalid job definition name %q: up to 128 letters, numbers, hyphens and underscores", opt.JobDefinition)
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opt.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opt.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	return importJobDefinition(ctx, batch.NewFromConfig(awsCfg), configPath, opt, stdout)
}

// importJobDefinition is Init after the AWS client has been built: everything
// here is deterministic given the client, which is what makes the overwrite
// guard and the file handling testable. opt.JobDefinition is assumed to be
// validated.
func importJobDefinition(ctx context.Context, client BatchAPI, configPath string, opt *InitOption, stdout io.Writer) error {
	remote, err := latestActive(ctx, client, opt.JobDefinition)
	if err != nil {
		return err
	}
	if remote == nil {
		return fmt.Errorf("job definition %s has no ACTIVE revision", opt.JobDefinition)
	}
	def, err := RemoteToMap(*remote)
	if err != nil {
		return fmt.Errorf("failed to normalize the remote definition: %w", err)
	}

	body, ext, err := renderInitFile(def, opt.Format)
	if err != nil {
		return err
	}
	configDir := filepath.Dir(configPath)
	defPath := filepath.Join(configDir, opt.Path, opt.JobDefinition+ext)
	if _, err := os.Stat(defPath); err == nil && !opt.Overwrite {
		return fmt.Errorf("%s already exists (use --overwrite to replace it)", defPath)
	}
	if err := os.MkdirAll(filepath.Dir(defPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(defPath, []byte(body), 0644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", defPath)

	rel, err := filepath.Rel(configDir, defPath)
	if err != nil {
		rel = defPath
	}
	msg, err := addToConfig(configPath, rel, opt.Region)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, msg)
	return nil
}

func renderInitFile(def map[string]any, format string) (string, string, error) {
	switch format {
	case "yaml":
		b, err := yaml.Marshal(def)
		if err != nil {
			return "", "", err
		}
		return string(b), ".yaml", nil
	case "json":
		s, err := MarshalIndent(def)
		return s, ".json", err
	default:
		// A JSON document is also a valid Jsonnet document, so the imported
		// definition is a starting point the user can then parameterise.
		s, err := MarshalIndent(def)
		return s, ".jsonnet", err
	}
}

// addToConfig registers path in the configuration file and reports what it
// did. An existing file is edited textually so that comments and template
// expressions survive. When the list cannot be edited safely the file is left
// untouched and the caller is told what to add by hand -- writing the job
// definition file is still useful on its own, so this is not an error.
func addToConfig(configPath, path, region string) (string, error) {
	manual := fmt.Sprintf("could not edit %s automatically; add %q to job_definitions by hand", configPath, path)
	isJsonnet := strings.EqualFold(filepath.Ext(configPath), ".jsonnet")

	src, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		// Save writes YAML, which a .jsonnet configuration would then fail to
		// evaluate, so a missing Jsonnet config is left for the user to create.
		if isJsonnet {
			return manual, nil
		}
		c := Config{Region: region, JobDefinitions: []string{path}, path: configPath}
		if err := c.Save(); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s", configPath), nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", configPath, err)
	}

	if isJsonnet {
		return manual, nil
	}

	lines := strings.Split(string(src), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "job_definitions:") {
			start = i
			break
		}
	}
	if start < 0 {
		return manual, nil
	}
	// A flow-style list (job_definitions: [a, b]) holds its entries on the same
	// line; appending a block entry after it would produce invalid YAML.
	if strings.TrimSpace(strings.TrimPrefix(lines[start], "job_definitions:")) != "" {
		return manual, nil
	}

	end := start + 1
	for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "- ") {
		if strings.TrimSpace(lines[end]) == "- "+path {
			return fmt.Sprintf("%s already lists %s", configPath, path), nil
		}
		end++
	}
	updated := append([]string{}, lines[:end]...)
	updated = append(updated, "  - "+path)
	updated = append(updated, lines[end:]...)
	if err := os.WriteFile(configPath, []byte(strings.Join(updated, "\n")), 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("added %s to %s", path, configPath), nil
}
