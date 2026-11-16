package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// pushBatchSize is the maximum number of branches to push in a single push call.
	pushBatchSize = 10
	// maxDivergentRefs is the maximum number of divergent refs to return in UpdateRemoteMirror's
	// response.
	maxDivergentRefs = 100
	// workspaceBranchPrefix is the namespace of the temporary branches of a
	// workspace. They are the server's bookkeeping of an unlanded change, not
	// branches of the repository, so a mirror does not publish them.
	workspaceBranchPrefix = "refs/heads/graft/"
)

func (s *server) UpdateRemoteMirror(stream gitalypb.RemoteService_UpdateRemoteMirrorServer) error {
	firstRequest, err := stream.Recv()
	if err != nil {
		return structerr.NewInternal("receive first request: %w", err)
	}

	if err = validateUpdateRemoteMirrorRequest(stream.Context(), s.locator, firstRequest); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := s.updateRemoteMirror(stream, firstRequest); err != nil {
		// The publication did not finish: the state of the mirror says so, so
		// that an operator reads what failed rather than a stale success.
		s.recordMirrorSync(stream.Context(), firstRequest.GetRepository(), firstRequest.GetTarget(), nil, false, err)
		return structerr.NewInternal("%w", err)
	}

	return nil
}

func (s *server) updateRemoteMirror(stream gitalypb.RemoteService_UpdateRemoteMirrorServer, firstRequest *gitalypb.UpdateRemoteMirrorRequest) error {
	ctx := stream.Context()

	branchMatchers := firstRequest.GetOnlyBranchesMatching()
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("receive: %w", err)
		}

		branchMatchers = append(branchMatchers, req.GetOnlyBranchesMatching()...)
	}

	referenceMatcher, err := newReferenceMatcher(branchMatchers)
	if err != nil {
		return fmt.Errorf("create reference matcher: %w", err)
	}

	repo := s.localRepoFactory.Build(firstRequest.GetRepository())
	remote := firstRequest.GetRemote()

	remoteSuffix, err := text.RandomHex(8)
	if err != nil {
		return fmt.Errorf("generating remote suffix: %w", err)
	}
	remoteName := "inmemory-" + remoteSuffix

	var remoteConfig []gitcmd.ConfigPair
	remoteURL := remote.GetUrl()

	if resolvedAddress := remote.GetResolvedAddress(); resolvedAddress != "" {
		modifiedURL, resolveConfig, err := gitcmd.GetURLAndResolveConfig(remoteURL, resolvedAddress)
		if err != nil {
			return fmt.Errorf("couldn't get curloptResolve config: %w", err)
		}

		remoteURL = modifiedURL
		remoteConfig = append(remoteConfig, resolveConfig...)
	}

	remoteConfig = append(remoteConfig, gitcmd.ConfigPair{
		Key: fmt.Sprintf("remote.%s.url", remoteName), Value: remoteURL,
	})

	if authHeader := remote.GetHttpAuthorizationHeader(); authHeader != "" {
		remoteConfig = append(remoteConfig, gitcmd.ConfigPair{
			Key:   fmt.Sprintf("http.%s.extraHeader", remote.GetUrl()),
			Value: "Authorization: " + authHeader,
		})
	}

	sshCommand, clean, err := gitcmd.BuildSSHInvocation(ctx, s.logger, firstRequest.GetSshKey(), firstRequest.GetKnownHosts())
	if err != nil {
		return fmt.Errorf("build ssh invocation: %w", err)
	}
	defer clean()

	remoteRefsSlice, err := repo.GetRemoteReferences(ctx, remoteName,
		localrepo.WithPatterns("refs/heads/*", "refs/tags/*"),
		localrepo.WithConfig(remoteConfig...),
		localrepo.WithSSHCommand(sshCommand),
	)
	if err != nil {
		return fmt.Errorf("get remote references: %w", err)
	}

	localRefs, err := repo.GetReferences(ctx, "refs/heads/", "refs/tags/")
	if err != nil {
		return fmt.Errorf("get local references: %w", err)
	}
	// A workspace's temporary branch is the server's bookkeeping of a change
	// that has not landed, not a branch of the repository. A mirror is the
	// repository, so it is not published — a "publish every branch" pattern
	// means every branch a person can name, not the drafts of a workspace.
	localRefs = slices.DeleteFunc(localRefs, func(ref git.Reference) bool {
		return strings.HasPrefix(ref.Name.String(), workspaceBranchPrefix)
	})

	// What this target published last time. A branch this target published,
	// and that the repository no longer has, left the repository: the mirror
	// carries what the repository holds, so the publication removes it. It is
	// not a stranger's work on the mirror — this server put it there — so the
	// guard that keeps divergent references does not apply to it.
	var publishedBefore map[string]string
	if target := firstRequest.GetTarget(); target != "" {
		if store, key, err := hook.MirrorStore(ctx, s.core, s.localRepoFactory, firstRequest.GetRepository()); err == nil {
			if syncs, err := store.ListSyncs(ctx, key); err == nil {
				for _, sync := range syncs {
					if sync.Target == target {
						publishedBefore = sync.Branches
					}
				}
			}
		}
	}

	defaultBranch, err := repo.HeadReference(ctx)
	if err != nil {
		return fmt.Errorf("get default branch: %w", err)
	}

	remoteRefs := make(map[git.ReferenceName]string, len(remoteRefsSlice))
	for _, ref := range remoteRefsSlice {
		if ref.IsSymbolic {
			// There should be no symbolic refs in refs/heads/ or refs/tags, so we'll just ignore
			// them if something has placed one there.
			continue
		}

		remoteRefs[ref.Name] = ref.Target
	}

	var defaultBranchExists bool
	var divergentRefs [][]byte
	// kept holds the branches the mirror keeps with history the repository
	// does not have, by reference: they are reported, and the state of the
	// mirror records the commit the mirror holds.
	kept := map[git.ReferenceName]string{}
	toUpdate := map[git.ReferenceName]string{}
	for _, localRef := range localRefs {
		if localRef.Name == defaultBranch {
			defaultBranchExists = true
		}

		if localRef.IsSymbolic {
			continue
		}

		remoteTarget, ok := remoteRefs[localRef.Name]
		if !ok {
			// ref does not exist on the mirror, it should be created
			toUpdate[localRef.Name] = localRef.Target
			delete(remoteRefs, localRef.Name)
			continue
		}

		if remoteTarget == localRef.Target {
			// ref is up to date on the mirror
			delete(remoteRefs, localRef.Name)
			continue
		}

		if firstRequest.GetKeepDivergentRefs() {
			isAncestor, err := repo.IsAncestor(ctx, git.Revision(remoteTarget), git.Revision(localRef.Target))
			if err != nil && !errors.Is(err, localrepo.InvalidCommitError(remoteTarget)) {
				return fmt.Errorf("checking for ancestry: %w", err)
			}

			if !isAncestor {
				// The mirror's reference has diverged from the local ref, or the mirror contains a commit
				// which is not present in the local repository.
				kept[localRef.Name] = remoteTarget
				if referenceMatcher.MatchString(localRef.Name.String()) && len(divergentRefs) < maxDivergentRefs {
					// diverged branches on the mirror are only included in the response if they match
					// one of the branches in the selector
					divergentRefs = append(divergentRefs, []byte(localRef.Name))
				}

				delete(remoteRefs, localRef.Name)
				continue
			}
		}

		// the mirror's ref does not match ours, we should update it.
		toUpdate[localRef.Name] = localRef.Target
		delete(remoteRefs, localRef.Name)
	}

	toDelete := remoteRefs
	if !defaultBranchExists || firstRequest.GetKeepDivergentRefs() {
		toDelete = map[git.ReferenceName]string{}
	}

	for remoteRef, remoteCommitOID := range toDelete {
		isAncestor, err := repo.IsAncestor(ctx, git.Revision(remoteCommitOID), defaultBranch.Revision())
		if err != nil && !errors.Is(err, localrepo.InvalidCommitError(remoteCommitOID)) {
			return fmt.Errorf("checking for default branch ancestry: %w", err)
		}

		if isAncestor {
			continue
		}

		// The commit in the extra branch in the remote repository has not been merged in to the
		// local repository's default branch. Keep it to avoid losing work.
		delete(toDelete, remoteRef)
	}

	// A branch this target published before, and that the repository no
	// longer has, goes with the branch: the mirror carries the branches of the
	// repository, and a person who deleted one did not mean to keep a copy on
	// every mirror. The ancestry guard above protects work that came from
	// somewhere else; this is refs this server published itself.
	local := make(map[git.ReferenceName]struct{}, len(localRefs))
	for _, localRef := range localRefs {
		local[localRef.Name] = struct{}{}
	}
	for branch := range publishedBefore {
		remoteRef := git.ReferenceName("refs/heads/" + branch)
		if _, stillLocal := local[remoteRef]; stillLocal {
			continue
		}
		remoteCommitOID, onRemote := remoteRefs[remoteRef]
		if !onRemote {
			continue
		}
		toDelete[remoteRef] = remoteCommitOID
	}

	var refspecs []string
	for prefix, references := range map[string]map[git.ReferenceName]string{
		"": toUpdate, ":": toDelete,
	} {
		for reference := range references {
			if !referenceMatcher.MatchString(reference.String()) {
				continue
			}

			refspecs = append(refspecs, prefix+reference.String())
			if reference == defaultBranch {
				// The default branch needs to be pushed in the first batch of refspecs as some features
				// depend on it existing in the repository. The default branch may not exist in the repo
				// yet if this is the first mirroring push.
				last := len(refspecs) - 1
				refspecs[0], refspecs[last] = refspecs[last], refspecs[0]
			}
		}
	}

	for len(refspecs) > 0 {
		batch := refspecs
		if len(refspecs) > pushBatchSize {
			batch = refspecs[:pushBatchSize]
		}

		refspecs = refspecs[len(batch):]

		if err := repo.Push(ctx, remoteName, batch, localrepo.PushOptions{
			SSHCommand: sshCommand,
			Force:      !firstRequest.GetKeepDivergentRefs(),
			Config:     remoteConfig,
		}); err != nil {
			return fmt.Errorf("push to mirror: %w", err)
		}
	}

	// The publication is what the mirror holds now: the record of it is what
	// the mirror policy of the server decides the next push with
	// (doc/graft/MIRROR.md).
	s.recordMirrorSync(ctx, firstRequest.GetRepository(), firstRequest.GetTarget(), publishedBranches(localRefs, referenceMatcher, kept), len(kept) > 0, nil)

	return stream.SendAndClose(&gitalypb.UpdateRemoteMirrorResponse{DivergentRefs: divergentRefs})
}

// newReferenceMatcher returns a regexp which matches references that should
// be updated in the mirror repository. Tags are always matched successfully.
// branchMatchers optionally contain patterns that are used to match branches.
// The patterns should only include the branch name without the `refs/heads/`
// prefix. "*" can be used as a wildcard in the patterns. If no branchMatchers
// are specified, all branches are matched successfully.
func newReferenceMatcher(branchMatchers [][]byte) (*regexp.Regexp, error) {
	sb := &strings.Builder{}
	sb.WriteString("^refs/tags/.+$|^refs/heads/(")

	for i, expression := range branchMatchers {
		segments := strings.Split(string(expression), "*")
		for i := range segments {
			segments[i] = regexp.QuoteMeta(segments[i])
		}

		sb.WriteString(strings.Join(segments, ".*"))

		if i < len(branchMatchers)-1 {
			sb.WriteString("|")
		}
	}

	if len(branchMatchers) == 0 {
		sb.WriteString(".+")
	}

	sb.WriteString(")$")

	return regexp.Compile(sb.String())
}

func validateUpdateRemoteMirrorRequest(ctx context.Context, locator storage.Locator, req *gitalypb.UpdateRemoteMirrorRequest) error {
	if err := locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return err
	}

	if req.GetRemote() == nil {
		return errors.New("missing Remote")
	}

	if req.GetRemote().GetUrl() == "" {
		return errors.New("remote is missing URL")
	}

	return nil
}

// publishedBranches returns the commit the mirror holds for every branch the
// publication is for: the commit that was pushed, or the commit the mirror
// keeps when the branch diverged and the request keeps divergent references.
func publishedBranches(localRefs []git.Reference, matcher *regexp.Regexp, kept map[git.ReferenceName]string) map[string]string {
	published := make(map[string]string, len(localRefs))
	for _, localRef := range localRefs {
		if !matcher.MatchString(localRef.Name.String()) {
			continue
		}
		branch, ok := strings.CutPrefix(localRef.Name.String(), "refs/heads/")
		if !ok {
			continue
		}
		if remote, diverged := kept[localRef.Name]; diverged {
			published[branch] = remote
			continue
		}
		published[branch] = localRef.Target
	}
	return published
}

// recordMirrorSync records the state of the mirror target a publication was
// for. A publication that names no target is a one-off copy, and nothing is
// recorded. Failing to record never fails a publication that succeeded: the
// commits reached the mirror, and the state is repaired by the next run.
func (s *server) recordMirrorSync(ctx context.Context, repo *gitalypb.Repository, target string, published map[string]string, diverged bool, publicationErr error) {
	if target == "" {
		return
	}

	store, key, err := hook.MirrorStore(ctx, s.core, s.localRepoFactory, repo)
	if err != nil {
		s.logger.WithError(err).WithField("target", target).WarnContext(ctx, "the publication of the mirror is not recorded")
		return
	}

	sync := mirrorstate.Sync{
		Target:      target,
		Status:      mirrorstate.SyncOK,
		Branches:    published,
		PublishedAt: time.Now().UTC(),
	}
	switch {
	case publicationErr != nil:
		sync.Status = mirrorstate.SyncFailed
		sync.Error = publicationErr.Error()
	case diverged:
		sync.Status = mirrorstate.SyncDiverged
	}

	if err := store.RecordSync(ctx, key, sync); err != nil {
		s.logger.WithError(err).WithField("target", target).WarnContext(ctx, "the publication of the mirror is not recorded")
	}
}
