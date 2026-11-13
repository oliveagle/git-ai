package hook

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/graft/branchgate"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// BranchGateState is the state the branch gate decides with, for one branch.
type BranchGateState struct {
	// Reviews are the reviews of the branch.
	Reviews []*gitalypb.Review
	// Checkpoint is the commit the verification checkpoint of the branch points
	// at, the green reference of graft-verify. It is empty when the branch has
	// no checkpoint.
	Checkpoint string
}

// BranchGateSource gives the branch gate the state of a branch. The server
// wires one that reads the reviews of the repository and the verification
// checkpoint of the branch (internal/cli/gitaly). A nil source leaves the
// gate without reviews and without a checkpoint, so that a rule that asks for
// one refuses every land.
type BranchGateSource interface {
	// BranchState returns the state of the branch of the repository.
	BranchState(ctx context.Context, repo *gitalypb.Repository, branch string) (BranchGateState, error)
}

// enforceBranchGate decides the updates of the transaction with the branch
// management policy of the server ([[graft.branch]]). It runs in the prepared
// phase, while the references are locked and before git writes them, so that
// a refusal aborts the transaction whatever asked for the update.
//
// The gate binds the callers that authenticated with a per-agent token, like
// the access policy does: the operator credential (the shared token) and the
// server's own operations are trusted, so that the verifier may revert a
// commit that broke the branch. A CI that lands with the operator credential
// asks the same question with graft gate check, which the BranchPolicyService
// answers with this very policy (internal/gitaly/service/branchpolicy).
//
// A branch without a rule is not managed: its updates are not touched.
func (m *GitLabHookManager) enforceBranchGate(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) error {
	policy := m.branchPolicy
	if policy.Empty() {
		return nil
	}

	meta, err := gitcmd.ChangelogMetaFromEnv(env)
	if err != nil {
		if !errors.Is(err, gitcmd.ErrPayloadNotFound) {
			return fmt.Errorf("reading graft metadata: %w", err)
		}
		// The update carries no identity: it is not a land, and a managed
		// branch only a land may update.
		meta = gitcmd.ChangelogMeta{}
	}

	if !meta.Agent || meta.Actor == "" {
		return nil
	}

	for _, update := range changelog.ParseUpdates(changes) {
		branch, ok := strings.CutPrefix(update.Reference, "refs/heads/")
		if !ok {
			continue
		}
		if policy.Rule(branch) == nil {
			continue
		}

		decision, err := m.decideBranchUpdate(ctx, policy, payload, meta, branch, update)
		if err != nil {
			return err
		}

		fields := log.Fields{
			"actor":     meta.Actor,
			"branch":    branch,
			"reference": update.Reference,
			"code":      decision.Code,
		}
		if !decision.Allowed {
			fields["reason"] = decision.Reason
			m.logger.WithFields(fields).InfoContext(ctx, "agent branch update denied")
			return structerr.NewPermissionDenied("%s", decision.Reason)
		}
		m.logger.WithFields(fields).DebugContext(ctx, "agent branch update allowed")
	}

	return nil
}

// decideBranchUpdate decides one update of a managed branch with the rules of
// the server.
func (m *GitLabHookManager) decideBranchUpdate(ctx context.Context, policy *branchgate.Policy, payload gitcmd.HooksPayload, meta gitcmd.ChangelogMeta, branch string, update changelog.Update) (branchgate.Decision, error) {
	deletion := changelog.IsZeroOID(update.NewOID)
	creation := changelog.IsZeroOID(update.OldOID)

	// A land carries the identities of the changes it lands and the branch it
	// lands them on. An update without them is a direct update, such as a
	// push.
	land := !deletion && string(meta.LandReference) == update.Reference && len(meta.Lands) > 0

	var changes []string
	switch {
	case land:
		for _, l := range meta.Lands {
			changes = append(changes, l.Original)
		}
	case !creation:
		changes = []string{update.NewOID}
	}

	var reviews []*gitalypb.Review
	verified := false
	if m.branchGate != nil {
		state, err := m.branchGate.BranchState(ctx, payload.Repo, branch)
		if err != nil {
			return branchgate.Decision{}, fmt.Errorf("reading the state of the branch %q: %w", branch, err)
		}
		reviews = state.Reviews
		// The checkpoint covers the branch when it points at the tip the
		// update builds on: the branch is green.
		verified = state.Checkpoint != "" && state.Checkpoint == update.OldOID
	}

	return branchgate.Decide(branchgate.Input{
		Rules:       policy.Rules,
		ReviewRules: policy.ReviewRules,
		Branch:      branch,
		Changes:     changes,
		Tip:         update.OldOID,
		Reviews:     reviews,
		Verified:    verified,
		Direct:      !land,
		Creation:    creation,
		Deletion:    deletion,
	}), nil
}
