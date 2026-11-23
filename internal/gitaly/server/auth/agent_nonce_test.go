package auth

import (
	"strconv"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestNonceCache(t *testing.T) {
	t.Parallel()

	t.Run("a nonce is remembered once", func(t *testing.T) {
		t.Parallel()

		cache := newNonceCache()
		expiry := time.Now().Add(signatureWindow)

		require.NoError(t, cache.add("nonce-1", expiry))
		require.ErrorIs(t, cache.add("nonce-1", expiry), errNonceUsed)
	})

	t.Run("an expired nonce makes room", func(t *testing.T) {
		t.Parallel()

		cache := newNonceCache()
		past := time.Now().Add(-time.Second)
		future := time.Now().Add(signatureWindow)

		for i := 0; i < maxNonces; i++ {
			require.NoError(t, cache.add("stale-"+strconv.Itoa(i), past))
		}
		require.NoError(t, cache.add("fresh", future))
		// Only the fresh nonce remains: the stale ones are purged, not kept
		// at the old size.
		cache.mutex.Lock()
		require.Len(t, cache.nonces, 1)
		cache.mutex.Unlock()
	})

	t.Run("a full cache refuses instead of forgetting an unexpired nonce", func(t *testing.T) {
		t.Parallel()

		cache := newNonceCache()
		future := time.Now().Add(signatureWindow)

		require.NoError(t, cache.add("nonce-first", future))
		for i := 1; i < maxNonces; i++ {
			require.NoError(t, cache.add("nonce-"+strconv.Itoa(i), future))
		}

		require.ErrorIs(t, cache.add("nonce-overflow", future), errNonceCacheFull)
		// The flood must not have made the server forget an unexpired nonce:
		// its request is still refused as a replay.
		require.ErrorIs(t, cache.add("nonce-first", future), errNonceUsed)
	})
}

// TestInterceptors_NonceFloodCannotReopenReplayWindow checks that an attacker
// who knows the public fingerprint of an agent — public information — but not
// its private key cannot fill the nonce cache to make the server forget the
// nonce of a captured request: with the private key the attacker signs fresh
// requests anyway, so the cache is only reachable through a valid signature.
func TestInterceptors_NonceFloodCannotReopenReplayWindow(t *testing.T) {
	t.Parallel()

	agents, a, _ := testAgents(t)
	unary, _, err := Interceptors(auth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	// A legitimate request the attacker captured and wants to replay.
	ctx := contextWithSignature(t, a.sign(t, testMethod, ""), "")
	_, found, err := callUnary(t, ctx, unary, nil)
	require.NoError(t, err)
	require.True(t, found)

	// The attacker floods the cache: a fresh nonce and a broken signature
	// per request, the real fingerprint as the key metadata.
	for i := 0; i < maxNonces; i++ {
		md := metadata.Pairs(
			agentkey.KeyMetadata, a.fingerprint,
			agentkey.TimestampMetadata, strconv.FormatInt(time.Now().Unix(), 10),
			agentkey.NonceMetadata, "flood-"+strconv.Itoa(i),
			agentkey.SignatureMetadata, "bm90LWEtc2lnbmF0dXJl",
		)
		_, _, err := callUnary(t, contextWithMetadata(t, md), unary, nil)
		require.Equal(t, codes.Unauthenticated, status.Code(err), "flood request %d", i)
	}

	// The captured request must still be refused as a replay: the flood may
	// not have made the server forget its nonce.
	_, found, err = callUnary(t, ctx, unary, nil)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.False(t, found)
}
