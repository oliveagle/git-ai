package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// startWithMirrorPolicy starts the example server with the mirror policy on.
func startWithMirrorPolicy(t *testing.T, mirror config.GraftMirror) (gitalyStack, string) {
	t.Helper()

	ex := loadExample(t)
	ex.graft.Mirror = mirror
	return startGitaly(t, ex.graft, ex.sharedToken), ex.sharedToken
}

// operatorPush pushes refspec from the client repository at clientPath to the
// stack's repository over gitaly-ssh with the shared token, and returns the
// combined output of git push and whether it succeeded.
func operatorPush(t *testing.T, s gitalyStack, clientPath, refspec string) (string, bool) {
	t.Helper()

	payload, err := protojson.Marshal(&gitalypb.SSHReceivePackRequest{
		Repository: &gitalypb.Repository{
			StorageName:  s.storage,
			RelativePath: s.relPath,
			GlRepository: "graft:" + s.storage + "/" + s.relPath,
		},
		GlId:         "1",
		GlUsername:   "operator",
		GlRepository: "graft:" + s.storage + "/" + s.relPath,
	})
	require.NoError(t, err)

	var flags []string
	for flag, value := range featureflag.FromContext(s.ctx) {
		flags = append(flags, flag.FormatWithValue(value))
	}

	cmd := gittest.NewCommand(t, s.cfg, "-C", clientPath, "push", "-v", "git@localhost:graft/push.git", refspec)
	cmd.Env = append(cmd.Env,
		"GITALY_PAYLOAD="+string(payload),
		"GITALY_ADDRESS="+s.addr,
		"GITALY_TOKEN="+s.cfg.Auth.Token,
		"GITALY_FEATUREFLAGS="+strings.Join(flags, ","),
		fmt.Sprintf("GIT_SSH_COMMAND=%s receive-pack", s.cfg.BinaryPath("gitaly-ssh")),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// writeBranch points branch at a commit whose tree holds file, on top of
// parent.
func writeBranch(t *testing.T, s gitalyStack, branch, path, content string, parents ...string) string {
	t.Helper()

	parentIDs := make([]git.ObjectID, 0, len(parents))
	for _, parent := range parents {
		parentIDs = append(parentIDs, git.ObjectID(parent))
	}

	return gittest.WriteCommit(t, s.cfg, s.repoPath,
		gittest.WithBranch(branch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: path, Content: content}),
		gittest.WithParents(parentIDs...),
	).String()
}

// newMirrorRemote creates the bare repository a mirror publishes to.
func newMirrorRemote(t *testing.T, s gitalyStack) string {
	t.Helper()

	remote := filepath.Join(t.TempDir(), "mirror.git")
	gittest.Exec(t, s.cfg, "init", "--bare", "--initial-branch=main",
		"--object-format="+gittest.DefaultObjectHash.Format, remote)
	return remote
}

// mirrorRepo returns the repository through the SDK with the shared token:
// the token of the operator, who is the one that declares and publishes a
// mirror.
func mirrorRepo(t *testing.T, s gitalyStack, token string) *sdk.Repo {
	t.Helper()

	conn, err := sdk.Dial(s.ctx, s.addr, sdk.WithStorage(s.storage), sdk.WithToken(token))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn.Repo(s.relPath)
}

// TestMirrorPolicyHoldsOnPush walks the policy from "a repository that no
// mirror publishes" to "a mirror that is far enough behind to refuse", on the
// push path git really takes: git-receive-pack, the reference-transaction hook
// the server runs through gitaly-hooks, and the state the MirrorService holds.
func TestMirrorPolicyHoldsOnPush(t *testing.T) {
	t.Parallel()

	s, token := startWithMirrorPolicy(t, config.GraftMirror{Required: true, WarnLag: 10, DenyLag: 50})
	remote := newMirrorRemote(t, s)
	const branch = "topic/mirror"

	base := writeBranch(t, s, branch, "README.md", "mirror\n")

	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", "--branch", branch, s.repoPath, clientPath)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "first.txt"), []byte("first\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "first.txt")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=operator", "-c", "user.email=operator@example.com", "commit", "--quiet", "-m", "first")

	// A repository that declares no mirror target takes no push, and the
	// refusal tells the pusher the command that declares one.
	out, ok := operatorPush(t, s, clientPath, "HEAD:refs/heads/"+branch)
	require.False(t, ok, out)
	require.Contains(t, out, "no mirror target")
	require.Contains(t, out, "graft mirror queue add")
	require.Contains(t, out, "graft mirror status")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/"+branch))

	// A target the server knows counts; one that never published is not a
	// mirror yet.
	repo := mirrorRepo(t, s, token)
	_, err := repo.RegisterMirrorTarget(s.ctx, sdk.MirrorTargetOptions{
		Name:     "codeup",
		URL:      remote,
		Branches: []string{branch},
		Enabled:  true,
	})
	require.NoError(t, err)

	out, ok = operatorPush(t, s, clientPath, "HEAD:refs/heads/"+branch)
	require.False(t, ok, out)
	require.Contains(t, out, "ever published successfully")

	// Publishing the target records what the mirror holds, and the push goes
	// through.
	_, err = repo.Mirror(s.ctx, sdk.MirrorOptions{URL: remote, Branches: []string{branch}, Target: "codeup"})
	require.NoError(t, err)
	out, ok = operatorPush(t, s, clientPath, "HEAD:refs/heads/"+branch)
	require.True(t, ok, out)
	mirrored := s.git(t, "rev-parse", "refs/heads/"+branch)

	// A mirror ten commits behind warns the pusher, and the warning reaches it
	// where a refusal would: the stderr of the hook.
	tip := mirrored
	for i := range 12 {
		tip = writeBranch(t, s, branch, fmt.Sprintf("behind-%d.txt", i), "x\n", tip)
	}
	gittest.Exec(t, s.cfg, "-C", clientPath, "fetch", "--quiet", "origin")
	gittest.Exec(t, s.cfg, "-C", clientPath, "reset", "--quiet", "--hard", tip)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "second.txt"), []byte("second\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "second.txt")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=operator", "-c", "user.email=operator@example.com", "commit", "--quiet", "-m", "second")

	out, ok = operatorPush(t, s, clientPath, "HEAD:refs/heads/"+branch)
	require.True(t, ok, out)
	require.Contains(t, out, "commits behind")
	require.Contains(t, out, "graft mirror queue run")

	// The decision the command reports is the one the push took.
	targets, policy, err := repo.ListMirrorTargets(s.ctx, branch)
	require.NoError(t, err)
	require.True(t, policy.Required)
	require.Equal(t, 10, policy.WarnLag)
	require.Equal(t, 50, policy.DenyLag)
	require.Len(t, targets, 1)
	// The mirror holds the commit the publication took (the base), then the
	// successful push added one, the twelve commits added twelve and the push
	// of the client added the last one.
	require.Equal(t, 14, targets[0].Lag)
}

// TestMirrorPolicyRefusesALaggingMirror checks the second step of the policy:
// once deny_lag is set, a mirror that is far enough behind holds the push, and
// the repository keeps the history it had.
func TestMirrorPolicyRefusesALaggingMirror(t *testing.T) {
	t.Parallel()

	s, token := startWithMirrorPolicy(t, config.GraftMirror{Required: true, WarnLag: 1, DenyLag: 2})
	remote := newMirrorRemote(t, s)
	const branch = "topic/lagged"

	base := writeBranch(t, s, branch, "README.md", "lagged\n")
	repo := mirrorRepo(t, s, token)

	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", "--branch", branch, s.repoPath, clientPath)

	_, err := repo.RegisterMirrorTarget(s.ctx, sdk.MirrorTargetOptions{
		Name: "gitee", URL: remote, Branches: []string{branch}, Enabled: true,
	})
	require.NoError(t, err)
	_, err = repo.Mirror(s.ctx, sdk.MirrorOptions{URL: remote, Branches: []string{branch}, Target: "gitee"})
	require.NoError(t, err)

	// The mirror is two commits behind the branch, the refusal threshold.
	tip := base
	for i := range 2 {
		tip = writeBranch(t, s, branch, fmt.Sprintf("behind-%d.txt", i), "x\n", tip)
	}
	gittest.Exec(t, s.cfg, "-C", clientPath, "fetch", "--quiet", "origin")
	gittest.Exec(t, s.cfg, "-C", clientPath, "reset", "--quiet", "--hard", tip)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "pushed.txt"), []byte("pushed\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "pushed.txt")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=operator", "-c", "user.email=operator@example.com", "commit", "--quiet", "-m", "pushed")

	out, ok := operatorPush(t, s, clientPath, "HEAD:refs/heads/"+branch)
	require.False(t, ok, out)
	require.Contains(t, out, "hold a push back")
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/heads/"+branch))
}
