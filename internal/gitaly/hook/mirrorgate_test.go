package hook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	mirrorGateOldOID  = "2222222222222222222222222222222222222222"
	mirrorGateNewOID  = "1111111111111111111111111111111111111111"
	mirrorGateZeroOID = "0000000000000000000000000000000000000000"
)

// mirrorGateStub is the state of the mirrors for the tests of the gate.
type mirrorGateStub struct {
	targets []mirrorgate.Target
	err     error
	// asked records the branches the gate asked about, so that a test can
	// check the gate asked once per branch the transaction writes.
	asked []string
}

// MirrorState implements MirrorGateSource.
func (s *mirrorGateStub) MirrorState(ctx context.Context, repo *gitalypb.Repository, branch, tip string) (MirrorGateState, error) {
	s.asked = append(s.asked, branch)
	return MirrorGateState{Targets: s.targets}, s.err
}

// mirrorGateManager returns a hook manager that decides with the mirror
// policy of graft.
func mirrorGateManager(t *testing.T, graft config.Graft, source MirrorGateSource) *GitLabHookManager {
	t.Helper()

	m := &GitLabHookManager{
		cfg:    config.Cfg{Graft: graft},
		logger: testhelper.SharedLogger(t),
	}
	m.mirrorGate = source
	return m
}

// mirrorGatePolicy is the policy the tests turn on.
func mirrorGatePolicy() config.Graft {
	return config.Graft{Mirror: config.GraftMirror{Required: true, WarnLag: 10}}
}

// mirrorGateTarget returns a target that published successfully, behind by
// lag commits.
func mirrorGateTarget(name string, branches []string, lag int) mirrorgate.Target {
	return mirrorgate.Target{
		Name:     name,
		URL:      "https://example.com/" + name + ".git",
		Branches: branches,
		Enabled:  true,
		Synced:   true,
		Status:   "ok",
		Lag:      lag,
	}
}

func TestEnforceMirrorGate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		graft    config.Graft
		source   MirrorGateSource
		changes  []byte
		wantCode codes.Code
	}{
		{
			desc:     "a server without the policy takes the push",
			graft:    config.Graft{},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "a repository without a mirror target takes no push",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:  "a mirror that never published takes no push",
			graft: mirrorGatePolicy(),
			source: &mirrorGateStub{targets: []mirrorgate.Target{{
				Name:     "codeup",
				URL:      "https://example.com/codeup.git",
				Branches: []string{"*"},
				Enabled:  true,
			}}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "a published mirror takes the push",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"*"}, 0)}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "a mirror behind the warning threshold takes the push",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"*"}, 10)}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "a mirror behind the refusal threshold takes no push",
			graft:    config.Graft{Mirror: config.GraftMirror{Required: true, WarnLag: 10, DenyLag: 20}},
			source:   &mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"*"}, 20)}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "a diverged mirror takes no push",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"*"}, 0)}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
		{
			desc:     "a branch no target publishes takes the push",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"main"}, 0)}},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/topic"),
			wantCode: codes.OK,
		},
		{
			desc:     "a reference outside refs/heads is not governed",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/tags/v1"),
			wantCode: codes.OK,
		},
		{
			desc:     "a deletion publishes nothing and is not governed",
			graft:    mirrorGatePolicy(),
			source:   &mirrorGateStub{},
			changes:  branchGateChanges(mirrorGateOldOID, mirrorGateZeroOID, "refs/heads/main"),
			wantCode: codes.OK,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			m := mirrorGateManager(t, tc.graft, tc.source)

			err := m.enforceMirrorGate(context.Background(), gitcmd.HooksPayload{}, tc.changes)
			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
		})
	}
}

// TestEnforceMirrorGate_diverged checks that a mirror that holds a commit the
// repository does not have refuses the push, whatever the lag says.
func TestEnforceMirrorGate_diverged(t *testing.T) {
	t.Parallel()

	target := mirrorGateTarget("codeup", []string{"*"}, 0)
	target.Diverged = true
	m := mirrorGateManager(t, mirrorGatePolicy(), &mirrorGateStub{targets: []mirrorgate.Target{target}})

	err := m.enforceMirrorGate(context.Background(), gitcmd.HooksPayload{},
		branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"))

	require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)

	var denial MirrorGateDenialError
	require.True(t, errors.As(err, &denial), "error: %v", err)
	require.Equal(t, mirrorgate.CodeDiverged, denial.Code)
	require.Equal(t, "main", denial.Branch)
}

// TestEnforceMirrorGate_denialNamesWhatToRepair checks that the refusal tells
// the pusher the command that repairs it, and does not leak a credential of
// the mirror URL (the reason is shown to the pusher).
func TestEnforceMirrorGate_denialNamesWhatToRepair(t *testing.T) {
	t.Parallel()

	m := mirrorGateManager(t, mirrorGatePolicy(), &mirrorGateStub{})

	err := m.enforceMirrorGate(context.Background(), gitcmd.HooksPayload{},
		branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"))

	require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)

	var denial MirrorGateDenialError
	require.True(t, errors.As(err, &denial), "error: %v", err)
	require.Equal(t, mirrorgate.CodeNoTarget, denial.Code)
	require.True(t, strings.Contains(denial.Reason, "graft mirror queue add"), "reason: %s", denial.Reason)
	require.True(t, strings.Contains(denial.Reason, "graft mirror status"), "reason: %s", denial.Reason)
}

// TestEnforceMirrorGate_warningAllowsThePush checks that a mirror behind the
// warning threshold but not the refusal one does not stop the push.
func TestEnforceMirrorGate_warningAllowsThePush(t *testing.T) {
	t.Parallel()

	m := mirrorGateManager(t, mirrorGatePolicy(),
		&mirrorGateStub{targets: []mirrorgate.Target{mirrorGateTarget("codeup", []string{"*"}, 12)}})

	messages := &TransactionMessages{}
	ctx := WithTransactionMessages(context.Background(), messages)

	require.NoError(t, m.enforceMirrorGate(ctx, gitcmd.HooksPayload{},
		branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main")))

	// The warning reaches the pusher, which otherwise learns nothing about
	// the mirror that is behind.
	require.Len(t, messages.All(), 1)
	require.Contains(t, messages.All()[0], "12 commits behind")
}

// TestEnforceMirrorGate_noSourceFailsClosed checks that a server that turns
// the policy on without wiring the source refuses the push rather than
// opening every repository.
func TestEnforceMirrorGate_noSourceFailsClosed(t *testing.T) {
	t.Parallel()

	m := mirrorGateManager(t, mirrorGatePolicy(), nil)

	err := m.enforceMirrorGate(context.Background(), gitcmd.HooksPayload{},
		branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"))

	require.Equal(t, codes.Internal, status.Code(err), "error: %v", err)
}

// TestEnforceMirrorGate_sourceError checks that a failure to read the mirrors
// stops the push: the gate cannot allow what it cannot see.
func TestEnforceMirrorGate_sourceError(t *testing.T) {
	t.Parallel()

	m := mirrorGateManager(t, mirrorGatePolicy(), &mirrorGateStub{err: errors.New("the database is down")})

	err := m.enforceMirrorGate(context.Background(), gitcmd.HooksPayload{},
		branchGateChanges(mirrorGateOldOID, mirrorGateNewOID, "refs/heads/main"))

	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
}
