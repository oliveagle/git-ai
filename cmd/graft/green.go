package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/sdk"
)

// greenSynopsis is the command line of graft green.
const greenSynopsis = "usage: graft green set --branch BRANCH --commit OID [--repo REPO] [--json]\n" +
	"       graft green show --branch BRANCH [--repo REPO] [--json]"

// greenUsage lists the subcommands of graft green.
const greenUsage = "green requires a subcommand: set or show (" + greenSynopsis + ")"

// greenHelp explains graft green below its synopsis.
const greenHelp = `Publishes and reads the verification checkpoint of a branch.

The checkpoint is the reference refs/graft/green/<branch>. It names the commit
whose tree passed the tests of the CI. A branch whose rule asks for a verified
branch (require_verified, see BRANCHES.md) takes no land while the checkpoint
does not cover the tip the land builds on.

- green set moves the checkpoint of the branch to the commit C, with a
  compare-and-swap: the move fails when the checkpoint moved since it was read,
  so that two CIs cannot overwrite each other. The commit must exist in the
  repository.
- green show prints the commit the checkpoint of the branch points at, and the
  tip of the branch, so that a CI can tell whether the branch is green.

The command needs the shared token: the checkpoint is the trust anchor of the
branch management policy, and only an operator may move it. graft-verify moves
it by itself when it runs the tests.

Exit status: 0 when the checkpoint was moved or read, 1 when the move was
refused, 2 when the command line is wrong.`

// greenRefPrefix is the namespace of the verification checkpoints of the
// branches, the green references that graft-verify moves.
const greenRefPrefix = "refs/graft/green/"

// greenFlags holds the flags of the green command.
type greenFlags struct {
	branch string
	commit string
	repo   string
}

// greenResult is the JSON output of the green command.
type greenResult struct {
	// Branch is the branch the checkpoint belongs to.
	Branch string `json:"branch"`
	// Ref is the reference that holds the checkpoint.
	Ref string `json:"ref"`
	// Checkpoint is the commit the checkpoint points at, empty when the
	// branch has none.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Previous is the commit the checkpoint pointed at before the move,
	// empty when it had none.
	Previous string `json:"previous,omitempty"`
	// Tip is the commit the branch points at.
	Tip string `json:"tip,omitempty"`
	// Green tells whether the checkpoint covers the tip of the branch.
	Green bool `json:"green"`
	// Moved tells whether the checkpoint was moved.
	Moved bool `json:"moved"`
}

// greenCommand returns the green command, the verification checkpoint of a
// branch.
func greenCommand() command {
	f := &greenFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", greenSynopsis, greenHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.branch, "branch", "", "branch whose checkpoint is moved or read")
			fs.StringVar(&f.commit, "commit", "", "commit the checkpoint is moved to")
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.BoolVar(&o.jsonOut, "json", false, "print the checkpoint as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdGreen(ctx, out, o, f, args)
		},
	}
}

// cmdGreen dispatches a green subcommand. The command line is checked before
// the server is contacted.
func cmdGreen(ctx context.Context, out io.Writer, o *options, f *greenFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", greenUsage)
	}
	if len(args) != 1 {
		return usagef("%s", greenSynopsis)
	}

	switch args[0] {
	case "set":
		if f.branch == "" {
			return usagef("green set requires --branch B")
		}
		if f.commit == "" {
			return usagef("green set requires --commit OID")
		}
		return greenSet(ctx, out, o, f)
	case "show":
		if f.branch == "" {
			return usagef("green show requires --branch B")
		}
		return greenShow(ctx, out, o, f)
	default:
		return usagef("unknown green subcommand %q: %s", args[0], greenUsage)
	}
}

// greenSet moves the checkpoint of the branch to the commit.
func greenSet(ctx context.Context, out io.Writer, o *options, f *greenFlags) error {
	repo, err := greenRepo(ctx, o, f)
	if err != nil {
		return err
	}

	// The checkpoint names a commit whose tree was tested: the commit must
	// exist in the repository.
	commit, err := repo.ResolveRevision(ctx, f.commit)
	if err != nil {
		return fmt.Errorf("the commit %q: %w", f.commit, err)
	}

	name := greenRefPrefix + strings.TrimPrefix(f.branch, "refs/heads/")
	previous, err := repo.ReferenceValue(ctx, name)
	if err != nil {
		return fmt.Errorf("reading the checkpoint of the branch %q: %w", f.branch, err)
	}

	if err := repo.UpdateReference(ctx, name, previous, commit); err != nil {
		return fmt.Errorf("moving the checkpoint of the branch %q to %s: %w", f.branch, commit, err)
	}

	res := greenResult{Branch: f.branch, Ref: name, Checkpoint: commit, Previous: previous, Moved: true}
	res.Tip, _ = greenTip(ctx, repo, f.branch)
	res.Green = res.Tip != "" && res.Tip == res.Checkpoint

	return printGreen(out, o.jsonOut, res)
}

// greenShow prints the checkpoint of the branch and the tip of the branch.
func greenShow(ctx context.Context, out io.Writer, o *options, f *greenFlags) error {
	repo, err := greenRepo(ctx, o, f)
	if err != nil {
		return err
	}

	name := greenRefPrefix + strings.TrimPrefix(f.branch, "refs/heads/")
	checkpoint, err := repo.ReferenceValue(ctx, name)
	if err != nil {
		return fmt.Errorf("reading the checkpoint of the branch %q: %w", f.branch, err)
	}

	res := greenResult{Branch: f.branch, Ref: name, Checkpoint: checkpoint}
	res.Tip, _ = greenTip(ctx, repo, f.branch)
	res.Green = res.Tip != "" && res.Tip == res.Checkpoint

	return printGreen(out, o.jsonOut, res)
}

// greenRepo returns the repository the green command works on.
func greenRepo(ctx context.Context, o *options, f *greenFlags) (*sdk.Repo, error) {
	t, _, err := resolveTarget(o, f.repo, "green")
	if err != nil {
		return nil, err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, errors.New("green: the backend cannot read a checkpoint")
	}
	return sb.repo, nil
}

// greenTip returns the commit the branch points at, empty when the branch does
// not exist.
func greenTip(ctx context.Context, repo *sdk.Repo, branch string) (string, error) {
	tip, err := repo.ResolveRevision(ctx, "refs/heads/"+strings.TrimPrefix(branch, "refs/heads/"))
	if err != nil {
		return "", err
	}
	return tip, nil
}

// printGreen prints the result of the green command.
func printGreen(out io.Writer, jsonOut bool, res greenResult) error {
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if res.Checkpoint == "" {
		fmt.Fprintf(out, "the branch %s has no checkpoint\n", res.Branch)
		return nil
	}
	if res.Moved {
		if res.Previous != "" {
			fmt.Fprintf(out, "the checkpoint of the branch %s moved from %s to %s\n", res.Branch, res.Previous, res.Checkpoint)
		} else {
			fmt.Fprintf(out, "the checkpoint of the branch %s is now %s\n", res.Branch, res.Checkpoint)
		}
		return nil
	}
	if res.Green {
		fmt.Fprintf(out, "the checkpoint of the branch %s is %s: the branch is green\n", res.Branch, res.Checkpoint)
		return nil
	}
	fmt.Fprintf(out, "the checkpoint of the branch %s is %s: the branch is not green\n", res.Branch, res.Checkpoint)
	return nil
}
