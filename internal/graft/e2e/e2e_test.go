// Package e2e holds Graft's end-to-end tests. They run the standalone server,
// the cache tiers and the SDK together in one process, the way an operator
// deploys them, but without spawning the graft or graft-cache binaries.
//
// The configuration of the standalone server comes from
// examples/graft/config.toml, so the example stays a working configuration.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/graft/cachetier"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// exampleConfigPath is the example configuration, relative to this package.
const exampleConfigPath = "../../../examples/graft/config.toml"

// exampleAgentNames are the agents of examples/graft/config.toml.
var exampleAgentNames = []string{"planner", "builder"}

// exampleKey is the SSH credential of an agent of the example configuration.
type exampleKey struct {
	// privateKeyPath is the file the test clients sign their requests with.
	privateKeyPath string
	// publicKey is the credential the server authorizes.
	publicKey string
}

// exampleKeys holds the credentials of the agents of the example
// configuration. setupExampleKeys generates the key pairs once per test
// binary: every stack that loads the example authorizes these public keys,
// and the clients sign with these private keys, as a deployed agent would.
var exampleKeys = map[string]exampleKey{}

// exampleKeysDir is the directory of the generated private keys.
var exampleKeysDir string

// setupExampleKeys generates the key pairs of the example configuration. It
// runs before the tests, from TestMain.
func setupExampleKeys() error {
	dir, err := os.MkdirTemp("", "graft-e2e-keys-")
	if err != nil {
		return err
	}
	exampleKeysDir = dir

	for _, name := range exampleAgentNames {
		privateKey, publicKey, err := testhelper.NewAgentKeyPair()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name+".key")
		if err := os.WriteFile(path, privateKey, 0o600); err != nil {
			return err
		}
		exampleKeys[name] = exampleKey{privateKeyPath: path, publicKey: publicKey}
	}

	return nil
}

// teardownExampleKeys removes the generated private keys.
func teardownExampleKeys(int) error {
	return os.RemoveAll(exampleKeysDir)
}

// gitalyStack is a standalone Gitaly server with one repository. The server
// answers access checks from the policy of its configuration, as the graft
// binary does in standalone mode.
type gitalyStack struct {
	ctx      context.Context
	cfg      config.Cfg
	addr     string
	storage  string
	relPath  string
	repoPath string
}

// sharedTestToken is the shared token of the test servers that configure
// agents. The test helpers use it for setup calls, which carry no agent
// identity.
const sharedTestToken = "shared-test-token"

// adminTestToken is the super admin's token of the test servers that have one:
// only the super admin may purge an archived repository (doc/graft/PLAN13.md).
const adminTestToken = "admin-test-token"

// exampleConfig is the part of examples/graft/config.toml that the tests run.
type exampleConfig struct {
	graft       config.Graft
	sharedToken string
}

// loadExample loads examples/graft/config.toml. The paths in the file are
// rewritten into a temporary directory, the whole file must validate, and the
// public keys of the agents are written there.
func loadExample(t *testing.T) exampleConfig {
	t.Helper()

	root := t.TempDir()
	dirs := map[string]string{
		"/var/lib/graft/run":   filepath.Join(root, "run"),
		"/var/lib/graft/repos": filepath.Join(root, "repos"),
		"/var/lib/graft/bin":   filepath.Join(root, "bin"),
		"/etc/graft/tokens":    filepath.Join(root, "tokens"),
	}
	for _, dir := range dirs {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	data, err := os.ReadFile(exampleConfigPath)
	require.NoError(t, err)
	text := string(data)
	for from, to := range dirs {
		text = strings.ReplaceAll(text, from, to)
	}

	for name, key := range exampleKeys {
		require.NoError(t, os.WriteFile(filepath.Join(dirs["/etc/graft/tokens"], name+".pub"), []byte(key.publicKey+"\n"), 0o600))
	}

	cfg, err := config.Load(strings.NewReader(text))
	require.NoError(t, err, "the example configuration must load")

	// Validate the whole file, not only the Graft section. The directories
	// it names exist, and the GitLab settings are not required in standalone
	// mode.
	cfg.Gitlab = config.Gitlab{}
	cfg.GitlabShell = config.GitlabShell{}
	require.NoError(t, cfg.Validate(), "the example configuration must validate")

	return exampleConfig{graft: cfg.Graft, sharedToken: cfg.Auth.Token}
}

// startExample starts the standalone server of the example configuration.
func startExample(t *testing.T) gitalyStack {
	t.Helper()

	ex := loadExample(t)
	return startGitaly(t, ex.graft, ex.sharedToken)
}

// unbatched returns graft with batched landing turned off. The phase-1 and
// phase-2 scenarios check per-request "land" changesets (operation, actor,
// count); with batching on, concurrent lands on main are recorded as one
// "land_batch" changeset whose members carry the actors. Batching itself is
// covered by phase3_test.go.
func unbatched(graft config.Graft) config.Graft {
	graft.Land.Batch.Disabled = true
	return graft
}

// startExampleUnbatched starts the example configuration with batching off,
// see unbatched.
func startExampleUnbatched(t *testing.T) gitalyStack {
	t.Helper()

	ex := loadExample(t)
	return startGitaly(t, unbatched(ex.graft), ex.sharedToken)
}

// startGitaly starts a standalone Gitaly with every service registered and
// creates an empty repository on it. The shared token authorizes the setup
// calls of the test helpers.
func startGitaly(t *testing.T, graft config.Graft, sharedToken string, opts ...testserver.GitalyServerOpt) gitalyStack {
	t.Helper()

	return startGitalyInMode(t, graft, sharedToken, true, opts...)
}

// startGitalyInMode is startGitaly with standalone mode on or off. Off, the agents of
// graft still authenticate, but the request defaults and the policy of standalone
// mode do not apply; the access checks of the pre-receive hook still go to the
// standalone client, which stands in for GitLab here.
func startGitalyInMode(t *testing.T, graft config.Graft, sharedToken string, standalone bool, opts ...testserver.GitalyServerOpt) gitalyStack {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedToken
	cfg.Graft = graft
	cfg.Graft.Standalone = standalone

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, append([]testserver.GitalyServerOpt{
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	}, opts...)...)
	cfg.SocketPath = addr

	// Only the setup call carries gitaly-servers metadata. The context of the
	// stack is used for agent calls too, and agents may not send it.
	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	return gitalyStack{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		storage:  cfg.Storages[0].Name,
		relPath:  repoProto.GetRelativePath(),
		repoPath: repoPath,
	}
}

// repoAs returns a handle to the repository whose requests carry token as a
// plain agent bearer token. An empty token sends no authorization.
func (s gitalyStack) repoAs(t *testing.T, token string) *sdk.Repo {
	t.Helper()

	opts := []sdk.DialOption{sdk.WithStorage(s.storage)}
	if token != "" {
		opts = append(opts, sdk.WithAgentKey(token))
	}
	conn, err := sdk.Dial(s.ctx, s.addr, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn.Repo(s.relPath)
}

// git runs a Git command in the repository and returns its output.
func (s gitalyStack) git(t *testing.T, args ...string) string {
	t.Helper()

	return strings.TrimSpace(string(gittest.Exec(t, s.cfg, append([]string{"-C", s.repoPath}, args...)...)))
}

// writeMain points main at a commit whose tree holds files.
func writeMain(t *testing.T, s gitalyStack, files map[string]string, parents ...string) string {
	t.Helper()

	entries := make([]gittest.TreeEntry, 0, len(files))
	for path, content := range files {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: path, Content: content})
	}

	parentIDs := make([]git.ObjectID, 0, len(parents))
	for _, parent := range parents {
		parentIDs = append(parentIDs, git.ObjectID(parent))
	}

	return gittest.WriteCommit(t, s.cfg, s.repoPath,
		gittest.WithBranch("main"),
		gittest.WithTreeEntries(entries...),
		gittest.WithParents(parentIDs...),
	).String()
}

// commitFile creates a commit on branch that adds path to startRev. The
// author and the acting user of the commit are who.
func commitFile(ctx context.Context, repo *sdk.Repo, who, branch, startRev, path, content string) (string, error) {
	res, err := repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:   branch,
		StartRev: startRev,
		Message:  "add " + path,
		Author:   sdk.Signature{Name: who, Email: who + "@example.com"},
		// CommitFiles does not fill in the acting user, so the ID is set.
		User:    sdk.User{ID: who, Name: who, Email: who + "@example.com"},
		Actions: []sdk.Action{sdk.Put{Path: path, Content: []byte(content)}},
	})
	if err != nil {
		return "", err
	}

	return res.CommitID, nil
}

// clientDisplayName is the name that landAs sends as the acting user. It is
// deliberately not an agent name, so a test that sees an agent name as the
// actor knows the server took it from the token and not from the request.
const clientDisplayName = "client-supplied display name"

// landAs lands commit on branch, creating the branch if it does not exist.
// The acting user has a display name and an address but no ID, so the server
// fills in the identity of the token. The actor of the land must therefore be
// who, never clientDisplayName.
func landAs(ctx context.Context, repo *sdk.Repo, who, branch, commit string) (*sdk.LandResult, error) {
	return repo.Land(ctx, sdk.LandOptions{
		Branch:       branch,
		Commit:       commit,
		User:         sdk.User{Name: clientDisplayName, Email: who + "@example.com"},
		CreateBranch: true,
	})
}

// allChangesets pages through the changeset log with a small page size.
func allChangesets(ctx context.Context, t *testing.T, repo *sdk.Repo) []*sdk.Changeset {
	t.Helper()

	var all []*sdk.Changeset
	var after uint64
	for {
		page, err := repo.Changesets(ctx, after, 7)
		require.NoError(t, err)
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].GetSequence()
	}
}

// requireDense checks that the changesets have sequences 1, 2, ... without gaps.
func requireDense(t *testing.T, changesets []*sdk.Changeset) {
	t.Helper()

	for i, c := range changesets {
		require.Equal(t, uint64(i+1), c.GetSequence(), "changeset %d", i)
	}
}

// landsOn returns the changesets with operation "land" on ref.
func landsOn(changesets []*sdk.Changeset, ref string) []*sdk.Changeset {
	var lands []*sdk.Changeset
	for _, c := range changesets {
		if c.GetOperation() == "land" && string(c.GetReference()) == ref {
			lands = append(lands, c)
		}
	}
	return lands
}

// startTier starts a cache tier that serves ObjectService from its cache and
// proxies every other RPC to upstream. It returns the tier's address and the
// tier itself, whose Stats report hits and misses.
func startTier(t *testing.T, upstream string, cfg cachetier.Config) (string, *cachetier.Server) {
	t.Helper()
	ctx := testhelper.Context(t)

	logger := testhelper.SharedLogger(t)
	conn, err := cachetier.DialUpstream(ctx, upstream, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	cfg.Logger = logger
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 64 << 20
	}
	server, err := cachetier.NewServer(conn, cfg)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := cachetier.NewGRPCServer(server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	return "tcp://" + listener.Addr().String(), server
}

// blobsOf returns the blob object IDs of a recursive tree listing.
func blobsOf(t *testing.T, repo *sdk.Repo, rev string) map[string]string {
	t.Helper()

	entries, err := repo.ListTree(testhelper.Context(t), rev, "", true)
	require.NoError(t, err)

	blobs := map[string]string{}
	for _, e := range entries {
		if e.Type == gitalypb.ObjectType_BLOB {
			blobs[e.OID] = e.Path
		}
	}
	return blobs
}

// workerName returns the agent name of worker i.
func workerName(i int) string {
	return fmt.Sprintf("worker-%02d", i)
}

// TestStandaloneSetup starts Gitaly with [graft] standalone = true, two agents
// and a policy, from the example configuration, and checks that the server
// starts without GitLab configured. Health checks need no credential, a signed
// agent request is accepted, and anonymous or unknown-key requests are
// rejected: a Graft server never serves an anonymous caller. Covers WP1, WP2
// and the agent identity requirement (doc/graft/PLAN14.md).
func TestStandaloneSetup(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	require.True(t, ex.graft.Standalone)
	require.Len(t, ex.graft.Agents, 2)
	require.Len(t, ex.graft.Policy, 3)

	s := startGitaly(t, ex.graft, ex.sharedToken)

	t.Run("health check answers without a token", func(t *testing.T) {
		conn, err := client.DialContext(s.ctx, s.addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		resp, err := healthpb.NewHealthClient(conn).Check(s.ctx, &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
	})

	writeMain(t, s, map[string]string{"README.md": "hello graft\n"})

	t.Run("signed agent request is accepted", func(t *testing.T) {
		_, err := s.repoAs(t, exampleKeys["planner"].privateKeyPath).ResolveRevision(s.ctx, "main")
		require.NoError(t, err)
	})

	t.Run("anonymous request is rejected", func(t *testing.T) {
		_, err := s.repoAs(t, "").ResolveRevision(s.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})

	t.Run("anonymous write is rejected", func(t *testing.T) {
		// The rule the user cares about: nobody changes main without an
		// authenticated identity, and an agent's identity is its SSH key.
		_, err := s.repoAs(t, "").CommitFiles(s.ctx, sdk.CommitOptions{
			Branch:  "main",
			Message: "anonymous write",
			Author:  sdk.Signature{Name: "anonymous", Email: "anonymous@example.com"},
			User:    sdk.User{ID: "anonymous", Name: "anonymous", Email: "anonymous@example.com"},
			Actions: []sdk.Action{sdk.Put{Path: "anonymous.txt", Content: []byte("x\n")}},
		})
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)

		tip, err := s.repoAs(t, exampleKeys["planner"].privateKeyPath).ResolveRevision(s.ctx, "main")
		require.NoError(t, err)
		require.NotEmpty(t, tip)
	})

	t.Run("unknown key is rejected", func(t *testing.T) {
		unknownKey, _ := testhelper.AgentKeyFiles(t)
		_, err := s.repoAs(t, unknownKey).ResolveRevision(s.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})

	t.Run("broken signature is rejected", func(t *testing.T) {
		// A signature that was made for another method does not verify. The
		// server refuses the request instead of falling back to a weaker
		// credential.
		conn := s.dialTamperedSignature(t, exampleKeys["planner"].privateKeyPath)
		_, err := gitalypb.NewRepositoryServiceClient(conn).RepositoryExists(s.ctx, &gitalypb.RepositoryExistsRequest{
			Repository: &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath},
		})
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})
}

// TestAgentIdentitiesAndDefaults checks that every agent key maps to its
// agent name, and that a land without a user ID is recorded for the agent
// whose token made the request. Covers WP2 (identities, request defaults) and
// WP5 (actors in the changeset log).
func TestAgentIdentitiesAndDefaults(t *testing.T) {
	t.Parallel()

	s := startExampleUnbatched(t)
	base := writeMain(t, s, map[string]string{"README.md": "base\n"})

	planner := s.repoAs(t, exampleKeys["planner"].privateKeyPath)
	builder := s.repoAs(t, exampleKeys["builder"].privateKeyPath)

	topic, err := commitFile(s.ctx, planner, "planner", "planner/topic", base, "planner.txt", "from planner\n")
	require.NoError(t, err)

	res, err := landAs(s.ctx, planner, "planner", "main", topic)
	require.NoError(t, err)
	require.False(t, res.AlreadyLanded)
	require.Equal(t, topic, res.LandedCommitID, "a fast-forward lands the commit itself")

	feature, err := commitFile(s.ctx, builder, "builder", "builder/feature", base, "builder.txt", "from builder\n")
	require.NoError(t, err)
	_, err = landAs(s.ctx, builder, "builder", "feature/builder", feature)
	require.NoError(t, err, "the builder may create and land on feature branches")

	changesets := allChangesets(s.ctx, t, planner)
	requireDense(t, changesets)

	lands := landsOn(changesets, "refs/heads/main")
	require.Len(t, lands, 1)
	require.Equal(t, "planner", lands[0].GetActor())

	var builderLands []*sdk.Changeset
	for _, c := range changesets {
		if c.GetOperation() == "land" && c.GetActor() == "builder" {
			builderLands = append(builderLands, c)
		}
	}
	require.Len(t, builderLands, 1)
	require.Equal(t, "refs/heads/feature/builder", string(builderLands[0].GetReference()))

	for _, c := range changesets {
		if c.GetOperation() == "commit_files" {
			require.Contains(t, []string{"planner", "builder"}, c.GetActor())
		}
	}
}

// TestDaemonChainedToCache starts a graft-cache daemon tier whose upstream is
// a graft-cache cache tier whose upstream is Gitaly. A second daemon behind
// the same cache tier must be served from the cache tier, not from Gitaly.
// Covers WP7.
func TestDaemonChainedToCache(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	base := writeMain(t, s, map[string]string{"README.md": "chained\n", "docs-a.txt": "a\n"})
	reader := s.repoAs(t, exampleKeys["planner"].privateKeyPath)

	// The tiers hold the shared token: a signed request cannot be re-signed
	// for the upstream calls a tier makes on the caller's behalf, so a tier
	// that serves signed agents needs its own credential.
	cacheAddr, cacheTier := startTier(t, s.addr, cachetier.Config{
		Tier:    cachetier.TierCache,
		DiskDir: t.TempDir(),
		Token:   s.cfg.Auth.Token,
	})
	daemonAddr, daemon := startTier(t, cacheAddr, cachetier.Config{Tier: cachetier.TierDaemon, Token: s.cfg.Auth.Token})

	blobs := blobsOf(t, reader, base)
	oids := make([]string, 0, len(blobs))
	for oid := range blobs {
		oids = append(oids, oid)
	}

	direct, err := reader.GetObjects(s.ctx, oids)
	require.NoError(t, err)

	first, err := daemonHandle(t, s, daemonAddr, exampleKeys["planner"].privateKeyPath).GetObjects(s.ctx, oids)
	require.NoError(t, err)
	requireSameObjects(t, direct, first)
	require.NotZero(t, daemon.Stats().ObjectMisses)
	require.NotZero(t, cacheTier.Stats().MemoryEntries, "the cache tier must hold what the daemon fetched")

	// A fresh daemon has an empty memory cache. Its misses are served by the
	// cache tier, so the cache tier must report hits.
	freshAddr, fresh := startTier(t, cacheAddr, cachetier.Config{Tier: cachetier.TierDaemon, Token: s.cfg.Auth.Token})
	cacheHitsBefore := cacheTier.Stats().ObjectHits

	second, err := daemonHandle(t, s, freshAddr, exampleKeys["planner"].privateKeyPath).GetObjects(s.ctx, oids)
	require.NoError(t, err)
	requireSameObjects(t, direct, second)
	require.Greater(t, cacheTier.Stats().ObjectHits, cacheHitsBefore, "the cache tier must serve the fresh daemon")
	require.Zero(t, fresh.Stats().ObjectHits, "the fresh daemon starts empty")
}

// daemonHandle returns a repository handle through a cache tier address. An
// empty token sends no authorization.
func daemonHandle(t *testing.T, s gitalyStack, addr, token string) *sdk.Repo {
	t.Helper()

	opts := []sdk.DialOption{sdk.WithStorage(s.storage)}
	if token != "" {
		opts = append(opts, sdk.WithAgentKey(token))
	}
	conn, err := sdk.Dial(s.ctx, addr, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn.Repo(s.relPath)
}

// requireSameObjects checks that two reads returned identical objects.
func requireSameObjects(t *testing.T, want, got []sdk.Object) {
	t.Helper()

	require.Len(t, got, len(want))
	byOID := map[string]sdk.Object{}
	for _, o := range want {
		byOID[o.OID] = o
	}
	for _, o := range got {
		require.Equal(t, byOID[o.OID].Type, o.Type, o.OID)
		require.Equal(t, byOID[o.OID].Data, o.Data, o.OID)
		require.Equal(t, byOID[o.OID].Missing, o.Missing, o.OID)
	}
}

// TestTwentyConcurrentLands creates a repository, then lets 20 agents land
// disjoint changes created from the same base commit onto main concurrently
// using UserLand (rebase strategy). Every land must succeed, the final tree
// must contain all 20 files, and history must be linear. Covers WP3, WP5 and
// WP8 (SDK Land).
func TestTwentyConcurrentLands(t *testing.T) {
	t.Parallel()

	const workers = 20
	graft, tokens := workerGraft(t, workers)
	s := startGitaly(t, unbatched(graft), sharedTestToken)
	base := writeMain(t, s, map[string]string{"README.md": "concurrent\n"})

	// Each worker has its own connection, so its token is its identity.
	repos := make([]*sdk.Repo, workers)
	for i := range repos {
		repos[i] = s.repoAs(t, tokens[workerName(i)])
	}

	// Commits are created before any land, so that every change starts from
	// the same base and the lands race for main.
	commits := make([]string, workers)
	for i := range repos {
		commit, err := commitFile(s.ctx, repos[i], workerName(i), "work/"+workerName(i), base,
			fmt.Sprintf("workers/%s.txt", workerName(i)), fmt.Sprintf("written by %s\n", workerName(i)))
		require.NoError(t, err)
		commits[i] = commit
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*sdk.LandResult, workers)
	errs := make([]error, workers)
	for i := range repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = landAs(s.ctx, repos[i], workerName(i), "main", commits[i])
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i], "land of %s", workerName(i))
		require.NotEmpty(t, results[i].LandedCommitID)
		require.False(t, results[i].AlreadyLanded, "land of %s", workerName(i))
	}

	tree := s.git(t, "ls-tree", "-r", "--name-only", "main")
	for i := 0; i < workers; i++ {
		require.Contains(t, tree, fmt.Sprintf("workers/%s.txt", workerName(i)))
	}
	require.Contains(t, tree, "README.md")

	require.Empty(t, s.git(t, "rev-list", "--merges", "main"), "history must be linear")
	require.Equal(t, fmt.Sprint(workers+1), s.git(t, "rev-list", "--count", "main"))

	lands := landsOn(allChangesets(s.ctx, t, repos[0]), "refs/heads/main")
	require.Len(t, lands, workers)
	actors := make([]string, 0, len(lands))
	for _, c := range lands {
		actors = append(actors, c.GetActor())
	}
	expected := make([]string, 0, workers)
	for i := 0; i < workers; i++ {
		expected = append(expected, workerName(i))
	}
	sort.Strings(actors)
	require.Equal(t, expected, actors, "each worker must be the actor of exactly one land, taken from its token")
}

// workerGraft returns a standalone configuration with n worker agents. The
// workers may update main; nobody else may. Every other branch is open. The
// returned map holds the private key file of every worker.
func workerGraft(t *testing.T, n int) (config.Graft, map[string]string) {
	t.Helper()

	tokens := map[string]string{}
	var agents []config.GraftAgent
	for i := 0; i < n; i++ {
		name := workerName(i)
		privateKeyPath, publicKey := testhelper.AgentKeyFiles(t)
		tokens[name] = privateKeyPath
		agents = append(agents, config.GraftAgent{Name: name, PublicKey: publicKey})
	}

	return config.Graft{
		Standalone: true,
		Agents:     agents,
		Policy: []config.GraftPolicyRule{
			{Actor: "worker-*", Refs: []string{"refs/heads/main"}, Allow: true},
			{Refs: []string{"refs/heads/main"}, Allow: false},
			{Refs: []string{"refs/heads/**"}, Allow: true},
		},
	}, tokens
}

// TestReadThroughDaemonHasHits reads a tree and its objects through the daemon
// and asserts the cache reports hits on repeated reads. Covers WP4 and WP7.
func TestReadThroughDaemonHasHits(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	writeMain(t, s, map[string]string{
		"README.md": "read through the daemon\n",
		"main.go":   "package main\n",
		"lib.go":    "package lib\n",
	})

	daemonAddr, daemon := startTier(t, s.addr, cachetier.Config{Tier: cachetier.TierDaemon, Token: s.cfg.Auth.Token})
	repo := daemonHandle(t, s, daemonAddr, exampleKeys["planner"].privateKeyPath)

	blobs := blobsOf(t, repo, "main")
	require.Len(t, blobs, 3)
	oids := make([]string, 0, len(blobs))
	for oid := range blobs {
		oids = append(oids, oid)
	}

	first, err := repo.GetObjects(s.ctx, oids)
	require.NoError(t, err)
	hitsBefore := daemon.Stats().ObjectHits

	second, err := repo.GetObjects(s.ctx, oids)
	require.NoError(t, err)
	require.Greater(t, daemon.Stats().ObjectHits, hitsBefore, "the second read must hit the cache")
	requireSameObjects(t, first, second)
}

// TestBlameAndHistory checks that Blame and PathHistory through the SDK match
// `git blame --porcelain` and `git log --first-parent -- path` on the same
// repository. Covers WP6 and WP8.
func TestBlameAndHistory(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleKeys["planner"].privateKeyPath)

	// The README changes in the first, second and third commits. The fourth
	// commit does not touch it.
	c1 := writeCommitWith(t, s, "", map[string]string{"README.md": "one\ntwo\n"})
	c2 := writeCommitWith(t, s, c1, map[string]string{"README.md": "one\ntwo\nthree\n"})
	c3 := writeCommitWith(t, s, c2, map[string]string{"README.md": "ONE\ntwo\nthree\n"})
	writeCommitWith(t, s, c3, map[string]string{"README.md": "ONE\ntwo\nthree\n", "other.txt": "x\n"})

	t.Run("blame matches git", func(t *testing.T) {
		got, err := repo.Blame(s.ctx, "main", "README.md")
		require.NoError(t, err)

		want := gitBlameCommits(t, s, "main", "README.md")
		require.Len(t, got.Lines, len(want))
		for i, line := range got.Lines {
			require.Equal(t, want[i], line.CommitID, "line %d", i+1)
		}
		require.Equal(t, []string{c3, c1, c2}, commitsOf(got.Lines))
	})

	t.Run("path history matches git", func(t *testing.T) {
		got, err := repo.PathHistory(s.ctx, "main", "README.md", 100)
		require.NoError(t, err)

		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.GetId())
		}
		want := strings.Fields(s.git(t, "log", "--first-parent", "--format=%H", "main", "--", "README.md"))
		require.Equal(t, want, ids)
		require.Equal(t, []string{c3, c2, c1}, ids)
	})
}

// writeCommitWith writes a commit on top of parent (none if empty) that holds
// exactly files, and points main at it.
func writeCommitWith(t *testing.T, s gitalyStack, parent string, files map[string]string) string {
	t.Helper()

	var parents []string
	if parent != "" {
		parents = append(parents, parent)
	}
	return writeMain(t, s, files, parents...)
}

// gitBlameCommits returns the commit that last changed each line of path at
// rev, as reported by `git blame --porcelain`.
func gitBlameCommits(t *testing.T, s gitalyStack, rev, path string) []string {
	t.Helper()

	out := s.git(t, "blame", "--porcelain", rev, "--", path)
	var commits []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "\t") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || len(fields[0]) != 40 {
			continue
		}
		var final int
		_, err := fmt.Sscan(fields[2], &final)
		require.NoError(t, err)
		for len(commits) < final {
			commits = append(commits, "")
		}
		commits[final-1] = fields[0]
	}
	return commits
}

// commitsOf returns the commit of every line in order.
func commitsOf(lines []sdk.BlameLine) []string {
	commits := make([]string, 0, len(lines))
	for _, line := range lines {
		commits = append(commits, line.CommitID)
	}
	return commits
}

// TestChangesetLogHasAllLands checks that ListChangesets after a series of
// lands returns every land with its operation ("land"), actor and a dense
// sequence starting at 1. Covers WP5 and WP8 (Changesets).
func TestChangesetLogHasAllLands(t *testing.T) {
	t.Parallel()

	const lands = 9
	s := startExampleUnbatched(t)
	base := writeMain(t, s, map[string]string{"README.md": "log\n"})
	planner := s.repoAs(t, exampleKeys["planner"].privateKeyPath)

	tip := base
	for i := 0; i < lands; i++ {
		name := fmt.Sprintf("log/%02d.txt", i)
		commit, err := commitFile(s.ctx, planner, "planner", fmt.Sprintf("log/%02d", i), tip, name, "entry\n")
		require.NoError(t, err)
		res, err := landAs(s.ctx, planner, "planner", "main", commit)
		require.NoError(t, err)
		tip = res.LandedCommitID
	}

	changesets := allChangesets(s.ctx, t, planner)
	requireDense(t, changesets)

	mainLands := landsOn(changesets, "refs/heads/main")
	require.Len(t, mainLands, lands)
	for _, c := range mainLands {
		require.Equal(t, "planner", c.GetActor())
		require.NotEmpty(t, c.GetRecordedAt())
	}

	// A page size of seven must return the same log as one large page.
	page, err := planner.Changesets(s.ctx, 0, 1000)
	require.NoError(t, err)
	require.Equal(t, len(changesets), len(page))
}

// TestWatchReceivesLiveUpdate starts WatchChangesets from the current end of
// the log, performs one land, and expects exactly that changeset on the
// channel. Cancelling the context must end the watch without an error. Covers
// WP5 and WP8 (WatchChangesets).
func TestWatchReceivesLiveUpdate(t *testing.T) {
	t.Parallel()

	s := startExampleUnbatched(t)
	base := writeMain(t, s, map[string]string{"README.md": "watch\n"})
	planner := s.repoAs(t, exampleKeys["planner"].privateKeyPath)

	commit, err := commitFile(s.ctx, planner, "planner", "watch/topic", base, "watched.txt", "live\n")
	require.NoError(t, err)

	existing := allChangesets(s.ctx, t, planner)
	cursor := existing[len(existing)-1].GetSequence()

	watchCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	changes, errs := planner.WatchChangesets(watchCtx, cursor)

	_, err = landAs(s.ctx, planner, "planner", "main", commit)
	require.NoError(t, err)

	var got *sdk.Changeset
	select {
	case got = <-changes:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no live changeset arrived")
	}
	require.Equal(t, cursor+1, got.GetSequence())
	require.Equal(t, "land", got.GetOperation())
	require.Equal(t, "refs/heads/main", string(got.GetReference()))
	require.Equal(t, "planner", got.GetActor())

	select {
	case extra := <-changes:
		require.FailNow(t, "unexpected changeset", "got %v", extra)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	requireWatchEnded(t, changes, errs)
}

// requireWatchEnded checks that both watch channels close after cancellation
// and that no error is reported.
func requireWatchEnded(t *testing.T, changes <-chan *sdk.Changeset, errs <-chan error) {
	t.Helper()

	timeout := time.After(10 * time.Second)
	for changes != nil || errs != nil {
		select {
		case _, ok := <-changes:
			if ok {
				require.FailNow(t, "changeset received after cancellation")
			}
			changes = nil
		case err, ok := <-errs:
			if ok {
				require.NoError(t, err)
			}
			errs = nil
		case <-timeout:
			require.FailNow(t, "watch did not end after cancellation")
		}
	}
}

// TestPolicyDeniesProtectedBranch lets a non-privileged agent try to land on
// main, which the policy protects, and expects a PermissionDenied error with
// a detailed UserLandError (access_check) naming the acting user. The same
// agent must still be able to land on a feature branch. Covers WP1 and WP3.
func TestPolicyDeniesProtectedBranch(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	base := writeMain(t, s, map[string]string{"README.md": "policy\n"})
	planner := s.repoAs(t, exampleKeys["planner"].privateKeyPath)
	builder := s.repoAs(t, exampleKeys["builder"].privateKeyPath)

	// The planner may update main.
	plannerCommit, err := commitFile(s.ctx, planner, "planner", "planner/policy", base, "planner.txt", "ok\n")
	require.NoError(t, err)
	planned, err := landAs(s.ctx, planner, "planner", "main", plannerCommit)
	require.NoError(t, err)
	mainBefore := s.git(t, "rev-parse", "refs/heads/main")
	require.Equal(t, planned.LandedCommitID, mainBefore)

	builderCommit, err := commitFile(s.ctx, builder, "builder", "builder/policy", base, "builder.txt", "denied\n")
	require.NoError(t, err)

	_, err = landAs(s.ctx, builder, "builder", "main", builderCommit)
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)

	landErr, ok := sdk.LandError(err)
	require.True(t, ok, "the denial must carry a UserLandError: %v", err)
	require.NotNil(t, landErr.GetAccessCheck(), "the denial must be an access check")
	require.Equal(t, "builder", landErr.GetAccessCheck().GetUserId(), "the identity of the agent is the acting user")
	require.Contains(t, landErr.GetAccessCheck().GetErrorMessage(), "refs/heads/main")

	require.Equal(t, mainBefore, s.git(t, "rev-parse", "refs/heads/main"), "a denied land must leave main unchanged")

	// The builder may still land on a feature branch.
	res, err := landAs(s.ctx, builder, "builder", "feature/builder-policy", builderCommit)
	require.NoError(t, err)
	require.True(t, res.BranchCreated)

	for _, c := range landsOn(allChangesets(s.ctx, t, planner), "refs/heads/main") {
		require.NotEqual(t, "builder", c.GetActor(), "the denied land must not be recorded")
	}
}

// TestPolicyUsesTokenIdentity pins the behaviour that the policy and the
// changeset actor come from the identity of the agent. The builder's request
// names planner as the acting user, and the land on main must still be denied
// to builder: in standalone mode the pre-receive access check decides on the
// agent whose key signed the request, not on the client-sent gl_id.
func TestPolicyUsesTokenIdentity(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	base := writeMain(t, s, map[string]string{"README.md": "impersonation\n"})
	planner := s.repoAs(t, exampleKeys["planner"].privateKeyPath)
	builder := s.repoAs(t, exampleKeys["builder"].privateKeyPath)

	plannerCommit, err := commitFile(s.ctx, planner, "planner", "planner/impersonation", base, "planner.txt", "ok\n")
	require.NoError(t, err)
	_, err = landAs(s.ctx, planner, "planner", "main", plannerCommit)
	require.NoError(t, err)
	mainBefore := s.git(t, "rev-parse", "refs/heads/main")

	builderCommit, err := commitFile(s.ctx, builder, "builder", "builder/impersonation", base, "builder.txt", "denied\n")
	require.NoError(t, err)

	_, err = builder.Land(s.ctx, sdk.LandOptions{
		Branch: "main",
		Commit: builderCommit,
		User:   sdk.User{ID: "planner", Name: "planner", Email: "planner@example.com"},
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	require.Equal(t, mainBefore, s.git(t, "rev-parse", "refs/heads/main"), "a denied land must leave main unchanged")
}

// TestChangesetActorUsesTokenIdentity checks that the changeset log records the
// identity of the agent as the actor, even when the client sends another
// agent's name as its acting user. The builder's key signs commits and lands
// on a feature branch with User.ID set to planner. Every commit_files and land
// changeset must name builder as the actor. The feature branch is open to the
// builder, so the policy does not decide here; the policy side is pinned by
// TestPolicyUsesTokenIdentity. Covers WP5.
func TestChangesetActorUsesTokenIdentity(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	base := writeMain(t, s, map[string]string{"README.md": "actor\n"})
	builder := s.repoAs(t, exampleKeys["builder"].privateKeyPath)
	impersonated := sdk.User{ID: "planner", Name: "planner", Email: "planner@example.com"}

	commit, err := builder.CommitFiles(s.ctx, sdk.CommitOptions{
		Branch:   "builder/actor",
		StartRev: base,
		Message:  "add builder-actor.txt",
		Author:   sdk.Signature{Name: "builder", Email: "builder@example.com"},
		User:     impersonated,
		Actions:  []sdk.Action{sdk.Put{Path: "builder-actor.txt", Content: []byte("actor\n")}},
	})
	require.NoError(t, err)

	res, err := builder.Land(s.ctx, sdk.LandOptions{
		Branch:       "feature/builder-actor",
		Commit:       commit.CommitID,
		User:         impersonated,
		CreateBranch: true,
	})
	require.NoError(t, err)
	require.True(t, res.BranchCreated)

	changesets := allChangesets(s.ctx, t, builder)
	requireDense(t, changesets)

	var writes int
	for _, c := range changesets {
		if c.GetOperation() == "commit_files" || c.GetOperation() == "land" {
			writes++
			require.Equal(t, "builder", c.GetActor(), "operation %s on %s", c.GetOperation(), c.GetReference())
		}
	}
	require.Equal(t, 2, writes, "one commit and one land must be recorded")
	require.Len(t, landsOn(changesets, "refs/heads/feature/builder-actor"), 1)
}

// TestWritesThroughTiers runs writes through a chain of cache tiers: a daemon
// in front of a cache tier in front of Gitaly. A commit and a land by the
// planner must succeed and move main, and a land on main by the builder must
// be denied with the access-check detail. The changeset log must record only
// the planner's land. Covers WP7 (pass-through of writes) and WP10.
func TestWritesThroughTiers(t *testing.T) {
	t.Parallel()

	s := startExampleUnbatched(t)
	base := writeMain(t, s, map[string]string{"README.md": "writes\n"})

	cacheAddr, _ := startTier(t, s.addr, cachetier.Config{Tier: cachetier.TierCache, DiskDir: t.TempDir()})
	daemonAddr, _ := startTier(t, cacheAddr, cachetier.Config{Tier: cachetier.TierDaemon})
	planner := daemonHandle(t, s, daemonAddr, exampleKeys["planner"].privateKeyPath)
	builder := daemonHandle(t, s, daemonAddr, exampleKeys["builder"].privateKeyPath)

	plannerCommit, err := commitFile(s.ctx, planner, "planner", "planner/tiers", base, "tiers.txt", "through the tiers\n")
	require.NoError(t, err)
	landed, err := landAs(s.ctx, planner, "planner", "main", plannerCommit)
	require.NoError(t, err)
	require.Equal(t, landed.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
	require.Contains(t, s.git(t, "ls-tree", "--name-only", "main"), "tiers.txt")

	builderCommit, err := commitFile(s.ctx, builder, "builder", "builder/tiers", base, "builder-tiers.txt", "denied\n")
	require.NoError(t, err)
	mainBefore := s.git(t, "rev-parse", "refs/heads/main")

	_, err = landAs(s.ctx, builder, "builder", "main", builderCommit)
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	landErr, ok := sdk.LandError(err)
	require.True(t, ok, "the denial must carry a UserLandError: %v", err)
	require.Equal(t, "builder", landErr.GetAccessCheck().GetUserId())
	require.Equal(t, mainBefore, s.git(t, "rev-parse", "refs/heads/main"), "a denied land must leave main unchanged")

	res, err := landAs(s.ctx, builder, "builder", "feature/builder-tiers", builderCommit)
	require.NoError(t, err)
	require.True(t, res.BranchCreated)

	lands := landsOn(allChangesets(s.ctx, t, planner), "refs/heads/main")
	require.Len(t, lands, 1)
	require.Equal(t, "planner", lands[0].GetActor())
}

// TestTierTokenWidensAccess checks the trust boundary of a cache tier that was
// started with a token. A client that sends no authorization is served with
// the tier's token and reads an object that it could not read from Gitaly
// without credentials. A tier without a token refuses the same client. Covers
// WP7 and the trust boundary documented in doc/graft/ARCHITECTURE.md.
func TestTierTokenWidensAccess(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	s := startGitaly(t, ex.graft, ex.sharedToken)
	writeMain(t, s, map[string]string{"README.md": "widened\n"})
	blob := s.git(t, "rev-parse", "main:README.md")

	withToken, _ := startTier(t, s.addr, cachetier.Config{Tier: cachetier.TierDaemon, Token: ex.sharedToken})
	withoutToken, _ := startTier(t, s.addr, cachetier.Config{Tier: cachetier.TierDaemon})

	objects, err := daemonHandle(t, s, withToken, "").GetObjects(s.ctx, []string{blob})
	require.NoError(t, err, "the tier's token must serve a client that sends none")
	require.Len(t, objects, 1)
	require.False(t, objects[0].Missing)

	_, err = daemonHandle(t, s, withoutToken, "").GetObjects(s.ctx, []string{blob})
	require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
}

// dialTamperedSignature returns a raw gRPC connection whose requests carry a
// signature that was made for another method. The server must refuse them.
func (s gitalyStack) dialTamperedSignature(t *testing.T, privateKeyPath string) *grpc.ClientConn {
	t.Helper()

	raw, err := os.ReadFile(privateKeyPath)
	require.NoError(t, err)
	key, err := agentkey.ParsePrivateKey(raw)
	require.NoError(t, err)
	signer := agentkey.NewSigner(key)

	sign := func(ctx context.Context, method string, req any) context.Context {
		timestamp := time.Now().Unix()
		nonce, err := agentkey.Nonce()
		require.NoError(t, err)
		value, err := agentkey.Sign(key, agentkey.Payload(timestamp, nonce, "/gitaly.OtherService/Other", agentkey.BodyHash(req)))
		require.NoError(t, err)

		return metadata.AppendToOutgoingContext(ctx,
			agentkey.KeyMetadata, signer.Fingerprint(),
			agentkey.TimestampMetadata, strconv.FormatInt(timestamp, 10),
			agentkey.NonceMetadata, nonce,
			agentkey.SignatureMetadata, value,
		)
	}

	conn, err := client.DialContext(s.ctx, s.addr, client.WithGrpcOptions([]grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(sign(ctx, method, req), method, req, reply, cc, opts...)
		}),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// dialToken returns a raw gRPC connection whose requests carry the given
// bearer token, for RPCs that the SDK does not wrap.
func (s gitalyStack) dialToken(t *testing.T, token string) *grpc.ClientConn {
	t.Helper()

	conn, err := client.DialContext(s.ctx, s.addr, client.WithGrpcOptions([]grpc.DialOption{
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// dialAgent returns a raw gRPC connection whose requests are signed with the
// private key of an agent, for RPCs that the SDK does not wrap.
func (s gitalyStack) dialAgent(t *testing.T, privateKeyPath string) *grpc.ClientConn {
	t.Helper()

	signer, err := sdk.AgentSigner(privateKeyPath)
	require.NoError(t, err)

	conn, err := client.DialContext(s.ctx, s.addr, client.WithGrpcOptions([]grpc.DialOption{
		grpc.WithChainUnaryInterceptor(signer.UnaryInterceptor()),
		grpc.WithChainStreamInterceptor(signer.StreamInterceptor()),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}
