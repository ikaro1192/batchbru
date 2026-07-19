package batchbru

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fatih/color"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
)

// DiffOption configures the diff command.
type DiffOption struct {
	Names    []string `help:"job definition names to target (wildcards allowed, repeatable)" name:"name" short:"n"`
	ExitCode bool     `help:"exit with code 1 when there is a difference"`
}

// Diff compares each local job definition against the latest ACTIVE revision.
func (a *App) Diff(ctx context.Context, opt *DiffOption) error {
	targets, err := a.Target(opt.Names)
	if err != nil {
		return err
	}
	var ec errorCollector
	changed := false
	for _, jd := range targets {
		d, err := a.diffOne(ctx, jd)
		if err != nil {
			ec.add(jd.Name, err)
			continue
		}
		if d.text != "" {
			changed = true
			fmt.Fprint(a.Stdout, colorizeDiff(d.text))
			a.falseDiffHint(jd.Name, d.remoteOnly)
		}
	}
	if err := ec.err(); err != nil {
		return err
	}
	if changed && opt.ExitCode {
		return errExitCode
	}
	return nil
}

// diffResult is what diffOne reports: the unified diff text (empty when the
// definitions are equivalent) and the members that exist only remotely.
type diffResult struct {
	text       string
	remoteOnly []string
}

// diffOne renders the unified diff between the latest ACTIVE revision of jd and
// its local definition. An empty text means they are equivalent.
func (a *App) diffOne(ctx context.Context, jd *JobDef) (diffResult, error) {
	remote, err := latestActive(ctx, a.Batch, jd.Name)
	if err != nil {
		return diffResult{}, err
	}

	var (
		remoteText string
		remoteOnly []string
		fromLabel  = fmt.Sprintf("%s (no ACTIVE revision)", jd.Name)
	)
	if remote != nil {
		m, err := RemoteToMap(*remote)
		if err != nil {
			return diffResult{}, fmt.Errorf("failed to normalize the remote definition: %w", err)
		}
		m = PruneDefaults(m, jd.Def)
		remoteOnly = RemoteOnlyPaths(m, jd.Def)
		remoteText, err = MarshalIndent(m)
		if err != nil {
			return diffResult{}, err
		}
		fromLabel = fmt.Sprintf("%s:%d", jd.Name, aws.ToInt32(remote.Revision))
	}

	localText, err := MarshalIndent(jd.Def)
	if err != nil {
		return diffResult{}, err
	}
	if remoteText == localText {
		return diffResult{}, nil
	}
	edits := myers.ComputeEdits(span.URIFromPath(fromLabel), remoteText, localText)
	u := gotextdiff.ToUnified(fromLabel, jd.Path, remoteText, edits)
	return diffResult{text: fmt.Sprint(u), remoteOnly: remoteOnly}, nil
}

// falseDiffHint warns about members that exist only in the remote definition.
// Removing a member on purpose looks exactly like the API having materialised a
// default that nonFalsyDefaults does not know about; in the latter case the
// diff is false and deploy would register a redundant revision on every run, so
// point the user at the workaround and the issue tracker.
func (a *App) falseDiffHint(name string, remoteOnly []string) {
	if len(remoteOnly) == 0 {
		return
	}
	fmt.Fprintf(a.Stderr, "%s: only in the remote definition: %s\n", name, strings.Join(remoteOnly, ", "))
	fmt.Fprint(a.Stderr, "If you did not remove these members, they are probably defaults the API filled in\n"+
		"and this is a false diff: deploying would register a redundant revision on every run.\n"+
		"Add the members to your local file to silence it, and please report them at\n"+
		"https://github.com/ikaro1192/batchbru/issues so batchbru can learn the default.\n")
}

func colorizeDiff(s string) string {
	red := color.New(color.FgRed).SprintFunc()
	green := color.New(color.FgGreen).SprintFunc()
	cyan := color.New(color.FgCyan).SprintFunc()
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "@@"):
			b.WriteString(cyan(line))
		case strings.HasPrefix(line, "-"):
			b.WriteString(red(line))
		case strings.HasPrefix(line, "+"):
			b.WriteString(green(line))
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}
