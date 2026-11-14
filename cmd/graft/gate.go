package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
)

// gateSynopsis is the command line of graft gate.
const gateSynopsis = "usage: graft gate check --branch BRANCH [--commit OID] [--result OID] [--direct] [--repo REPO] [--json]\n" +
	"       graft gate rules [--repo REPO] [--json]"

// gateUsage lists the subcommands of graft gate.
const gateUsage = "gate requires a subcommand: check or rules (" + gateSynopsis + ")"

// gateHelp explains graft gate below its synopsis.
const gateHelp = `Asks the branch management policy of the server whether a change may land on a
branch, and prints the rules the server enforces.

- The rules are the ones the server enforces ([[graft.branch]], see
  BRANCHES.md), read from the server, so that the check cannot be made with a
  copy of the policy that differs from the one in force. graft gate rules
  prints them.
- The state the answer is made with is the state the server decides with: the
  tip of the branch, its open reviews and its verification checkpoint. The
  answer is therefore the one the reference-transaction hook makes when the
  change lands, and a CI can use the exit status as the status check of a
  merge request. graft green set publishes the checkpoint.
- gate check asks about a change: --commit C is the change that would land, the
  commit the review is about. Without it the pending commit of the workspace is
  used, and without a workspace the state of the branch alone is checked.
- --result M is the commit the land would produce, as a dry-run land returned
  it (UserLand with dry_run, sdk.LandOptions.DryRun). The verification
  checkpoint of the branch may cover that commit instead of its tip: that is a
  merge queue, which asks what the land would produce, tests that commit,
  publishes it with graft green set, and lands exactly it.
- A branch whose rule asks for a verified result (require_verified_result, the
  rule of a merge queue) takes only a land whose own result the checkpoint
  covers, so --result is required there: without it the answer is
  result_unknown, and with a checkpoint on anything but that commit it is
  not_verified. A result the branch would no longer write, because the branch
  moved after it was computed, is stale_result: the land has to be computed
  again, and a queue that reads the code knows it before it runs the suite.
- --direct asks about an update that is not a land, such as a push.

Exit status: 0 when the policy allows the change, 1 when it refuses it (the
reason is printed), 2 when the command line is wrong.`

// gateFlags holds the flags of graft gate.
type gateFlags struct {
	branch string
	commit string
	result string
	direct bool
	repo   string
}

// gateRule is one branch rule, as the command prints it.
type gateRule struct {
	// Name is the branch the rule governs, without refs/heads/.
	Name string `json:"name"`
	// RequireReview demands a review of the change that lands on the branch.
	RequireReview bool `json:"require_review"`
	// RequireApprovals is the number of approvals the review must carry.
	RequireApprovals int32 `json:"require_approvals"`
	// AllowStaleBase accepts a review based on an older tip.
	AllowStaleBase bool `json:"allow_stale_base"`
	// AllowDirectUpdate accepts an update that is not a land.
	AllowDirectUpdate bool `json:"allow_direct_update"`
	// RequireVerified demands a checkpoint that covers the tip.
	RequireVerified bool `json:"require_verified"`
	// RequireVerifiedResult demands a checkpoint that covers the commit the
	// update produces: the rule of a merge queue.
	RequireVerifiedResult bool `json:"require_verified_result"`
}

// gateResult is the JSON output of graft gate check.
type gateResult struct {
	// Branch is the branch the change would land on.
	Branch string `json:"branch"`
	// Managed tells whether the branch has a rule.
	Managed bool `json:"managed"`
	// Commit is the commit that would land, empty when the check is about the
	// state of the branch alone.
	Commit string `json:"commit,omitempty"`
	// Direct tells whether the check is about an update that is not a land.
	Direct bool `json:"direct,omitempty"`
	// Result is the commit the land would produce, when the caller knows it.
	Result string `json:"result,omitempty"`
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
	// Rule is the rule of the branch, empty when the branch is not managed.
	Rule *gateRule `json:"rule,omitempty"`
}

// gateRulesResult is the JSON output of graft gate rules.
type gateRulesResult struct {
	// Rules are the branch rules the server enforces.
	Rules []gateRule `json:"rules"`
}

// gateCommand returns the gate command, the branch management policy of the
// server as a check.
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
			fs.StringVar(&f.result, "result", "", "commit the land would produce, as a dry-run land returned it: the checkpoint may cover it")
			fs.BoolVar(&f.direct, "direct", false, "ask about an update that is not a land, such as a push")
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.BoolVar(&o.jsonOut, "json", false, "print the decision as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdGate(ctx, out, o, f, args)
		},
	}
}

// cmdGate dispatches a gate subcommand. The command line is checked before the
// server is contacted.
func cmdGate(ctx context.Context, out io.Writer, o *options, f *gateFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", gateUsage)
	}
	if len(args) != 1 {
		return usagef("%s", gateSynopsis)
	}

	switch args[0] {
	case "check":
		if f.branch == "" {
			return usagef("gate check requires --branch B")
		}
		return gateCheck(ctx, out, o, f)
	case "rules":
		return gateRules(ctx, out, o, f)
	default:
		return usagef("unknown gate subcommand %q: %s", args[0], gateUsage)
	}
}

// gateCheck asks the server whether the change may land, and prints the
// answer.
func gateCheck(ctx context.Context, out io.Writer, o *options, f *gateFlags) error {
	repo, backend, root, err := gateRepo(ctx, o, f)
	if err != nil {
		return err
	}

	commit := f.commit
	if commit == "" && !f.direct && root != "" {
		commit, err = gatePendingCommit(root, backend)
		if err != nil {
			return err
		}
	}

	opts := sdk.BranchDecisionOptions{Branch: f.branch, Direct: f.direct}
	if commit != "" {
		opts.Commits = []string{commit}
	}
	opts.Result = f.result

	answer, err := repo.BranchDecision(ctx, opts)
	if err != nil {
		return fmt.Errorf("asking the branch policy of the server: %w", err)
	}

	res := gateResult{
		Branch:     answer.GetBranch(),
		Managed:    answer.GetManaged(),
		Commit:     commit,
		Direct:     f.direct,
		Result:     f.result,
		Tip:        answer.GetTip(),
		Checkpoint: answer.GetCheckpoint(),
		Reviews:    int(answer.GetReviews()),
		Allowed:    answer.GetAllowed(),
		Code:       answer.GetCode(),
		Reason:     answer.GetReason(),
		Rule:       ruleOf(answer.GetRule()),
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

// gateRules prints the branch rules the server enforces.
func gateRules(ctx context.Context, out io.Writer, o *options, f *gateFlags) error {
	repo, _, _, err := gateRepo(ctx, o, f)
	if err != nil {
		return err
	}

	rules, err := repo.BranchRules(ctx)
	if err != nil {
		return fmt.Errorf("reading the branch rules of the server: %w", err)
	}

	res := gateRulesResult{}
	for _, rule := range rules {
		res.Rules = append(res.Rules, *ruleOf(rule))
	}

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if len(res.Rules) == 0 {
		fmt.Fprintln(out, "the server manages no branch: every branch is updated as any other reference")
		return nil
	}
	for i := range res.Rules {
		fmt.Fprintf(out, "%s: %s\n", res.Rules[i].Name, describeGateRule(&res.Rules[i]))
	}
	return nil
}

// gateRepo returns the repository the gate command works on, the backend of
// the workspace and the root of the workspace (empty when there is none).
func gateRepo(ctx context.Context, o *options, f *gateFlags) (*sdk.Repo, workspace.Backend, string, error) {
	t, root, err := resolveTarget(o, f.repo, "gate")
	if err != nil {
		return nil, nil, "", err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, nil, "", err
	}
	// The widening is safe: the command line checks that gate runs on the SDK
	// backend when it is registered.
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, nil, "", errors.New("gate: the backend cannot check a branch")
	}
	return sb.repo, backend, root, nil
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

// ruleOf returns the rule as the command prints it, nil when the branch is not
// managed.
func ruleOf(rule *sdk.BranchRule) *gateRule {
	if rule == nil {
		return nil
	}
	return &gateRule{
		Name:              rule.GetName(),
		RequireReview:     rule.GetRequireReview(),
		RequireApprovals:  rule.GetRequireApprovals(),
		AllowStaleBase:    rule.GetAllowStaleBase(),
		AllowDirectUpdate: rule.GetAllowDirectUpdate(),
		RequireVerified:   rule.GetRequireVerified(),
		// The stricter rule of a merge queue, so that a reader of the rules
		// sees what the server will refuse.
		RequireVerifiedResult: rule.GetRequireVerifiedResult(),
	}
}

// describeGateRule describes a rule for a reader.
func describeGateRule(rule *gateRule) string {
	var parts []string
	if rule.AllowDirectUpdate {
		parts = append(parts, "a push may update it")
	} else {
		parts = append(parts, "only a land may update it")
	}
	if rule.RequireReview {
		parts = append(parts, "a review is required")
	}
	if rule.RequireApprovals > 0 {
		parts = append(parts, fmt.Sprintf("%d approval(s) are required", rule.RequireApprovals))
	}
	if rule.AllowStaleBase {
		parts = append(parts, "a review based on an older tip is accepted")
	}
	if rule.RequireVerifiedResult {
		parts = append(parts, "the verification checkpoint must cover the commit the land produces")
	} else if rule.RequireVerified {
		parts = append(parts, "the verification checkpoint must cover the tip")
	}
	return strings.Join(parts, "; ")
}

// printGateCheck prints the answer of graft gate check for a reader.
func printGateCheck(out io.Writer, res gateResult) {
	subject := "the branch " + res.Branch
	switch {
	case res.Direct && res.Commit != "":
		subject = "the direct update of " + res.Branch + " to " + res.Commit
	case res.Direct:
		subject = "a direct update of " + res.Branch
	case res.Commit != "":
		subject = "the change " + res.Commit
	}

	if !res.Managed {
		fmt.Fprintf(out, "%s is not managed: the policy does not govern it\n", subject)
		return
	}

	verb := "land on"
	if res.Direct {
		verb = "update"
	}

	if res.Allowed {
		fmt.Fprintf(out, "%s may %s %s\n", subject, verb, res.Branch)
		return
	}

	fmt.Fprintf(out, "%s may not %s %s: %s\n", subject, verb, res.Branch, res.Reason)
}
