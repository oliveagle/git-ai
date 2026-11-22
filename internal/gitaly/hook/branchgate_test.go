package hook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/branchgate"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	branchGateOldOID  = "2222222222222222222222222222222222222222"
	branchGateNewOID  = "1111111111111111111111111111111111111111"
	branchGateZeroOID = "0000000000000000000000000000000000000000"
)

// branchGateStub is the state of a branch for the tests of the gate.
type branchGateStub struct {
	reviews    []*gitalypb.Review
	checkpoint string
	// suites are the suites the checkpoint certifies, keyed by name.
	suites map[string]string
	// asked records the suites a decision asked for, so that a test can
	// check the rule of the branch drove the question.
	asked []string
}

// BranchState implements BranchGateSource.
func (s *branchGateStub) BranchState(ctx context.Context, repo *gitalypb.Repository, branch string, suites []string) (BranchGateState, error) {
	s.asked = suites
	return BranchGateState{Reviews: s.reviews, Checkpoint: s.checkpoint, Suites: s.suites}, nil
}

// branchGateEnv returns the hook environment of an update performed by actor.
// When land is not empty, the update carries the identities of a land on the
// branch.
func branchGateEnv(t *testing.T, actor string, agent bool, land []changelog.Land) []string {
	t.Helper()

	meta := gitcmd.ChangelogMeta{Actor: actor, Agent: agent}
	if len(land) > 0 {
		meta.LandReference = []byte("refs/heads/main")
		meta.Lands = land
	}

	payload := map[string]any{
		"graft_changelog": map[string]any{
			"actor": actor,
			"agent": agent,
		},
	}
	if len(land) > 0 {
		encoded, err := json.Marshal(meta)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(encoded, &fields))
		payload["graft_changelog"] = fields
	}

	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	return []string{gitcmd.EnvHooksPayload + "=" + base64.StdEncoding.EncodeToString(encoded)}
}

// branchGateChanges returns the reference-transaction hook input of one update.
func branchGateChanges(oldOID, newOID, reference string) []byte {
	return []byte(fmt.Sprintf("%s %s %s\n", oldOID, newOID, reference))
}

// branchGateManager returns a hook manager that decides with the branch rules.
func branchGateManager(t *testing.T, graft config.Graft, source BranchGateSource) *GitLabHookManager {
	t.Helper()

	m := &GitLabHookManager{
		cfg:          config.Cfg{Graft: graft},
		logger:       testhelper.SharedLogger(t),
		branchPolicy: branchgate.NewStore(graft),
	}
	m.branchGate = source
	return m
}

// branchGateReview returns a review of the change on the branch.
func branchGateReview(head, base string) *gitalypb.Review {
	return &gitalypb.Review{
		Id:           "1",
		TargetBranch: []byte("main"),
		HeadCommitId: head,
		BaseCommitId: base,
		Author:       "author",
		Files:        []*gitalypb.ReviewFile{{Path: []byte("main.go")}},
		Verdicts: []*gitalypb.ReviewVerdict{
			{Actor: "reviewer", Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, Current: true},
		},
	}
}

func TestEnforceBranchGate(t *testing.T) {
	t.Parallel()

	review := branchGateReview("commit", branchGateOldOID)

	for _, tc := range []struct {
		desc     string
		graft    config.Graft
		source   BranchGateSource
		actor    string
		agent    bool
		land     []changelog.Land
		changes  []byte
		wantCode codes.Code
	}{
		{
			desc:     "a branch without a rule is not governed",
			graft:    config.Graft{},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "a branch without a rule is not governed by another branch's rule",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "release/1.0"}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "an agent may not push to a managed branch",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "an agent may push to a managed branch that allows direct updates",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main", AllowDirectUpdate: true}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "the operator is not bound by the branch rules",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "shared",
			agent:    false,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "an agent may not create a managed branch",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateZeroOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "an agent may not delete a managed branch",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateZeroOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "an agent may land a change without a review when the rule asks for none",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc: "an agent may not land a change without a review when the rule asks for one",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireReview: true},
			}},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc: "an agent may land a change with an approved review",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireReview: true},
			}},
			source:   &branchGateStub{reviews: []*gitalypb.Review{review}},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc: "an agent may not land a change whose review is based on an older tip",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireReview: true},
			}},
			source: &branchGateStub{reviews: []*gitalypb.Review{
				branchGateReview("commit", "older"),
			}},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc: "an agent may not land a change on a branch that is not verified",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireVerified: true},
			}},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc: "an agent may land a change on a branch that is verified",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireVerified: true},
			}},
			source:   &branchGateStub{checkpoint: branchGateOldOID},
			actor:    "agent-a",
			agent:    true,
			land:     []changelog.Land{{Original: "commit", Landed: "landed"}},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc: "every change of a batch needs its own review",
			graft: config.Graft{Branches: []config.GraftBranchRule{
				{Name: "main", RequireReview: true},
			}},
			source: &branchGateStub{reviews: []*gitalypb.Review{review}},
			actor:  "agent-a",
			agent:  true,
			land: []changelog.Land{
				{Original: "commit", Landed: "landed"},
				{Original: "other", Landed: "other-landed"},
			},
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "a reference outside refs/heads is not governed",
			graft:    config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}},
			actor:    "agent-a",
			agent:    true,
			changes:  branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/tags/v1"),
			wantCode: codes.OK,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			m := branchGateManager(t, tc.graft, tc.source)

			err := m.enforceBranchGate(context.Background(), gitcmd.HooksPayload{}, branchGateEnv(t, tc.actor, tc.agent, tc.land), tc.changes)
			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
		})
	}
}

// TestEnforceBranchGate_reasonNamesTheRule checks that the refusal of the gate
// says what the policy refused, so that the caller of the RPC can tell it from
// another failure.
func TestEnforceBranchGate_reasonNamesTheRule(t *testing.T) {
	t.Parallel()

	m := branchGateManager(t, config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}}}, nil)

	err := m.enforceBranchGate(context.Background(), gitcmd.HooksPayload{},
		branchGateEnv(t, "agent-a", true, nil),
		branchGateChanges(branchGateOldOID, branchGateNewOID, "refs/heads/main"))

	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.True(t, strings.Contains(err.Error(), "main"), "error: %v", err)
}
