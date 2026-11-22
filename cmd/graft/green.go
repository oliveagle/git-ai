package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/greenref"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
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
- --suite names a suite that ran on the tree the checkpoint certifies, and may
  be repeated. Each suite is published in its own reference, next to the
  checkpoint, naming the commit it ran on: the rule of the branch may demand
  the suites a change needs (require_suites), and a checkpoint that certifies
  only some of them does not carry a land.
- The checkpoint certifies a tree: the tree the suite ran on, which
  refs/graft/green-tree/<branch> names. green set publishes the tree of the
  commit it names, which is the tree a suite has to test for the checkpoint to
  mean anything. Run it in the workspace that was tested: a sparse workspace, a
  workspace of another commit, or a workspace with a tracked file changed
  tested a tree that is not the tree of the commit, and the move is refused.
  A branch whose rule asks for the whole tree (require_full_tree) takes no land
  while the checkpoint certifies no tree.
- green show prints the commit the checkpoint of the branch points at, the
  tree it certifies, the suites it certifies and the tip of the branch, so that
  a CI can tell whether the branch is green.

The command needs the shared token: the checkpoint is the trust anchor of the
branch management policy, and only an operator may move it. graft-verify moves
it by itself when it runs the tests.

Exit status: 0 when the checkpoint was moved or read, 1 when the move was
refused, 2 when the command line is wrong.`

// greenFlags holds the flags of the green command.
type greenFlags struct {
	branch string
	commit string
	repo   string
	suites suiteList
}

// suiteList is a repeatable string flag: the suites that ran on the tree the
// checkpoint certifies.
type suiteList []string

func (s *suiteList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *suiteList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// greenResult is the JSON output of the green command.
type greenResult struct {
	// Branch is the branch the checkpoint belongs to.
	Branch string `json:"branch"`
	// Ref is the reference that holds the checkpoint.
	Ref string `json:"ref"`
	// TreeRef is the reference that holds the tree the checkpoint certifies.
	TreeRef string `json:"tree_ref"`
	// Checkpoint is the commit the checkpoint points at, empty when the
	// branch has none.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Previous is the commit the checkpoint pointed at before the move,
	// empty when it had none.
	Previous string `json:"previous,omitempty"`
	// Tree is the tree of the commit the checkpoint names.
	Tree string `json:"tree,omitempty"`
	// CertifiedTree is the tree the checkpoint certifies: the tree the suite
	// ran on. It is empty when the checkpoint certifies no tree.
	CertifiedTree string `json:"certified_tree,omitempty"`
	// FullTree tells whether the checkpoint certifies the whole tree of the
	// commit it names.
	FullTree bool `json:"full_tree"`
	// Suites are the suites the checkpoint certifies, among the ones the rule
	// of the branch requires. A suite with no commit certified none: the
	// suite did not run on the commit the checkpoint names.
	Suites []greenSuite `json:"suites,omitempty"`
	// Tip is the commit the branch points at.
	Tip string `json:"tip,omitempty"`
	// Green tells whether the checkpoint covers the tip of the branch.
	Green bool `json:"green"`
	// Moved tells whether the checkpoint was moved.
	Moved bool `json:"moved"`
}

// greenSuite is one suite the checkpoint of a branch certifies.
type greenSuite struct {
	// Name is the name of the suite, such as "unit".
	Name string `json:"suite"`
	// Commit is the commit the suite ran on, empty when the checkpoint
	// certifies no run of the suite.
	Commit string `json:"commit,omitempty"`
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
			fs.Var(&f.suites, "suite", "suite that ran on the tree the checkpoint certifies (repeatable)")
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
		for _, suite := range f.suites {
			if !greenref.ValidSuiteName(suite) {
				return usagef("green set: %q is not a valid suite name: a suite names a reference component, without a slash", suite)
			}
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

// greenSet moves the checkpoint of the branch to the commit, and publishes the
// tree the checkpoint certifies.
func greenSet(ctx context.Context, out io.Writer, o *options, f *greenFlags) error {
	repo, backend, root, err := greenRepo(ctx, o, f)
	if err != nil {
		return err
	}

	// The checkpoint names a commit whose tree was tested: the commit must
	// exist in the repository.
	commit, err := repo.ResolveRevision(ctx, f.commit)
	if err != nil {
		return fmt.Errorf("the commit %q: %w", f.commit, err)
	}

	// The tree the checkpoint certifies is the tree of the commit it names.
	// The workspace the command runs in is the one that was tested, and it
	// says whether that tree is the tree of the commit: a sparse workspace,
	// a workspace of another commit, or a workspace with a tracked file
	// changed tested something else.
	if root != "" {
		if err := greenWholeTree(root, backend, commit); err != nil {
			return err
		}
	}

	tree, err := repo.TreeID(ctx, commit)
	if err != nil {
		return fmt.Errorf("reading the tree of the commit %s: %w", commit, err)
	}

	name := greenref.Name(f.branch)
	treeName := greenref.TreeName(f.branch)
	previous, err := repo.ReferenceValue(ctx, name)
	if err != nil {
		return fmt.Errorf("reading the checkpoint of the branch %q: %w", f.branch, err)
	}
	previousTree, err := repo.ReferenceValue(ctx, treeName)
	if err != nil {
		return fmt.Errorf("reading the tree the checkpoint of the branch %q certifies: %w", f.branch, err)
	}

	// The tree moves first, then the suites, so that the checkpoint is the
	// last reference to move: a reader that sees the new checkpoint also sees
	// the tree it certifies and the suites that ran on it, and a reader that
	// sees only the tree sees a checkpoint that certifies no tree.
	updates := []sdk.ReferenceUpdate{{Name: treeName, Old: previousTree, New: tree}}
	for _, suite := range f.suites {
		suiteName := greenref.SuitesName(suite, f.branch)
		previousSuite, err := repo.ReferenceValue(ctx, suiteName)
		if err != nil {
			return fmt.Errorf("reading the commit the suite %q certified on the branch %q: %w", suite, f.branch, err)
		}
		updates = append(updates, sdk.ReferenceUpdate{Name: suiteName, Old: previousSuite, New: commit})
	}
	updates = append(updates, sdk.ReferenceUpdate{Name: name, Old: previous, New: commit})

	if err := repo.UpdateReferences(ctx, updates); err != nil {
		return fmt.Errorf("moving the checkpoint of the branch %q to %s: %w", f.branch, commit, err)
	}

	res := greenResult{
		Branch:        f.branch,
		Ref:           name,
		TreeRef:       treeName,
		Checkpoint:    commit,
		Previous:      previous,
		Tree:          tree,
		CertifiedTree: tree,
		FullTree:      true,
		Moved:         true,
	}
	for _, suite := range f.suites {
		res.Suites = append(res.Suites, greenSuite{Name: suite, Commit: commit})
	}
	res.Tip, _ = greenTip(ctx, repo, f.branch)
	res.Green = res.Tip != "" && res.Tip == res.Checkpoint

	return printGreen(out, o.jsonOut, res)
}

// greenWholeTree refuses a checkpoint that would certify a tree the suite did
// not test. The workspace the command runs in is the one that was tested.
func greenWholeTree(root string, backend workspace.Backend, commit string) error {
	ws, err := workspace.Open(root, backend)
	if err != nil {
		return err
	}

	summary := ws.Summary()
	if len(summary.Sparse) > 0 {
		return fmt.Errorf("the workspace holds only %s: a checkpoint would certify a tree the suite did not test. Test the whole tree (graft clone without --sparse), or publish the checkpoint from the workspace that was tested",
			strings.Join(summary.Sparse, ", "))
	}
	if summary.Base != commit {
		return fmt.Errorf("the workspace holds %s, not %s: publish the checkpoint from the workspace that was tested",
			summary.Base, commit)
	}

	// A tracked file that changed is a file the suite tested as it is on
	// disk, not as it is in the commit. A file the suite wrote is not part of
	// the tree of the commit, and says nothing about it.
	changes, err := ws.Status()
	if err != nil {
		return fmt.Errorf("reading the state of the workspace: %w", err)
	}
	for _, change := range changes {
		if change.Kind == workspace.ChangeModified || change.Kind == workspace.ChangeDeleted {
			return fmt.Errorf("the workspace changed the tracked file %s: the suite would test a tree that is not the tree of %s. Publish the checkpoint from a workspace that holds the commit as it is",
				change.Path, commit)
		}
	}

	return nil
}

// greenShow prints the checkpoint of the branch and the tip of the branch.
func greenShow(ctx context.Context, out io.Writer, o *options, f *greenFlags) error {
	repo, _, _, err := greenRepo(ctx, o, f)
	if err != nil {
		return err
	}

	name := greenref.Name(f.branch)
	checkpoint, err := repo.ReferenceValue(ctx, name)
	if err != nil {
		return fmt.Errorf("reading the checkpoint of the branch %q: %w", f.branch, err)
	}

	res := greenResult{Branch: f.branch, Ref: name, TreeRef: greenref.TreeName(f.branch), Checkpoint: checkpoint}
	res.CertifiedTree, err = repo.ReferenceValue(ctx, res.TreeRef)
	if err != nil {
		return fmt.Errorf("reading the tree the checkpoint of the branch %q certifies: %w", f.branch, err)
	}
	res.Suites, err = greenSuites(ctx, repo, f.branch)
	if err != nil {
		return err
	}
	if checkpoint != "" {
		res.Tree, _ = repo.TreeID(ctx, checkpoint)
	}
	res.FullTree = res.CertifiedTree != "" && res.CertifiedTree == res.Tree
	res.Tip, _ = greenTip(ctx, repo, f.branch)
	res.Green = res.Tip != "" && res.Tip == res.Checkpoint

	return printGreen(out, o.jsonOut, res)
}

// greenSuites returns the suites the checkpoint of the branch certifies: the
// rule of the branch says which suites it requires, and each of them has a
// reference that names the commit the suite ran on. A suite with no reference
// certifies no run of the suite.
func greenSuites(ctx context.Context, repo *sdk.Repo, branch string) ([]greenSuite, error) {
	rules, err := repo.BranchRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the rules of the server: %w", err)
	}

	name := strings.TrimPrefix(branch, "refs/heads/")
	var required []string
	for _, rule := range rules {
		if strings.TrimPrefix(rule.GetName(), "refs/heads/") == name {
			required = rule.GetRequireSuites()
			break
		}
	}

	var suites []greenSuite
	for _, suite := range required {
		commit, err := repo.ReferenceValue(ctx, greenref.SuitesName(suite, branch))
		if err != nil {
			return nil, fmt.Errorf("reading the commit the suite %q certified on the branch %q: %w", suite, branch, err)
		}
		suites = append(suites, greenSuite{Name: suite, Commit: commit})
	}
	return suites, nil
}

// greenRepo returns the repository the green command works on, the backend of
// the workspace and the root of the workspace (empty when there is none).
func greenRepo(ctx context.Context, o *options, f *greenFlags) (*sdk.Repo, workspace.Backend, string, error) {
	t, root, err := resolveTarget(o, f.repo, "green")
	if err != nil {
		return nil, nil, "", err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, nil, "", err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, nil, "", errors.New("green: the backend cannot read a checkpoint")
	}
	return sb.repo, backend, root, nil
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
	} else if res.Green {
		fmt.Fprintf(out, "the checkpoint of the branch %s is %s: the branch is green\n", res.Branch, res.Checkpoint)
	} else {
		fmt.Fprintf(out, "the checkpoint of the branch %s is %s: the branch is not green\n", res.Branch, res.Checkpoint)
	}

	switch {
	case res.FullTree:
		fmt.Fprintf(out, "the checkpoint certifies the whole tree of the commit %s\n", res.Checkpoint)
	case res.CertifiedTree != "":
		fmt.Fprintf(out, "the checkpoint certifies only the tree %s of the commit %s\n", res.CertifiedTree, res.Checkpoint)
	case res.Checkpoint != "":
		fmt.Fprintf(out, "the checkpoint certifies no tree of the commit %s\n", res.Checkpoint)
	}

	for _, suite := range res.Suites {
		if suite.Commit == "" {
			fmt.Fprintf(out, "the checkpoint certifies no run of the suite %s\n", suite.Name)
			continue
		}
		fmt.Fprintf(out, "the checkpoint certifies the suite %s on %s\n", suite.Name, suite.Commit)
	}

	return nil
}
