package sdk

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MirrorTargetOptions are the arguments of RegisterMirrorTarget.
type MirrorTargetOptions struct {
	// Name identifies the target in the repository. It is the name a
	// publication names, and the name a refusal reports.
	Name string
	// URL is the Git URL of the mirror.
	URL string
	// Branches are the branch patterns the mirror may publish, without a
	// refs/heads/ prefix. "*" is a wildcard.
	Branches []string
	// Enabled tells whether the target is published. A disabled target is
	// ignored by the mirror policy.
	Enabled bool
}

// MirrorTarget is one mirror of a repository, with the state of its last
// publication.
type MirrorTarget struct {
	// Name identifies the target in the repository.
	Name string
	// URL is the Git URL of the mirror.
	URL string
	// Branches are the branch patterns the mirror may publish.
	Branches []string
	// Enabled tells whether the target is published.
	Enabled bool
	// Status is the outcome of the last publication: "ok", "diverged" or
	// "failed". It is empty when the target never published.
	Status string
	// Error is the reason of the last failure.
	Error string
	// PublishedAt is when the mirror was published last, zero when it never
	// was.
	PublishedAt time.Time
	// Published is the commit the mirror holds for every branch it published.
	Published map[string]string
	// Lag is the number of commits of the branch the mirror does not hold.
	Lag int
	// Diverged tells whether the mirror holds the branch at a commit the
	// repository does not have.
	Diverged bool
}

// MirrorPolicy is the mirror policy of the server.
type MirrorPolicy struct {
	// Required refuses a push to a repository that has no mirror target, or
	// whose mirrors never published successfully.
	Required bool
	// WarnLag is the number of commits a mirror may be behind before a push
	// is warned about.
	WarnLag int
	// DenyLag is the number of commits a mirror may be behind before a push
	// is refused. Zero turns the refusal off.
	DenyLag int
}

// RegisterMirrorTarget records a mirror target of the repository, or replaces
// the target that has that name. The server then holds the pushes of the
// repository to the mirror policy: a repository whose mirror never published
// takes no push, and a mirror that falls behind warns, then refuses.
//
// The call needs the shared (operator) token: agents may not declare the
// mirrors that hold them to account.
func (r *Repo) RegisterMirrorTarget(ctx context.Context, opts MirrorTargetOptions) (*MirrorTarget, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "the mirror target needs a name")
	}
	if strings.TrimSpace(opts.URL) == "" {
		return nil, status.Error(codes.InvalidArgument, "the mirror target needs a URL")
	}
	if len(opts.Branches) == 0 {
		return nil, status.Error(codes.InvalidArgument, "the mirror target needs at least one branch pattern")
	}

	resp, err := gitalypb.NewMirrorServiceClient(r.client.conn).RegisterTarget(ctx, &gitalypb.RegisterMirrorTargetRequest{
		Repository: r.repository(),
		Name:       opts.Name,
		Url:        opts.URL,
		Branches:   opts.Branches,
		Enabled:    opts.Enabled,
	})
	if err != nil {
		return nil, err
	}

	return mirrorTargetOf(resp.GetTarget()), nil
}

// UnregisterMirrorTarget removes a mirror target of the repository. The state
// of its last publication stays, so that the history of the repository says
// what it published.
func (r *Repo) UnregisterMirrorTarget(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return status.Error(codes.InvalidArgument, "the mirror target needs a name")
	}

	_, err := gitalypb.NewMirrorServiceClient(r.client.conn).UnregisterTarget(ctx, &gitalypb.UnregisterMirrorTargetRequest{
		Repository: r.repository(),
		Name:       name,
	})
	return err
}

// ListMirrorTargets lists the mirror targets of the repository and the state
// of their last publication. branch is the branch the lag is measured for,
// without the refs/heads/ prefix; empty measures it for the default branch.
// It also returns the branch the server measured, which is the branch an empty
// request means.
func (r *Repo) ListMirrorTargets(ctx context.Context, branch string) ([]MirrorTarget, MirrorPolicy, string, error) {
	resp, err := gitalypb.NewMirrorServiceClient(r.client.conn).ListTargets(ctx, &gitalypb.ListMirrorTargetsRequest{
		Repository: r.repository(),
		Branch:     branch,
	})
	if err != nil {
		return nil, MirrorPolicy{}, "", err
	}

	targets := make([]MirrorTarget, 0, len(resp.GetTargets()))
	for _, target := range resp.GetTargets() {
		targets = append(targets, *mirrorTargetOf(target))
	}

	policy := MirrorPolicy{
		Required: resp.GetPolicy().GetRequired(),
		WarnLag:  int(resp.GetPolicy().GetWarnLag()),
		DenyLag:  int(resp.GetPolicy().GetDenyLag()),
	}
	return targets, policy, resp.GetBranch(), nil
}

// mirrorTargetOf returns the target as the SDK carries it.
func mirrorTargetOf(target *gitalypb.MirrorTarget) *MirrorTarget {
	out := &MirrorTarget{
		Name:      target.GetName(),
		URL:       target.GetUrl(),
		Branches:  target.GetBranches(),
		Enabled:   target.GetEnabled(),
		Status:    target.GetStatus(),
		Error:     target.GetError(),
		Published: target.GetPublished(),
		Lag:       int(target.GetLag()),
		Diverged:  target.GetDiverged(),
	}
	if publishedAt := target.GetPublishedAt(); publishedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, publishedAt); err == nil {
			out.PublishedAt = parsed
		}
	}
	return out
}
