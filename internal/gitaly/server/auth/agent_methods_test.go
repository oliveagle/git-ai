package auth

import (
	"context"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInterceptors_AgentMethodAuthorization checks that callers authenticated
// as an agent cannot call the administrative RPCs that bypass the reference
// policy, while the shared token can, and that the other RPCs stay open to
// agents.
func TestInterceptors_AgentMethodAuthorization(t *testing.T) {
	t.Parallel()

	agents, a, _ := testAgents(t)
	unary, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	for _, tc := range []struct {
		desc string
		// method is the RPC being called.
		method string
		// agent tells whether the request is signed with the key of the
		// agent; otherwise it carries the shared token.
		agent    bool
		authless bool
		wantCode codes.Code
	}{
		{desc: "agent may land", method: gitalypb.OperationService_UserLand_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may commit files", method: gitalypb.OperationService_UserCommitFiles_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may create branches", method: gitalypb.OperationService_UserCreateBranch_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may delete branches", method: gitalypb.OperationService_UserDeleteBranch_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may read objects", method: gitalypb.ObjectService_GetObjects_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may read trees", method: gitalypb.ObjectService_GetTree_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may find commits", method: gitalypb.CommitService_FindCommit_FullMethodName, agent: true, wantCode: codes.OK},
		{desc: "agent may not write references", method: gitalypb.RepositoryService_WriteRef_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not delete references with RefService", method: gitalypb.RefService_DeleteRefs_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not update references with RefService", method: gitalypb.RefService_UpdateReferences_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not fetch remotes", method: gitalypb.RepositoryService_FetchRemote_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories", method: gitalypb.RepositoryService_CreateRepository_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not fork repositories", method: gitalypb.RepositoryService_CreateFork_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from URLs", method: gitalypb.RepositoryService_CreateRepositoryFromURL_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from bundles", method: gitalypb.RepositoryService_CreateRepositoryFromBundle_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not remove repositories", method: gitalypb.RepositoryService_RemoveRepository_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not replicate repositories", method: gitalypb.RepositoryService_ReplicateRepository_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not restore repositories", method: gitalypb.RepositoryService_RestoreRepository_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not set custom hooks", method: gitalypb.RepositoryService_SetCustomHooks_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not restore custom hooks", method: gitalypb.RepositoryService_RestoreCustomHooks_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from snapshots", method: gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "shared token may create repositories from snapshots", method: gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName, wantCode: codes.OK},
		{desc: "agent may not call hook callbacks", method: gitalypb.HookService_ReferenceTransactionHook_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not call pre-receive", method: gitalypb.HookService_PreReceiveHook_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "agent may not manage object pools", method: gitalypb.ObjectPoolService_DeleteObjectPool_FullMethodName, agent: true, wantCode: codes.PermissionDenied},
		{desc: "shared token may call hook callbacks", method: gitalypb.HookService_ReferenceTransactionHook_FullMethodName, wantCode: codes.OK},
		{desc: "a request without a credential is unauthenticated", method: gitalypb.RepositoryService_RemoveRepository_FullMethodName, authless: true, wantCode: codes.Unauthenticated},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			unaryCtx, streamCtx := callContexts(t, a, tc.method, tc.agent, tc.authless)

			unaryCalled := false
			_, err := unary(unaryCtx, nil, &grpc.UnaryServerInfo{FullMethod: tc.method},
				func(context.Context, any) (any, error) {
					unaryCalled = true
					return nil, nil
				})
			require.Equal(t, tc.wantCode, status.Code(err), "unary error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, unaryCalled)

			streamCalled := false
			err = stream(nil, fakeServerStream{ctx: streamCtx}, &grpc.StreamServerInfo{FullMethod: tc.method},
				func(any, grpc.ServerStream) error {
					streamCalled = true
					return nil
				})
			require.Equal(t, tc.wantCode, status.Code(err), "stream error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, streamCalled)
		})
	}
}

// callContexts returns the contexts of a unary and a stream call of method.
// Each call carries a fresh signature, so that the second one is not a replay.
func callContexts(t *testing.T, key *agentKey, method string, agent, authless bool) (context.Context, context.Context) {
	t.Helper()

	switch {
	case agent:
		return signedAgentContext(t, key, method), signedAgentContext(t, key, method)
	case authless:
		return testhelper.Context(t), testhelper.Context(t)
	default:
		return contextWithAuthorization(t, v2Authorization(sharedSecretTest, time.Now())),
			contextWithAuthorization(t, v2Authorization(sharedSecretTest, time.Now()))
	}
}

// TestInterceptors_AgentMethodAuthorizationOnlyWithAgents checks that the
// method restrictions do not apply to a caller that is not an agent, on either
// server, and that an upstream Gitaly without a token keeps its behaviour.
func TestInterceptors_AgentMethodAuthorizationOnlyWithAgents(t *testing.T) {
	t.Parallel()

	agents, _, _ := testAgents(t)

	for _, tc := range []struct {
		desc     string
		agents   bool
		external bool
	}{
		{desc: "external server without agents", external: true},
		{desc: "internal server with agents", agents: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			var agentConfig []config.GraftAgent
			if tc.agents {
				agentConfig = agents
			}

			unary, _, err := Interceptors(gitalycfgauth.Config{}, Config{Agents: agentConfig}, tc.external)
			require.NoError(t, err)

			called := false
			_, err = unary(contextWithAuthorization(t, ""), nil,
				&grpc.UnaryServerInfo{FullMethod: gitalypb.HookService_ReferenceTransactionHook_FullMethodName},
				func(context.Context, any) (any, error) {
					called = true
					return nil, nil
				})
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}
