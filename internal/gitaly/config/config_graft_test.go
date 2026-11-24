package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/helper/duration"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// graftTestCfg returns a configuration that passes ValidateV2 in every respect
// except for the GitLab settings, which each test configures itself.
func graftTestCfg(t *testing.T) Cfg {
	t.Helper()

	root := testhelper.TempDir(t)

	binDir := filepath.Join(root, "bin.d")
	require.NoError(t, os.Mkdir(binDir, 0o755))

	storagePath := filepath.Join(root, "storage.d")
	require.NoError(t, os.Mkdir(storagePath, 0o755))

	cfg := Cfg{
		ListenAddr: "localhost:0",
		BinDir:     binDir,
		Storages:   []Storage{{Name: "default", Path: storagePath}},
		DailyMaintenance: DailyJob{
			Hour:     12,
			Minute:   0,
			Duration: duration.Duration(10 * time.Minute),
		},
	}
	// A Graft server refuses anonymous callers, so a valid configuration needs a
	// credential: this file exercises the GitLab and policy checks.
	cfg.Auth.Token = "test-shared-token"
	require.NoError(t, cfg.Sanitize())

	return cfg
}

func TestGraft_ValidateGitlabSecret_Standalone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc       string
		standalone bool
		cfg        func(t *testing.T) Cfg
		expectErr  bool
	}{
		{
			desc:       "standalone without any GitLab secret is valid",
			standalone: true,
			cfg:        func(t *testing.T) Cfg { return Cfg{} },
		},
		{
			desc:       "standalone ignores a non-existent gitlab-shell directory",
			standalone: true,
			cfg: func(t *testing.T) Cfg {
				return Cfg{GitlabShell: GitlabShell{Dir: "/does/not/exist"}}
			},
		},
		{
			desc:       "standalone ignores a missing secret file",
			standalone: true,
			cfg: func(t *testing.T) Cfg {
				return Cfg{Gitlab: Gitlab{SecretFile: "/does/not/exist"}}
			},
		},
		{
			desc:       "non-standalone without a secret is still an error",
			standalone: false,
			cfg:        func(t *testing.T) Cfg { return Cfg{} },
			expectErr:  true,
		},
		{
			desc:       "non-standalone with a GitLab secret is valid",
			standalone: false,
			cfg: func(t *testing.T) Cfg {
				return Cfg{Gitlab: Gitlab{Secret: "secret"}}
			},
		},
		{
			desc:       "non-standalone with a non-existent gitlab-shell directory is still an error",
			standalone: false,
			cfg: func(t *testing.T) Cfg {
				return Cfg{GitlabShell: GitlabShell{Dir: "/does/not/exist"}}
			},
			expectErr: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := tc.cfg(t)
			cfg.Graft.Standalone = tc.standalone

			err := cfg.validateGitlabSecret()
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGraft_ValidateV2_GitlabSettings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc       string
		standalone bool
		gitlab     Gitlab
		expectErr  bool
	}{
		{
			desc:       "standalone without GitLab URL or secret is valid",
			standalone: true,
		},
		{
			desc:       "standalone ignores an invalid GitLab URL",
			standalone: true,
			gitlab:     Gitlab{URL: "http://[::1", Secret: "secret"},
		},
		{
			desc:       "standalone ignores a GitLab URL that is not configured",
			standalone: true,
			gitlab:     Gitlab{URL: "", SecretFile: "/does/not/exist"},
		},
		{
			desc:       "non-standalone without GitLab URL is invalid",
			standalone: false,
			gitlab:     Gitlab{Secret: "secret"},
			expectErr:  true,
		},
		{
			desc:       "non-standalone without GitLab secret is invalid",
			standalone: false,
			gitlab:     Gitlab{URL: "https://gitlab.example.com"},
			expectErr:  true,
		},
		{
			desc:       "non-standalone with GitLab URL and secret is valid",
			standalone: false,
			gitlab:     Gitlab{URL: "https://gitlab.example.com", Secret: "secret"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := graftTestCfg(t)
			cfg.Gitlab = tc.gitlab
			cfg.Graft.Standalone = tc.standalone

			err := cfg.ValidateV2()
			if tc.expectErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "gitlab")
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestGraft_Validate_StandaloneSkipsGitlabSecret(t *testing.T) {
	t.Parallel()

	cfg := graftTestCfg(t)
	cfg.Graft.Standalone = true

	// The deprecated Validate runs validateGitlabSecret, which must not fail in standalone mode.
	require.NoError(t, cfg.Validate())
}

func TestGraft_Validate_RequiresCredential(t *testing.T) {
	t.Parallel()

	// The storage core makes a server a Graft server too, so it needs a
	// credential even without standalone mode.
	require.True(t, Graft{Storage: GraftStorage{Mode: "shadow"}}.RequiresAuthentication())

	// So do a configured agent and the super admin's secret: a server with an
	// agent must not let anonymous callers write what the agent may write.
	agent := GraftAgent{Name: "planner", PublicKey: "ssh-ed25519 AAAA"}
	require.True(t, Graft{Agents: []GraftAgent{agent}}.RequiresAuthentication())
	require.True(t, Graft{AdminToken: "secret"}.RequiresAuthentication())
	require.True(t, Graft{AdminTokenFile: "/etc/graft/admin-token"}.RequiresAuthentication())

	for _, tc := range []struct {
		desc       string
		standalone bool
		agent      bool
		token      string
		expectErr  bool
	}{
		{desc: "standalone without a credential", standalone: true, expectErr: true},
		{desc: "standalone with the shared token", standalone: true, token: "secret"},
		{desc: "an agent without a credential", agent: true, expectErr: true},
		{desc: "an agent with the shared token", agent: true, token: "secret"},
		{desc: "an upstream Gitaly without a credential is unchanged"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := graftTestCfg(t)
			cfg.Graft.Standalone = tc.standalone
			if tc.agent {
				cfg.Graft.Agents = []GraftAgent{agent}
			}
			cfg.Auth.Token = tc.token
			cfg.Gitlab = Gitlab{URL: "https://gitlab.example.com", Secret: "secret"}

			for _, err := range []error{cfg.Validate(), cfg.ValidateV2()} {
				if tc.expectErr {
					require.ErrorContains(t, err, "auth.token must be set")
					continue
				}
				require.NoError(t, err)
			}
		})
	}
}

func TestGraft_ValidatePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		policy      []GraftPolicyRule
		expectedErr string
	}{
		{desc: "empty policy"},
		{
			desc: "valid globs",
			policy: []GraftPolicyRule{
				{Actor: "agent-*", Refs: []string{"refs/heads/main", "refs/heads/**", "refs/heads/release**"}},
				{Actor: "*", Allow: true},
				{Allow: true},
			},
		},
		{
			desc:        "invalid actor glob",
			policy:      []GraftPolicyRule{{Actor: "[", Allow: true}},
			expectedErr: `graft policy rule 1: actor "["`,
		},
		{
			desc:        "invalid ref glob",
			policy:      []GraftPolicyRule{{Allow: true}, {Refs: []string{"refs/heads/[/**"}}},
			expectedErr: `graft policy rule 2: ref "refs/heads/[/**"`,
		},
		{
			desc:        "empty ref entry",
			policy:      []GraftPolicyRule{{Refs: []string{""}}},
			expectedErr: "graft policy rule 1: empty refs entry",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := graftTestCfg(t)
			cfg.Graft.Standalone = true
			cfg.Graft.Policy = tc.policy

			for _, err := range []error{cfg.Graft.Validate(), cfg.Validate(), cfg.ValidateV2()} {
				if tc.expectedErr == "" {
					require.NoError(t, err)
					continue
				}
				require.ErrorContains(t, err, tc.expectedErr)
			}
		})
	}
}

func TestGraft_ObjectServiceDefaults(t *testing.T) {
	t.Parallel()

	var zero GraftObjectService
	require.Equal(t, DefaultGraftMaxObjects, zero.MaxObjectsOrDefault())
	require.Equal(t, DefaultGraftChunkSize, zero.ChunkSizeOrDefault())

	custom := GraftObjectService{MaxObjects: 5, ChunkSize: 64}
	require.Equal(t, 5, custom.MaxObjectsOrDefault())
	require.Equal(t, 64, custom.ChunkSizeOrDefault())
}

func TestGraft_MaxOpenReviews(t *testing.T) {
	t.Parallel()

	require.Equal(t, DefaultGraftMaxOpenReviews, GraftReview{}.MaxOpenReviewsOrDefault())
	require.Equal(t, 7, GraftReview{MaxOpenReviews: 7}.MaxOpenReviewsOrDefault())

	cfg := graftTestCfg(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Review.MaxOpenReviews = -1
	for _, err := range []error{cfg.Graft.Validate(), cfg.Validate(), cfg.ValidateV2()} {
		require.ErrorContains(t, err, "max_open_reviews must not be negative")
	}
}

// TestGraft_ValidateMinClientVersion covers the version the server requires of
// its clients: a value it cannot compare would either refuse every client or
// none, so a value that is not a version is rejected at load time.
func TestGraft_ValidateMinClientVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"", "0.9.0", "v1.2.3", "0.10.0-rc.1", "0.9.1-2-g1a2b3c4"} {
		graft := Graft{MinClientVersion: version}
		require.NoError(t, graft.Validate(), version)
	}

	for _, version := range []string{"0.9", "latest", "0.9.x", "0.9.1-"} {
		graft := Graft{MinClientVersion: version}
		require.ErrorContains(t, graft.Validate(), "min_client_version", version)
	}
}
