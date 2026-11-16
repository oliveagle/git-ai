package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MirrorGateState is the state of the mirrors of a repository, at the commit
// a push writes.
type MirrorGateState struct {
	// Targets are the mirror targets of the repository, with the state of
	// their last publication and how far they are behind the commit.
	Targets []mirrorgate.Target
}

// MirrorGateSource gives the mirror gate the state of the mirrors of a
// repository. The server wires one that reads the targets of the repository
// and measures them against the local repository (internal/cli/gitaly). A nil
// source leaves the gate without state, and with the policy required every
// push is refused, because the repository has no mirror.
type MirrorGateSource interface {
	// MirrorState returns the state of the mirrors of the repository for a
	// push that writes tip to branch, without the refs/heads/ prefix. An empty
	// tip asks about the branch as it is now.
	MirrorState(ctx context.Context, repo *gitalypb.Repository, branch, tip string) (MirrorGateState, error)
}

// MirrorGateDenialError is the refusal of a push by the mirror policy of the
// server. It carries the decision, so that the caller can tell a policy
// decision from a failure and tell what the pusher has to repair.
type MirrorGateDenialError struct {
	// Branch is the branch the push writes, without the refs/heads/ prefix.
	Branch string
	// Code is the code of the decision (mirrorgate.CodeNoTarget, say).
	Code string
	// Reason explains the refusal for a human: what is missing, and the
	// command that repairs it.
	Reason string
}

func (e MirrorGateDenialError) Error() string {
	return e.Reason
}

// mirrorGateSource reads the mirror state of a repository: the targets of the
// storage core, or the files of the git directory when the storage core is
// off, and the commits the mirrors hold, measured against the local
// repository.
type mirrorGateSource struct {
	core    *corestore.Core
	factory localrepo.Factory
}

// NewMirrorGateSource returns the source of the state of the mirrors of a
// repository. The core may be nil: without it the state is kept in the files
// of the repository.
func NewMirrorGateSource(core *corestore.Core, factory localrepo.Factory) MirrorGateSource {
	return &mirrorGateSource{core: core, factory: factory}
}

// MirrorState implements MirrorGateSource.
func (s *mirrorGateSource) MirrorState(ctx context.Context, repo *gitalypb.Repository, branch, tip string) (MirrorGateState, error) {
	store, key, err := MirrorStore(ctx, s.core, s.factory, repo)
	if err != nil {
		return MirrorGateState{}, err
	}

	targets, err := store.ListTargets(ctx, key)
	if err != nil {
		return MirrorGateState{}, err
	}
	syncs, err := store.ListSyncs(ctx, key)
	if err != nil {
		return MirrorGateState{}, err
	}

	byName := make(map[string]mirrorstate.Sync, len(syncs))
	for _, sync := range syncs {
		byName[sync.Target] = sync
	}

	local := s.factory.Build(repo)
	if tip == "" && branch != "" {
		oid, err := local.ResolveReferenceExactly(ctx, git.ReferenceName("refs/heads/"+branch))
		if err != nil {
			if !errors.Is(err, git.ErrReferenceNotFound) {
				return MirrorGateState{}, fmt.Errorf("resolving the tip of the branch %q: %w", branch, err)
			}
		} else {
			tip = oid.String()
		}
	}

	state := MirrorGateState{Targets: make([]mirrorgate.Target, 0, len(targets))}
	for _, target := range targets {
		decided := mirrorgate.Target{
			Name:     target.Name,
			URL:      mirrorDisplayURL(target.URL),
			Branches: target.Branches,
			Enabled:  target.Enabled,
		}
		if sync, ok := byName[target.Name]; ok {
			decided.Synced = sync.Synced()
			decided.Status = sync.Status
			decided.PublishedAt = sync.PublishedAt
			decided.Error = sync.Error
			decided.Published = sync.Branches
			lag, diverged, err := mirrorLag(ctx, local, branch, tip, sync)
			if err != nil {
				return MirrorGateState{}, err
			}
			decided.Lag = lag
			decided.Diverged = diverged
		}
		state.Targets = append(state.Targets, decided)
	}

	return state, nil
}

// MirrorStore returns the store of the mirror state of the repository, and
// its key: the storage core's database when the core is on, the files of the
// git directory of the repository otherwise.
func MirrorStore(ctx context.Context, core *corestore.Core, factory localrepo.Factory, repo *gitalypb.Repository) (mirrorstate.Store, mirrorstate.Key, error) {
	if core != nil && core.Mirrors != nil {
		return core.Mirrors, mirrorstate.Key{
			Storage:      repo.GetStorageName(),
			RelativePath: corestore.NormalizeRelativePath(repo.GetRelativePath()),
		}, nil
	}
	if core != nil && core.Authoritative() {
		// The state of a mirror cannot live in a file of one node when the
		// repositories live in the storage core: the next push would be
		// decided by the state of whichever node it reached, and the mirror
		// policy would hold on one node and not on the next. A server that
		// runs the storage core is misconfigured without this store, and it
		// says so instead of writing a file nobody else reads.
		return nil, mirrorstate.Key{}, fmt.Errorf("the authoritative storage core has no mirror store")
	}

	path, err := factory.Build(repo).Path(ctx)
	if err != nil {
		return nil, mirrorstate.Key{}, fmt.Errorf("repository path: %w", err)
	}

	return mirrorstate.Open(filepath.Clean(path)), mirrorstate.Key{Path: path}, nil
}

// mirrorLag returns how many commits of the branch the mirror does not hold,
// and whether the mirror holds the branch at a commit the repository does not
// have.
//
// Only the commit the mirror published for this very branch is compared: a
// commit of another branch says nothing about this one, and a branch the
// mirror never published is not behind — there is nothing to compare it with,
// and the next publication takes the whole branch. Measuring against every
// commit the mirror holds would make every new branch inherit the lag of the
// mainline it was cut from: a repository whose mirror is behind would then
// take no branch at all, not even the temporary branch of a commit that is not
// landed yet, and the lag could not be repaired through that repository.
func mirrorLag(ctx context.Context, local *localrepo.Repo, branch, tip string, sync mirrorstate.Sync) (int, bool, error) {
	if tip == "" || branch == "" {
		return 0, false, nil
	}

	mirrored := sync.Branches[branch]
	if mirrored == "" {
		return 0, false, nil
	}

	known, err := local.HasRevision(ctx, git.Revision(mirrored))
	if err != nil {
		return 0, false, fmt.Errorf("reading the commit %s the mirror holds: %w", mirrored, err)
	}
	if !known {
		// The mirror holds a commit the repository does not have: the branch
		// was rewritten, or the mirror was published elsewhere.
		return 0, true, nil
	}

	ancestor, err := local.IsAncestor(ctx, git.Revision(mirrored), git.Revision(tip))
	if err != nil {
		return 0, false, fmt.Errorf("comparing the mirror of the branch %q: %w", branch, err)
	}
	if !ancestor {
		return 0, true, nil
	}

	lag, err := commitsNotOn(ctx, local, tip, mirrored)
	if err != nil {
		return 0, false, err
	}
	return lag, false, nil
}

// commitsNotOn returns how many commits tip reaches that the published commit
// of the mirror does not.
func commitsNotOn(ctx context.Context, local *localrepo.Repo, tip, published string) (int, error) {
	var stdout, stderr bytes.Buffer
	if err := local.ExecAndWait(ctx, gitcmd.Command{
		Name:  "rev-list",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--count"}},
		Args:  []string{tip, "^" + published},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr)); err != nil {
		return 0, structerr.New("rev-list --count: %w", err).WithMetadata("stderr", stderr.String())
	}

	lag, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		return 0, fmt.Errorf("reading the number of commits the mirror does not hold: %w", err)
	}
	return lag, nil
}

// enforceMirrorGate decides the updates of the transaction with the mirror
// policy of the server ([[graft.mirror]]). It runs in the prepared phase,
// while the references are locked and before git writes them, so that a
// refusal aborts the transaction whatever asked for the update.
//
// A push to a branch the repository does not publish is a warning, not a
// refusal: a repository publishes its mainline and still takes the branches a
// person works on. The warning is logged, and graft mirror status shows it.
func (m *GitLabHookManager) enforceMirrorGate(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) error {
	policy := m.cfg.Graft.Mirror.Policy()
	if !policy.Enabled() {
		return nil
	}
	if m.mirrorGate == nil {
		// Fail closed: a policy that refuses a repository without a mirror
		// must not open every repository because the source is not wired.
		return structerr.NewInternal("the mirror policy is in force, but the server has no source of the mirror state")
	}

	repository := payload.Repo.GetRelativePath()

	for _, update := range changelog.ParseUpdates(changes) {
		branch, ok := strings.CutPrefix(update.Reference, "refs/heads/")
		if !ok {
			continue
		}
		if changelog.IsZeroOID(update.NewOID) {
			// A deletion publishes nothing: the mirror keeps the branch until
			// it is published again, and the gate is about the history a push
			// adds.
			continue
		}

		state, err := m.mirrorGate.MirrorState(ctx, payload.Repo, branch, update.NewOID)
		if err != nil {
			return fmt.Errorf("reading the mirrors of the repository %s: %w", repository, err)
		}

		decision := mirrorgate.Decide(mirrorgate.Input{
			Policy:     policy,
			Repository: repository,
			Branch:     branch,
			Targets:    state.Targets,
		})

		fields := log.Fields{
			"repository": repository,
			"branch":     branch,
			"code":       decision.Code,
		}
		for _, target := range state.Targets {
			fields["mirror_"+target.Name+"_lag"] = target.Lag
		}

		if !decision.Allowed {
			fields["reason"] = decision.Reason
			m.logger.WithFields(fields).InfoContext(ctx, "push denied: the mirror is not up to date")
			return structerr.NewPermissionDenied("%w", MirrorGateDenialError{
				Branch: branch,
				Code:   decision.Code,
				Reason: mirrorGateReason(decision),
			})
		}

		if decision.Warning != "" {
			fields["warning"] = decision.Warning
			m.logger.WithFields(fields).WarnContext(ctx, "the mirror of the repository is behind")
			// The pusher reads the warning where it reads a refusal: neither
			// has a side band, and both come back in the response of the hook.
			TransactionMessagesFrom(ctx).Add(decision.Warning)
		} else {
			m.logger.WithFields(fields).DebugContext(ctx, "the mirror of the repository is up to date")
		}
	}

	return nil
}

// mirrorGateReason returns the reason of a refusal, with the command that shows
// the operator every mirror of the repository: the pusher of a branch usually
// cannot repair a mirror from the push.
func mirrorGateReason(decision mirrorgate.Decision) string {
	return decision.Reason + `; "graft mirror status" shows every mirror of the repository, how far it is behind, and the thresholds of the policy`
}

// mirrorDisplayURL hides the user information of a URL that a person may have
// put a credential in: the reason of a refusal is logged and shown.
func mirrorDisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
