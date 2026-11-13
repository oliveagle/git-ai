package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/branchgate"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
)

// gateSynopsis is the command line of graft gate.
const gateSynopsis = "usage: graft gate check --branch BRANCH --config FILE [--commit OID] [--repo REPO] [--json]"

// gateUsage lists the subcommands of graft gate.
const gateUsage = "gate requires a subcommand: check (" + gateSynopsis + ")"

// gateHelp explains graft gate check below its synopsis.
const gateHelp = `Checks whether the branch management policy of the server ([[graft.branch]]) allows a
change to land on a branch, the way the server decides it when an agent lands:
a land is refused when the policy refuses it.

- The rules are read from the configuration file of the server (--config), so
  that the check asks with the rules the server enforces. The file is read as a
  server reads it and validated as a reload validates it: a file the server
  would reject exits 2.
- The state of the branch is read from the server: the tip of the branch, the
  open reviews of the branch and its verification checkpoint (the green
  reference of graft-verify). An agent token cannot read the checkpoint: it is
  then reported as absent.
- With --commit C the change C is checked, as a land of it on the branch.
  Without it, only the state of the branch is checked: the checkpoint and the
  rule that governs the direct updates of the branch.

Exit status: 0 when the policy allows the change, 1 when it refuses it (the
reason is printed), 2 when the command line or the configuration file is
wrong.`

// gateGreenRefPrefix is the namespace of the verification checkpoints of the
// branches, the green references of graft-verify.
const gateGreenRefPrefix = "refs/graft/green/"

// gateReviewLimit is the number of reviews the check reads: enough for every
// open review of a repository, which the server bounds.
const gateReviewLimit = 1000

// gateFlags holds the flags of graft gate check.
type gateFlags struct {
	branch string
	commit string
	config string
	repo   string
}

// gateResult is the JSON output of graft gate check.
type gateResult struct {
	Branch string `json:"branch"`
	// Managed tells whether the branch has a rule.
	Managed bool `json:"managed"`
	// Commit is the commit that would land, empty when the check is about the
	// state of the branch alone.
	Commit string `json:"commit,omitempty"`
	// Tip is the commit the branch points at.
	Tip string `json:"tip"`
	// Checkpoint is the commit the verification checkpoint of the branch points
	// at, empty when the branch has none.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Reviews is the number of open reviews of the branch.
	Reviews int `json:"reviews"`
	// Allowed tells whether the change may land.
	Allowed bool `json:"allowed"`
	// Code names the outcome, for a machine to read.
	Code string `json:"code"`
	// Reason explains the outcome.
	Reason string `json:"reason,omitempty"`
}

// gateCommand returns the gate command, the check of the branch management
// policy before a land.
func gateCommand() command {
	f := &gateFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", gateSynopsis, gateHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.branch, "branch", "", "branch the change would land on")
			fs.StringVar(&f.commit, "commit", "", "commit that would land on the branch (default: the pending commit of the workspace)")
			fs.StringVar(&f.config, "config", "", "configuration file of the server, which holds the branch rules")
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.BoolVar(&o.jsonOut, "json", false, "print the decision as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdGate(ctx, out, o, f, args)
		},
	}
}

// cmdGate dispatches a gate subcommand. Arguments are checked before the
// server is contacted.
func cmdGate(ctx context.Context, out io.Writer, o *options, f *gateFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", gateUsage)
	}
	if args[0] != "check" {
		return usagef("unknown gate subcommand %q: %s", args[0], gateUsage)
	}
	if len(args) != 1 {
		return usagef("%s", gateSynopsis)
	}
	if f.branch == "" {
		return usagef("gate check requires --branch B")
	}
	if f.config == "" {
		return usagef("gate check requires --config FILE: the branch rules are read from the configuration of the server")
	}

	graft, err := loadPolicy(f.config)
	if err != nil {
		// A reload would reject the file: the exit status of a usage error.
		return usagef("%s: %v", f.config, err)
	}

	t, root, err := gateTarget(o, f)
	if err != nil {
		return err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return errors.New("gate: the backend cannot check a branch")
	}

	commit := f.commit
	if commit == "" && root != "" {
		commit, err = gatePendingCommit(root, backend)
		if err != nil {
			return err
		}
	}

	res, err := gateCheck(ctx, sb.repo, graft, f.branch, commit)
	if err != nil {
		return err
	}

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		printGateCheck(out, res)
	}

	if !res.Allowed {
		return errors.New(res.Reason)
	}
	return nil
}

// gateTarget returns the server and repository of the gate command, and the
// workspace root. They come from the workspace when there is one. Without a
// workspace they come from -address, -storage and -repo, and the root is empty.
// Inside a workspace the repository is the one of the workspace, so -repo must
// name that repository or be left out.
func gateTarget(o *options, f *gateFlags) (target, string, error) {
	return resolveTarget(o, f.repo, "gate")
}

// gatePendingCommit returns the commit the workspace has pending: the change
// that graft commit has made and has not landed yet.
func gatePendingCommit(root string, backend workspace.Backend) (string, error) {
	ws, err := workspace.Open(root, backend)
	if err != nil {
		return "", err
	}
	pending := ws.Summary().Pending
	switch {
	case pending == nil:
		return "", usagef("the workspace has no pending commit to check: run graft commit first, or pass --commit C")
	case pending.Landed != "":
		return "", usagef("the pending commit already landed: pass --commit C to check another commit")
	}
	return pending.Commit, nil
}

// gateCheck decides whether the change may land on the branch, with the rules
// of the server and the state read from it.
func gateCheck(ctx context.Context, repo *sdk.Repo, graft config.Graft, branch, commit string) (gateResult, error) {
	res := gateResult{Branch: branch, Commit: commit}

	tip, err := repo.ResolveRevision(ctx, "refs/heads/"+branch)
	if err != nil {
		// A branch that does not exist has no tip: the decision refuses the
		// change, and says why.
		if status.Code(err) != codes.NotFound {
			return res, fmt.Errorf("reading the tip of the branch %q: %w", branch, err)
		}
	} else {
		res.Tip = tip
	}

	reviews, err := repo.ListReviews(ctx, sdk.ListReviewsOptions{TargetBranch: branch, Limit: gateReviewLimit})
	if err != nil {
		return res, fmt.Errorf("reading the reviews of the branch %q: %w", branch, err)
	}
	res.Reviews = len(reviews)

	checkpoint, err := repo.ResolveRevision(ctx, gateGreenRefPrefix+branch)
	if err != nil {
		// The branch has no checkpoint, or the token may not read it.
		if code := status.Code(err); code != codes.NotFound && code != codes.PermissionDenied {
			return res, fmt.Errorf("reading the checkpoint of the branch %q: %w", branch, err)
		}
	} else {
		res.Checkpoint = checkpoint
	}

	var changes []string
	if commit != "" {
		changes = []string{commit}
	}

	decision := branchgate.Decide(branchgate.Input{
		Rules:       graft.Branches,
		ReviewRules: graft.Review.RulesOrDefault(),
		Branch:      branch,
		Changes:     changes,
		Tip:         tip,
		Reviews:     reviews,
		// The checkpoint covers the branch when it points at its tip: the
		// branch is green.
		Verified: res.Checkpoint != "" && res.Checkpoint == tip,
	})

	res.Managed = decision.Rule != nil
	res.Allowed = decision.Allowed
	res.Code = decision.Code
	res.Reason = decision.Reason
	return res, nil
}

// printGateCheck prints the result of graft gate check for a reader.
func printGateCheck(out io.Writer, res gateResult) {
	subject := "the branch " + res.Branch
	if res.Commit != "" {
		subject = "the change " + res.Commit
	}

	if !res.Managed {
		fmt.Fprintf(out, "%s is not managed: the policy does not govern it\n", subject)
		return
	}

	if res.Allowed {
		fmt.Fprintf(out, "%s may land on %s\n", subject, res.Branch)
		return
	}

	fmt.Fprintf(out, "%s may not land on %s: %s\n", subject, res.Branch, res.Reason)
}
