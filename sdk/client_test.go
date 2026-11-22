package sdk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/sdk"
)

func TestDial_TokenIsSentAsAuthorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		opt        sdk.DialOption
		wantPrefix string
		wantExact  string
	}{
		{
			name:       "shared token uses HMAC v2 format",
			opt:        sdk.WithToken("shared-secret"),
			wantPrefix: "Bearer v2.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakes := newFakeServices()
			fakes.objects.objects = map[string][]byte{"oid": []byte("x")}

			client := dialFake(t, fakes.register, tc.opt)
			_, err := client.Repo("repo").GetObjects(t.Context(), []string{"oid"})
			require.NoError(t, err)

			require.NotEmpty(t, fakes.objects.authorization)
			if tc.wantExact != "" {
				require.Equal(t, tc.wantExact, fakes.objects.authorization)
			} else {
				require.True(t, strings.HasPrefix(fakes.objects.authorization, tc.wantPrefix),
					"unexpected authorization %q", fakes.objects.authorization)
			}
		})
	}
}

// TestDial_AgentKeySignsTheRequest checks that the requests of an agent carry
// the signature of its key, and that they carry no bearer token.
func TestDial_AgentKeySignsTheRequest(t *testing.T) {
	t.Parallel()

	privateKey, publicKey := testhelper.AgentKeyPair(t)
	keyPath := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(keyPath, privateKey, 0o600))

	fakes := newFakeServices()
	fakes.objects.objects = map[string][]byte{"oid": []byte("x")}

	client := dialFake(t, fakes.register, sdk.WithAgentKey(keyPath))
	_, err := client.Repo("repo").GetObjects(t.Context(), []string{"oid"})
	require.NoError(t, err)

	// The fingerprint the server looks the agent up with is the one of the
	// public key.
	parsed, err := agentkey.ParsePublicKey([]byte(publicKey))
	require.NoError(t, err)
	require.Equal(t, agentkey.Fingerprint(parsed), fakes.objects.signature)
	require.Empty(t, fakes.objects.authorization, "a signed request carries no bearer token")
}

// TestDial_AgentKeyMustBeReadable checks that an unreadable private key is
// reported when the client is dialed.
func TestDial_AgentKeyMustBeReadable(t *testing.T) {
	t.Parallel()

	_, err := sdk.Dial(t.Context(), "tcp://127.0.0.1:1",
		sdk.WithAgentKey(filepath.Join(t.TempDir(), "missing")))
	require.ErrorContains(t, err, "read the private key of the agent")
}

func TestDial_NoTokenSendsNoAuthorization(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.objects.objects = map[string][]byte{"oid": []byte("x")}

	client := dialFake(t, fakes.register)
	_, err := client.Repo("repo").GetObjects(t.Context(), []string{"oid"})
	require.NoError(t, err)
	require.Empty(t, fakes.objects.authorization)
}

func TestDial_TokenOptionsAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	privateKey, _ := testhelper.AgentKeyPair(t)
	keyPath := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(keyPath, privateKey, 0o600))

	_, err := sdk.Dial(t.Context(), "tcp://127.0.0.1:1",
		sdk.WithToken("shared"), sdk.WithAgentKey(keyPath))
	require.ErrorContains(t, err, "mutually exclusive")
}

func TestDial_InvalidAddress(t *testing.T) {
	t.Parallel()

	_, err := sdk.Dial(t.Context(), "not-a-scheme://nowhere")
	require.Error(t, err)
}

func TestClient_RepoUsesStorage(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.objects.objects = map[string][]byte{"oid": []byte("x")}

	client := dialFake(t, fakes.register, sdk.WithStorage("cache-1"))
	require.Equal(t, "cache-1", client.Storage())

	repo := client.Repo("@hashed/ab/cd.git")
	require.Equal(t, "@hashed/ab/cd.git", repo.RelativePath())

	_, err := repo.GetObjects(t.Context(), []string{"oid"})
	require.NoError(t, err)
	require.Len(t, fakes.objects.getObjectsRequests, 1)
	require.Equal(t, "cache-1", fakes.objects.getObjectsRequests[0].GetRepository().GetStorageName())
	require.Equal(t, "@hashed/ab/cd.git", fakes.objects.getObjectsRequests[0].GetRepository().GetRelativePath())
}

func TestClient_DefaultStorage(t *testing.T) {
	t.Parallel()

	client := dialFake(t, newFakeServices().register)
	require.Equal(t, sdk.DefaultStorage, client.Storage())
}

func TestDial_WithGRPCOptions(t *testing.T) {
	t.Parallel()

	var methods []string
	interceptor := grpc.WithUnaryInterceptor(func(
		ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
	) error {
		methods = append(methods, method)
		return invoker(ctx, method, req, reply, cc, opts...)
	})

	fakes := newFakeServices()
	fakes.commits.revisions = map[string]string{"main": "commit-1"}

	client := dialFake(t, fakes.register, sdk.WithGRPCOptions(interceptor))
	id, err := client.Repo("repo").ResolveRevision(t.Context(), "main")
	require.NoError(t, err)
	require.Equal(t, "commit-1", id)
	require.Equal(t, []string{"/gitaly.CommitService/FindCommit"}, methods)
}
