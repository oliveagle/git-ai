package sdk

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MirrorOptions are the arguments of Mirror: one Git remote and the branches
// that are allowed to reach it.
type MirrorOptions struct {
	// URL is the Git URL of the mirror, whatever host serves it: Gitee,
	// GitHub, GitLab, a self-hosted instance, or a local path in a test.
	URL string
	// Branches are the branch patterns to push, without refs/heads/. "*" is a
	// wildcard. An empty list mirrors every branch; the CLI asks for an
	// explicit list, because a backup should not publish more than the caller
	// meant to publish.
	Branches []string
	// HTTPAuthorizationHeader is the value of the Authorization header used
	// for HTTP and HTTPS remotes. It is never logged.
	HTTPAuthorizationHeader string
	// SSHKey is the path of the private key used for SSH remotes.
	SSHKey string
	// KnownHosts is the path of the host identities checked by SSH.
	KnownHosts string
	// KeepDivergentRefs leaves a remote branch alone when it is not an
	// ancestor of the local branch, and reports it. A backup leaves this set:
	// a mirror that has taken unrelated history must be repaired by a person,
	// not overwritten by a retry.
	KeepDivergentRefs bool
}

// MirrorResult says what the server learned while pushing to the remote.
type MirrorResult struct {
	// DivergentRefs are the remote branches that the server did not update
	// because they diverged and KeepDivergentRefs was set. Their names are
	// full reference names.
	DivergentRefs []string
}

// Mirror pushes the selected branches (and the tags, which carry the commits
// they name) from the server's repository to one Git remote. It is the
// provider-neutral half of a backup: the host is only a URL, so a mirror to
// any Git server is the same call. The push is idempotent: running it again
// after a network failure converges the same references. It is a
// reconciliation, not an event: the next run sees what the failed run did not
// finish.
//
// The call needs the shared (operator) token. It asks the server to make the
// Git connection, so the remote credentials never leave the server process;
// the SDK sends them only in the RPC to that server.
func (r *Repo) Mirror(ctx context.Context, opts MirrorOptions) (*MirrorResult, error) {
	if opts.URL == "" {
		return nil, status.Error(codes.InvalidArgument, "mirror URL is required")
	}
	for i, branch := range opts.Branches {
		if strings.TrimSpace(branch) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "branch pattern %d is empty", i+1)
		}
	}

	remote := &gitalypb.UpdateRemoteMirrorRequest_Remote{
		Url:                     opts.URL,
		HttpAuthorizationHeader: opts.HTTPAuthorizationHeader,
	}
	first := &gitalypb.UpdateRemoteMirrorRequest{
		Repository:        r.repository(),
		Remote:            remote,
		SshKey:            opts.SSHKey,
		KnownHosts:        opts.KnownHosts,
		KeepDivergentRefs: opts.KeepDivergentRefs,
	}

	stream, err := gitalypb.NewRemoteServiceClient(r.client.conn).UpdateRemoteMirror(ctx)
	if err != nil {
		return nil, err
	}
	// The first message carries the connection; the branch patterns follow,
	// one message per pattern.
	if err := stream.Send(first); err != nil {
		_ = stream.CloseSend()
		return nil, err
	}
	for _, branch := range opts.Branches {
		if err := stream.Send(&gitalypb.UpdateRemoteMirrorRequest{
			OnlyBranchesMatching: [][]byte{[]byte(branch)},
		}); err != nil {
			_ = stream.CloseSend()
			return nil, err
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}

	res := &MirrorResult{}
	for _, ref := range resp.GetDivergentRefs() {
		res.DivergentRefs = append(res.DivergentRefs, string(ref))
	}
	return res, nil
}
