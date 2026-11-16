package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
)

// mirrorStatusSynopsis is the command line of graft mirror status.
const mirrorStatusSynopsis = "graft mirror status --repo REPO [--branch BRANCH] [--json]"

// mirrorStatusHelp explains graft mirror status below its synopsis.
const mirrorStatusHelp = `Shows the mirrors of a repository and what the mirror policy of the server
decides for a push: the targets the repository is published to, how far every
mirror is behind the branch, and the thresholds of the policy. It is the view
an operator repairs from when a push is refused.

The command asks the same question the reference-transaction hook decides, so
a repository that this shows as refused takes no push. It exits 0 when a push
would go through and 1 when it would not, which is what a schedule or an agent
reads before it pushes.

  graft mirror status --repo shop.git
  graft mirror status --repo shop.git --branch release/1.0 --json

A mirror that never published, and a repository with no target at all, are
what the policy refuses when [graft.mirror] required is on. A mirror that is
behind warns from warn_lag commits behind, and refuses from deny_lag when the
server sets it.`

// mirrorStatus is the JSON output of graft mirror status.
type mirrorStatus struct {
	Repository string               `json:"repository"`
	Branch     string               `json:"branch,omitempty"`
	Policy     mirrorStatusPolicy   `json:"policy"`
	Targets    []mirrorStatusTarget `json:"targets"`
	// Allowed tells whether a push would go through.
	Allowed bool `json:"allowed"`
	// Code names the decision, for a machine to read.
	Code string `json:"code"`
	// Reason explains a refusal.
	Reason string `json:"reason,omitempty"`
	// Warning is what the push is told when it goes through with a mirror
	// that is behind.
	Warning string `json:"warning,omitempty"`
}

// mirrorStatusPolicy is the mirror policy as the command prints it.
type mirrorStatusPolicy struct {
	Required bool `json:"required"`
	WarnLag  int  `json:"warn_lag"`
	DenyLag  int  `json:"deny_lag"`
}

// mirrorStatusTarget is one mirror as the command prints it.
type mirrorStatusTarget struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Branches    []string          `json:"branches"`
	Enabled     bool              `json:"enabled"`
	Status      string            `json:"status,omitempty"`
	Error       string            `json:"error,omitempty"`
	PublishedAt string            `json:"published_at,omitempty"`
	Published   map[string]string `json:"published,omitempty"`
	Lag         int               `json:"lag"`
	Diverged    bool              `json:"diverged,omitempty"`
}

// cmdMirrorStatus shows the mirrors of a repository and the decision the
// mirror policy makes for a push.
func cmdMirrorStatus(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, args []string) error {
	if len(args) != 0 {
		return usagef("%s", mirrorStatusSynopsis)
	}
	if f.repo == "" {
		return usagef("mirror status requires --repo REPO")
	}
	if len(f.branches) > 1 {
		return usagef("mirror status takes at most one --branch")
	}

	var branch string
	if len(f.branches) == 1 {
		branch = strings.TrimPrefix(f.branches[0], "refs/heads/")
	}

	repo, _, _, err := gateRepo(ctx, o, &gateFlags{repo: f.repo})
	if err != nil {
		return err
	}

	targets, policy, err := repo.ListMirrorTargets(ctx, branch)
	if err != nil {
		return err
	}

	// The decision is the one the hook makes: the same targets, the same
	// policy, and the lag the server measured.
	decided := make([]mirrorgate.Target, 0, len(targets))
	status := mirrorStatus{
		Repository: f.repo,
		Branch:     branch,
		Policy: mirrorStatusPolicy{
			Required: policy.Required,
			WarnLag:  policy.WarnLag,
			DenyLag:  policy.DenyLag,
		},
		Targets: make([]mirrorStatusTarget, 0, len(targets)),
	}
	for _, target := range targets {
		decided = append(decided, mirrorgate.Target{
			Name:     target.Name,
			URL:      target.URL,
			Branches: target.Branches,
			Enabled:  target.Enabled,
			Synced:   target.Status == "ok" || target.Status == "diverged",
			Status:   target.Status,
			Error:    target.Error,
			Lag:      target.Lag,
			Diverged: target.Diverged,
		})
		printed := mirrorStatusTarget{
			Name:      target.Name,
			URL:       target.URL,
			Branches:  target.Branches,
			Enabled:   target.Enabled,
			Status:    target.Status,
			Error:     target.Error,
			Published: target.Published,
			Lag:       target.Lag,
			Diverged:  target.Diverged,
		}
		if !target.PublishedAt.IsZero() {
			printed.PublishedAt = target.PublishedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		status.Targets = append(status.Targets, printed)
	}

	decision := mirrorgate.Decide(mirrorgate.Input{
		Policy: mirrorgate.Policy{
			Required: policy.Required,
			WarnLag:  policy.WarnLag,
			DenyLag:  policy.DenyLag,
		},
		Repository: f.repo,
		Branch:     branch,
		Targets:    decided,
	})
	status.Allowed = decision.Allowed
	status.Code = decision.Code
	status.Reason = decision.Reason
	status.Warning = decision.Warning

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return err
		}
	} else {
		mirrorStatusPrint(out, status)
	}

	if !decision.Allowed {
		return fmt.Errorf("%s", decision.Reason)
	}
	return nil
}

// mirrorStatusPrint prints the status of the mirrors and the decision.
func mirrorStatusPrint(out io.Writer, status mirrorStatus) {
	fmt.Fprintf(out, "%s: %d mirror target(s)\n", status.Repository, len(status.Targets))

	targets := append([]mirrorStatusTarget(nil), status.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	for _, target := range targets {
		state := target.Status
		switch {
		case state == "":
			state = "never published"
		case !target.Enabled:
			state += ", disabled"
		}
		line := fmt.Sprintf("  %-16s %-48s %s", target.Name, mirrorDisplayURL(target.URL), state)
		if target.Lag > 0 {
			line += fmt.Sprintf(", %d commit(s) behind", target.Lag)
		}
		if target.Diverged {
			line += ", diverged"
		}
		fmt.Fprintln(out, line)
		if target.Error != "" {
			fmt.Fprintf(out, "  %-16s %s\n", "", target.Error)
		}
	}

	if !status.Policy.Required && status.Policy.WarnLag <= 0 && status.Policy.DenyLag <= 0 {
		fmt.Fprintln(out, "the mirror policy is off: this server takes every push")
		return
	}

	fmt.Fprintf(out, "policy: required=%t, warn at %d commits behind", status.Policy.Required, status.Policy.WarnLag)
	if status.Policy.DenyLag > 0 {
		fmt.Fprintf(out, ", refuse at %d", status.Policy.DenyLag)
	} else {
		fmt.Fprint(out, ", refusal off")
	}
	fmt.Fprintln(out)

	switch {
	case !status.Allowed:
		fmt.Fprintf(out, "a push would be refused: %s\n", status.Reason)
	case status.Warning != "":
		fmt.Fprintf(out, "a push would go through: %s\n", status.Warning)
	case mirrorStatusLag(status.Branch, status.Targets) > 0 && status.Policy.WarnLag > 0:
		// A mirror that is behind but inside the window is neither a refusal
		// nor a warning; saying that the mirrors are up to date would hide the
		// number an operator watches as it grows.
		fmt.Fprintf(out, "a push would go through: the mirrors are %d commit(s) behind, below the warn threshold of %d\n", mirrorStatusLag(status.Branch, status.Targets), status.Policy.WarnLag)
	case mirrorStatusLag(status.Branch, status.Targets) > 0:
		fmt.Fprintf(out, "a push would go through: the mirrors are %d commit(s) behind, and the policy sets no warn threshold\n", mirrorStatusLag(status.Branch, status.Targets))
	default:
		fmt.Fprintln(out, "a push would go through: the mirrors are up to date")
	}
}

// mirrorStatusLag is how far the mirrors of a branch are behind: the lag of the
// enabled target that publishes the branch and is furthest behind, which is the
// number the policy compares its thresholds with. An empty branch counts every
// target, because the question is then about the repository as a whole.
func mirrorStatusLag(branch string, targets []mirrorStatusTarget) int {
	lag := 0
	for _, target := range targets {
		if !target.Enabled || target.Lag <= lag {
			continue
		}
		if branch != "" && !(mirrorgate.Target{Branches: target.Branches}).Publishes(branch) {
			continue
		}
		lag = target.Lag
	}
	return lag
}
