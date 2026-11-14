package hook

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dbreview"
	"github.com/feder-cr/git-ai/internal/graft/greenref"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// branchGateSource gives the branch management policy the state of a branch:
// the reviews of the repository and the verification checkpoint of the branch.
type branchGateSource struct {
	// core is the storage core, nil when it is off. In authoritative mode the
	// reviews live in its database.
	core *corestore.Core
	// factory builds the local repository the state is read from.
	factory localrepo.Factory
}

// NewBranchGateSource returns the source of the state of a branch that the
// branch management policy decides with. The core may be nil: without it the
// reviews are the files of the repository.
func NewBranchGateSource(core *corestore.Core, factory localrepo.Factory) BranchGateSource {
	return &branchGateSource{core: core, factory: factory}
}

// BranchState implements BranchGateSource.
func (s *branchGateSource) BranchState(ctx context.Context, repo *gitalypb.Repository, branch string) (BranchGateState, error) {
	reviews, err := s.reviews(ctx, repo, branch)
	if err != nil {
		return BranchGateState{}, err
	}

	checkpoint, err := s.checkpoint(ctx, repo, branch)
	if err != nil {
		return BranchGateState{}, err
	}

	certified, err := s.certifiedTree(ctx, repo, branch)
	if err != nil {
		return BranchGateState{}, err
	}

	// The tree of the commit the checkpoint names: the checkpoint certifies
	// the whole commit only when the tree it certifies is this one.
	var checkpointTree string
	if checkpoint != "" {
		local := s.factory.Build(repo)
		oid, err := local.ResolveRevision(ctx, git.Revision(checkpoint+"^{tree}"))
		if err != nil {
			return BranchGateState{}, fmt.Errorf("reading the tree of the checkpoint %s of the branch %q: %w", checkpoint, branch, err)
		}
		checkpointTree = oid.String()
	}

	return BranchGateState{
		Reviews:        reviews,
		Checkpoint:     checkpoint,
		CertifiedTree:  certified,
		CheckpointTree: checkpointTree,
	}, nil
}

// reviews returns the open reviews of the branch. In authoritative mode they
// live in the database of the storage core, otherwise in the files of the
// repository.
func (s *branchGateSource) reviews(ctx context.Context, repo *gitalypb.Repository, branch string) ([]*gitalypb.Review, error) {
	if s.core != nil && s.core.Authoritative() {
		tracked, err := s.core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
		if err != nil {
			return nil, fmt.Errorf("storage core: %w", err)
		}
		return dbreview.New(ctx, s.core.Meta, tracked.ID).List(branch, false, 0)
	}

	path, err := s.repoPath(ctx, repo)
	if err != nil {
		return nil, err
	}
	return store.Open(path).List(branch, false, 0)
}

// checkpoint returns the commit the verification checkpoint of the branch
// points at, empty when the branch has none.
func (s *branchGateSource) checkpoint(ctx context.Context, repo *gitalypb.Repository, branch string) (string, error) {
	local := s.factory.Build(repo)
	oid, err := local.ResolveReferenceExactly(ctx, git.ReferenceName(greenref.Name(branch)))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("reading the checkpoint of the branch %q: %w", branch, err)
	}
	return oid.String(), nil
}

// certifiedTree returns the tree the verification checkpoint of the branch
// certifies: the tree the suite ran on. It is empty when the publisher
// certified no tree, which is the case when the reference is absent.
func (s *branchGateSource) certifiedTree(ctx context.Context, repo *gitalypb.Repository, branch string) (string, error) {
	local := s.factory.Build(repo)
	oid, err := local.ResolveReferenceExactly(ctx, git.ReferenceName(greenref.TreeName(branch)))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("reading the tree the checkpoint of the branch %q certifies: %w", branch, err)
	}
	return oid.String(), nil
}

// repoPath returns the path of the git directory of the repository, where the
// reviews live as files.
func (s *branchGateSource) repoPath(ctx context.Context, repo *gitalypb.Repository) (string, error) {
	path, err := s.factory.Build(repo).Path(ctx)
	if err != nil {
		return "", fmt.Errorf("repository path: %w", err)
	}
	return path, nil
}
