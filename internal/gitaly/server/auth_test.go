package server

import (
	netctx "context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/cache"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitmvcc "github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/server"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/limithandler"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/mvcc"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	expcredentials "google.golang.org/grpc/experimental/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestSanity(t *testing.T) {
	serverSocketPath := runServer(t, testcfg.Build(t))

	conn, err := dial(testhelper.Context(t), serverSocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, performUnaryRequest(t, conn))
}

func TestTLSSanity(t *testing.T) {
	cfg := testcfg.Build(t)
	addr := runSecureServer(t, cfg)

	certPool, err := x509.SystemCertPool()
	require.NoError(t, err)

	cert := testhelper.MustReadFile(t, "testdata/gitalycert.pem")
	ok := certPool.AppendCertsFromPEM(cert)
	require.True(t, ok)

	creds := expcredentials.NewTLSWithALPNDisabled(&tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	})

	conn, err := dial(testhelper.Context(t), fmt.Sprintf("tls://%s", addr), client.WithTransportCredentials(creds))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, performUnaryRequest(t, conn))
}

func TestAuthFailures(t *testing.T) {
	testCases := []struct {
		desc string
		opts []grpc.DialOption
		code codes.Code
	}{
		{desc: "no auth", opts: nil, code: codes.Unauthenticated},
		{
			desc: "invalid auth",
			opts: []grpc.DialOption{grpc.WithPerRPCCredentials(brokenAuth{})},
			code: codes.Unauthenticated,
		},
		{
			desc: "wrong secret",
			opts: []grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2("foobar"))},
			code: codes.PermissionDenied,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
				Auth: auth.Config{Token: "quxbaz"},
			}))

			serverSocketPath := runServer(t, cfg)
			conn, err := dial(testhelper.Context(t), serverSocketPath, client.WithGrpcOptions(tc.opts))
			require.NoError(t, err, tc.desc)
			t.Cleanup(func() { conn.Close() })
			testhelper.RequireGrpcCode(t, performUnaryRequest(t, conn), tc.code)
		})
	}
}

func TestAuthSuccess(t *testing.T) {
	token := "foobar"

	testCases := []struct {
		desc     string
		opts     []grpc.DialOption
		required bool
		token    string
	}{
		{desc: "no auth, not required"},
		{
			desc:  "v2 correct auth, not required",
			opts:  []grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token))},
			token: token,
		},
		{
			desc:  "v2 incorrect auth, not required",
			opts:  []grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2("incorrect"))},
			token: token,
		},
		{
			desc:     "v2 correct auth, required",
			opts:     []grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token))},
			token:    token,
			required: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
				Auth: auth.Config{Token: tc.token, Transitioning: !tc.required},
			}))

			serverSocketPath := runServer(t, cfg)
			conn, err := dial(testhelper.Context(t), serverSocketPath, client.WithGrpcOptions(tc.opts))
			require.NoError(t, err, tc.desc)
			t.Cleanup(func() { conn.Close() })
			assert.NoError(t, performUnaryRequest(t, conn), tc.desc)
		})
	}
}

func TestUnauthenticatedHealthService(t *testing.T) {
	ctx := testhelper.Context(t)

	cfg := testcfg.Build(t)
	cfg.Auth.Token = "secret-token"
	serverSocketPath := runServer(t, cfg)

	for _, tc := range []struct {
		desc                    string
		token                   string
		expectedAuthFailureCode codes.Code
	}{
		{
			desc:  "with correct token",
			token: cfg.Auth.Token,
		},
		{
			desc:                    "with wrong token",
			token:                   "incorrect-token",
			expectedAuthFailureCode: codes.PermissionDenied,
		},
		{
			desc:                    "without token",
			expectedAuthFailureCode: codes.Unauthenticated,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var dialOpts []grpc.DialOption
			if tc.token != "" {
				dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(tc.token)))
			}

			conn, err := dial(testhelper.Context(t), serverSocketPath, client.WithGrpcOptions(dialOpts))
			require.NoError(t, err)
			defer testhelper.MustClose(t, conn)

			if err := performUnaryRequest(t, conn); tc.expectedAuthFailureCode != 0 {
				testhelper.RequireGrpcCode(t, err, tc.expectedAuthFailureCode)
			} else {
				require.NoError(t, err)
			}

			resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			require.NoError(t, err)
			testhelper.ProtoEqual(t, resp, &healthpb.HealthCheckResponse{
				Status: healthpb.HealthCheckResponse_SERVING,
			})
		})
	}
}

type brokenAuth struct{}

func (brokenAuth) RequireTransportSecurity() bool { return false }
func (brokenAuth) GetRequestMetadata(netctx.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer blablabla"}, nil
}

func dial(ctx netctx.Context, serverSocketPath string, opts ...client.DialOption) (*grpc.ClientConn, error) {
	return client.New(ctx, serverSocketPath, opts...)
}

func performUnaryRequest(tb testing.TB, conn *grpc.ClientConn) error {
	ctx := testhelper.Context(tb)

	_, err := gitalypb.NewServerServiceClient(conn).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	return err
}

func newOperationClient(t *testing.T, token, serverSocketPath string) (gitalypb.OperationServiceClient, *grpc.ClientConn) {
	t.Helper()

	connOpts := []grpc.DialOption{
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
	}
	conn, err := client.New(testhelper.Context(t), serverSocketPath, client.WithGrpcOptions(connOpts))
	require.NoError(t, err)

	return gitalypb.NewOperationServiceClient(conn), conn
}

func runServer(t *testing.T, cfg config.Cfg) string {
	t.Helper()
	ctx := testhelper.Context(t)

	logger := testhelper.SharedLogger(t)
	registry := backchannel.NewRegistry()
	conns := client.NewPool()
	t.Cleanup(func() { testhelper.MustClose(t, conns) })
	locator := config.NewLocator(cfg)
	txManager := transaction.NewManager(cfg, logger, registry)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	hookManager := hook.NewManager(cfg, locator, logger, gitCmdFactory, nil, txManager, gitlab.NewMockClient(
		t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
	), hook.NewPostReceiveRegistry())
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	diskCache := cache.New(cfg, locator, logger)
	_, setupPerRPCConcurrencyLimiters := limithandler.WithConcurrencyLimiters(cfg)
	limitHandler := limithandler.New(cfg, limithandler.LimitConcurrencyByRepo, setupPerRPCConcurrencyLimiters)
	updaterWithHooks := updateref.NewUpdaterWithHooks(cfg, logger, locator, hookManager, gitCmdFactory, catfileCache)

	mvccMiddleware := mvcc.NewMvccInterceptor(gitmvcc.NewCacheFactory(cfg.RuntimeDir, locator), logger, protoregistry.GitalyProtoPreregistered, locator)
	srv, err := NewGitalyServerFactory(
		cfg,
		logger,
		registry,
		diskCache,
		[]*limithandler.LimiterMiddleware{limitHandler},
		nil,
		TransactionMiddleware{},
		mvccMiddleware,
	).New(true, false)
	require.NoError(t, err)
	localRepoFactory := localrepo.NewFactory(logger, locator, gitCmdFactory, catfileCache)

	setup.RegisterAll(srv, &service.Dependencies{
		Logger:                 logger,
		Cfg:                    cfg,
		GitalyHookManager:      hookManager,
		TransactionManager:     txManager,
		StorageLocator:         locator,
		ClientPool:             conns,
		GitCmdFactory:          gitCmdFactory,
		CatfileCache:           catfileCache,
		UpdaterWithHooks:       updaterWithHooks,
		LocalRepositoryFactory: localRepoFactory,
	})
	serverSocketPath := testhelper.GetTemporaryGitalySocketFileName(t)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "unix", serverSocketPath)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)
	go testhelper.MustServe(t, srv, listener)

	return "unix://" + serverSocketPath
}

//go:generate openssl req -newkey rsa:4096 -new -nodes -x509 -days 3650 -out testdata/gitalycert.pem -keyout testdata/gitalykey.pem -subj "/C=US/ST=California/L=San Francisco/O=GitLab/OU=GitLab-Shell/CN=localhost" -addext "subjectAltName = IP:127.0.0.1, DNS:localhost"
func runSecureServer(t *testing.T, cfg config.Cfg) string {
	t.Helper()

	cfg.TLS = config.TLS{
		CertPath: "testdata/gitalycert.pem",
		KeyPath:  "testdata/gitalykey.pem",
	}

	conns := client.NewPool()
	t.Cleanup(func() { testhelper.MustClose(t, conns) })

	_, setupPerRPCConcurrencyLimiters := limithandler.WithConcurrencyLimiters(cfg)

	locator := config.NewLocator(cfg)
	logger := testhelper.SharedLogger(t)
	mvccMiddleware := mvcc.NewMvccInterceptor(gitmvcc.NewCacheFactory(cfg.RuntimeDir, locator), logger, protoregistry.GitalyProtoPreregistered, locator)
	srv, err := NewGitalyServerFactory(
		cfg,
		logger,
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), logger),
		[]*limithandler.LimiterMiddleware{limithandler.New(cfg, limithandler.LimitConcurrencyByRepo, setupPerRPCConcurrencyLimiters)},
		nil,
		TransactionMiddleware{},
		mvccMiddleware,
	).New(true, true)
	require.NoError(t, err)

	gitalypb.RegisterServerServiceServer(srv, server.NewServer(&service.Dependencies{
		Logger:        logger,
		GitCmdFactory: gittest.NewCommandFactory(t, cfg),
		Cfg:           cfg,
	}))
	healthpb.RegisterHealthServer(srv, health.NewServer())

	listener, hostPort := testhelper.GetLocalhostListener(t)
	t.Cleanup(srv.Stop)
	go testhelper.MustServe(t, srv, listener)

	return hostPort
}

func TestUnaryNoAuth(t *testing.T) {
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Auth: auth.Config{Token: "testtoken"}}))

	path := runServer(t, cfg)
	conn, err := dial(testhelper.Context(t), path)
	require.NoError(t, err)
	defer testhelper.MustClose(t, conn)
	ctx := testhelper.Context(t)

	client := gitalypb.NewRepositoryServiceClient(conn)
	_, err = client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: "new/project/path",
		},
	},
	)

	testhelper.RequireGrpcCode(t, err, codes.Unauthenticated)
}

func TestStreamingNoAuth(t *testing.T) {
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Auth: auth.Config{Token: "testtoken"}}))

	path := runServer(t, cfg)
	conn, err := dial(testhelper.Context(t), path)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	ctx := testhelper.Context(t)

	client := gitalypb.NewRepositoryServiceClient(conn)

	response, err := client.GetFileAttributes(ctx, &gitalypb.GetFileAttributesRequest{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: "new/project/path",
		},
	},
	)
	require.Nil(t, response)
	testhelper.RequireGrpcCode(t, err, codes.Unauthenticated)
}

func TestAuthBeforeLimit(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: auth.Config{Token: "abc123"},
		Concurrency: []config.Concurrency{{
			RPC: "/gitaly.OperationService/UserCreateTag",
			ConcurrencyLimits: config.ConcurrencyLimits{
				MaxPerRepo: 1,
			},
		}},
	},
	))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath)

	gitlabURL, cleanup := gitlab.SetupAndStartGitlabServer(t, cfg.GitlabShell.Dir, &gitlab.TestServerOptions{
		SecretToken:                 "secretToken",
		GLID:                        gittest.GlID,
		GLRepository:                repo.GetGlRepository(),
		PostReceiveCounterDecreased: true,
		Protocol:                    "web",
	})
	t.Cleanup(cleanup)
	cfg.Gitlab.URL = gitlabURL

	serverSocketPath := runServer(t, cfg)
	client, conn := newOperationClient(t, cfg.Auth.Token, serverSocketPath)
	t.Cleanup(func() { conn.Close() })

	defer func(d time.Duration) {
		gitalyauth.SetTokenValidityDuration(d)
	}(gitalyauth.TokenValidityDuration())
	gitalyauth.SetTokenValidityDuration(5 * time.Second)

	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte(fmt.Sprintf(`#!/usr/bin/env bash
sleep %v
`, gitalyauth.TokenValidityDuration().Seconds())))

	errChan := make(chan error)

	for i := 0; i < 2; i++ {
		go func() {
			_, err := client.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
				Repository:     repo,
				TagName:        []byte(fmt.Sprintf("tag-name-%d", i)),
				TargetRevision: []byte(commitID),
				User:           gittest.TestUser,
				Message:        []byte("a new tag!"),
			})
			errChan <- err
		}()
	}

	timer := time.NewTimer(1 * time.Minute)

	for i := 0; i < 2; i++ {
		select {
		case <-timer.C:
			require.Fail(t, "time limit reached waiting for calls to finish")
		case err := <-errChan:
			require.NoError(t, err)
		}
	}
}

// tokenValidityHelperEnv makes the test binary act as a gitaly server whose
// process environment decides the token validity window. The parent test uses
// it to prove that the GITALY_TOKEN_VALIDITY environment variable widens the
// window for a whole server process, exactly as the deployment sets it on the
// gitaly service.
const (
	tokenValidityHelperEnv    = "GIT_AI_TOKEN_VALIDITY_HELPER"
	tokenValidityHelperOutEnv = "GIT_AI_TOKEN_VALIDITY_HELPER_OUTPUT"
)

// TestTokenValidityHelperProcess is not a real test: it is the program the
// parent test runs. It starts a gitaly server and performs a unary call with a
// v2 token signed five minutes in the past, as a client whose clock lags the
// server's would. Whether the call is accepted depends on the process's
// GITALY_TOKEN_VALIDITY, which the auth package read before this test ran. The
// outcome is written to the file named by the GIT_AI_TOKEN_VALIDITY_HELPER_OUTPUT
// environment variable.
func TestTokenValidityHelperProcess(t *testing.T) {
	outPath := os.Getenv(tokenValidityHelperOutEnv)
	if outPath == "" {
		t.Skip("only runs as a helper process")
	}

	const token = "secret-token"
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: auth.Config{Token: token},
	}))
	path := runServer(t, cfg)

	conn, err := dial(testhelper.Context(t), path, client.WithGrpcOptions([]grpc.DialOption{
		grpc.WithPerRPCCredentials(skewedClockCredentials{token: token, skew: -5 * time.Minute}),
	}))
	if err != nil {
		failTokenValidityHelper(t, outPath, "dial: %v", err)
	}

	err = performUnaryRequest(t, conn)
	switch {
	case err == nil:
		writeTokenValidityHelperResult(t, outPath, "accepted")
	case status.Code(err) == codes.PermissionDenied:
		writeTokenValidityHelperResult(t, outPath, "denied")
	default:
		failTokenValidityHelper(t, outPath, "unexpected code %v: %v", status.Code(err), err)
	}
}

// TestTokenValidityEnvWidensWindow proves end to end that a server process
// started with GITALY_TOKEN_VALIDITY accepts a client whose clock is minutes
// behind, while the default window rejects the same client.
func TestTokenValidityEnvWidensWindow(t *testing.T) {
	testCases := []struct {
		desc string
		env  []string
		want string
	}{
		{desc: "default window rejects a slow client", want: "denied"},
		{desc: "widened window accepts a slow client", env: []string{"GITALY_TOKEN_VALIDITY=10m"}, want: "accepted"},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.want, runTokenValidityHelper(t, tc.env...))
		})
	}
}

func runTokenValidityHelper(t *testing.T, extraEnv ...string) string {
	t.Helper()

	outPath := filepath.Join(t.TempDir(), "result")

	cmd := exec.Command(os.Args[0], "-test.run=^TestTokenValidityHelperProcess$")
	cmd.Env = withoutEnv(os.Environ(), "GITALY_TOKEN_VALIDITY")
	cmd.Env = append(cmd.Env,
		tokenValidityHelperEnv+"=1",
		tokenValidityHelperOutEnv+"="+outPath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	require.FileExists(t, outPath)

	result, err := os.ReadFile(outPath)
	require.NoError(t, err)

	return strings.TrimSpace(string(result))
}

// withoutEnv returns env without the variables named by drop.
func withoutEnv(env []string, drop ...string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		key := strings.SplitN(kv, "=", 2)[0]
		dropKey := false
		for _, d := range drop {
			if key == d {
				dropKey = true
				break
			}
		}
		if !dropKey {
			kept = append(kept, kv)
		}
	}
	return kept
}

func writeTokenValidityHelperResult(t *testing.T, path, result string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(result+"\n"), 0o600))
	os.Exit(0)
}

func failTokenValidityHelper(t *testing.T, path, format string, args ...any) {
	t.Helper()
	_ = os.WriteFile(path, []byte("error "+fmt.Sprintf(format, args...)+"\n"), 0o600)
	os.Exit(1)
}

// skewedClockCredentials sends a v2 token signed at now+skew, as a client
// whose clock differs from the server's by skew would.
type skewedClockCredentials struct {
	token string
	skew  time.Duration
}

func (skewedClockCredentials) RequireTransportSecurity() bool { return false }

func (c skewedClockCredentials) GetRequestMetadata(netctx.Context, ...string) (map[string]string, error) {
	message := strconv.FormatInt(time.Now().Add(c.skew).Unix(), 10)
	signature := hmacV2([]byte(c.token), message)

	return map[string]string{"authorization": "Bearer " + fmt.Sprintf("v2.%x.%s", signature, message)}, nil
}

func hmacV2(secret []byte, message string) []byte {
	mac := hmac.New(sha256.New, secret)
	// hash.Hash never returns an error.
	_, _ = mac.Write([]byte(message))

	return mac.Sum(nil)
}
