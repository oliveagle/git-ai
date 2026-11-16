// Package mirror implements the MirrorService: the mirror targets of a
// repository, as a service.
//
// The reference-transaction hook refuses a push with the same targets and the
// same policy (internal/gitaly/hook), so the state an operator reads with
// graft mirror status and the refusal a pusher meets cannot disagree. See
// doc/graft/MIRROR.md.
package mirror

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedMirrorServiceServer
	deps *service.Dependencies
	// source reads the mirrors of a repository: the targets of the storage
	// core and how far they are behind. It is the source the hook decides
	// with.
	source hook.MirrorGateSource
}

// NewServer creates a new instance of the MirrorService server.
func NewServer(deps *service.Dependencies) gitalypb.MirrorServiceServer {
	return &server{
		deps:   deps,
		source: hook.NewMirrorGateSource(deps.GetGraftStorage(), deps.GetRepositoryFactory()),
	}
}

// RegisterTarget records a mirror target of a repository, or replaces the
// target that has that name.
func (s *server) RegisterTarget(ctx context.Context, req *gitalypb.RegisterMirrorTargetRequest) (*gitalypb.RegisterMirrorTargetResponse, error) {
	store, key, err := hook.MirrorStore(ctx, s.deps.GetGraftStorage(), s.deps.GetRepositoryFactory(), req.GetRepository())
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	now := time.Now().UTC()
	target := mirrorstate.Target{
		Name:      strings.TrimSpace(req.GetName()),
		URL:       strings.TrimSpace(req.GetUrl()),
		Branches:  append([]string(nil), req.GetBranches()...),
		Enabled:   req.GetEnabled(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.RegisterTarget(ctx, key, target); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	return &gitalypb.RegisterMirrorTargetResponse{
		Target: protoTarget(mirrorgate.Target{
			Name:     target.Name,
			URL:      target.URL,
			Branches: target.Branches,
			Enabled:  target.Enabled,
		}),
	}, nil
}

// UnregisterTarget removes a mirror target of a repository. The state of its
// last publication is kept.
func (s *server) UnregisterTarget(ctx context.Context, req *gitalypb.UnregisterMirrorTargetRequest) (*gitalypb.UnregisterMirrorTargetResponse, error) {
	store, key, err := hook.MirrorStore(ctx, s.deps.GetGraftStorage(), s.deps.GetRepositoryFactory(), req.GetRepository())
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, structerr.NewInvalidArgument("name is required")
	}

	if err := store.UnregisterTarget(ctx, key, name); err != nil {
		if errors.Is(err, mirrorstate.ErrTargetNotFound) {
			return nil, structerr.NewNotFound("the repository has no mirror target named %q", name)
		}
		return nil, structerr.NewInternal("%w", err)
	}

	return &gitalypb.UnregisterMirrorTargetResponse{}, nil
}

// ListTargets lists the mirror targets of a repository and the state of their
// last publication, with how far every mirror is behind the branch.
func (s *server) ListTargets(ctx context.Context, req *gitalypb.ListMirrorTargetsRequest) (*gitalypb.ListMirrorTargetsResponse, error) {
	branch, err := s.branch(ctx, req)
	if err != nil {
		return nil, err
	}

	state, err := s.source.MirrorState(ctx, req.GetRepository(), branch, "")
	if err != nil {
		return nil, structerr.NewInternal("reading the mirrors of the repository: %w", err)
	}

	policy := s.deps.GetCfg().Graft.Mirror.Policy()
	resp := &gitalypb.ListMirrorTargetsResponse{
		Targets: make([]*gitalypb.MirrorTarget, 0, len(state.Targets)),
		Policy: &gitalypb.MirrorPolicy{
			Required: policy.Required,
			WarnLag:  int32(policy.WarnLag),
			DenyLag:  int32(policy.DenyLag),
		},
	}
	for _, target := range state.Targets {
		resp.Targets = append(resp.Targets, protoTarget(target))
	}
	return resp, nil
}

// branch returns the branch the lag is measured for: the one the request
// names, or the default branch of the repository.
func (s *server) branch(ctx context.Context, req *gitalypb.ListMirrorTargetsRequest) (string, error) {
	branch := strings.TrimPrefix(strings.TrimSpace(req.GetBranch()), "refs/heads/")
	if branch != "" {
		return branch, nil
	}

	head, err := s.deps.GetRepositoryFactory().Build(req.GetRepository()).HeadReference(ctx)
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", nil
		}
		return "", structerr.NewInternal("reading the default branch of the repository: %w", err)
	}
	return strings.TrimPrefix(head.String(), "refs/heads/"), nil
}

// protoTarget returns the target as the API carries it.
func protoTarget(target mirrorgate.Target) *gitalypb.MirrorTarget {
	publishedAt := ""
	if !target.PublishedAt.IsZero() {
		publishedAt = target.PublishedAt.UTC().Format(time.RFC3339)
	}

	return &gitalypb.MirrorTarget{
		Name:        target.Name,
		Url:         target.URL,
		Branches:    target.Branches,
		Enabled:     target.Enabled,
		Status:      target.Status,
		Error:       target.Error,
		PublishedAt: publishedAt,
		Published:   target.Published,
		Lag:         int32(target.Lag),
		Diverged:    target.Diverged,
	}
}
