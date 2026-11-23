package verify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestObjectTypeMapsEveryKind covers the object types the backend reports: the
// ones the workspace names, and the one it does not, which keeps the name of
// the SDK.
func TestObjectTypeMapsEveryKind(t *testing.T) {
	require.Equal(t, workspace.ObjectBlob, objectType(sdk.ObjectBlob))
	require.Equal(t, workspace.ObjectTree, objectType(sdk.ObjectTree))
	require.Equal(t, workspace.ObjectCommit, objectType(sdk.ObjectCommit))
	require.Equal(t, workspace.ObjectType("5"), objectType(sdk.ObjectType(5)))
}

// TestClampUint32KeepsTheCountInRange covers the count a request carries: a
// negative count asks for the server default, and one above the range of the
// field saturates instead of wrapping.
func TestClampUint32KeepsTheCountInRange(t *testing.T) {
	require.Zero(t, clampUint32(-1))
	require.Zero(t, clampUint32(0))
	require.EqualValues(t, 7, clampUint32(7))
	require.EqualValues(t, ^uint32(0), clampUint32(int(^uint32(0))+1))
}

// TestZeroOIDLikeFollowsTheObjectFormat covers the "no object" value of a
// repository: it is as long as the object IDs around it.
func TestZeroOIDLikeFollowsTheObjectFormat(t *testing.T) {
	require.Equal(t, strings.Repeat("0", 40), zeroOIDLike(strings.Repeat("a", 40)))
	require.Equal(t, strings.Repeat("0", 64), zeroOIDLike(strings.Repeat("a", 64)))
	require.Empty(t, zeroOIDLike(""))
}

// TestWebhookNotifierRefusesWhatItCannotPost covers the notifier's refusals:
// no URL, a URL that is not one, and a host that answers with an error. Each
// one names what went wrong instead of dropping the outcome.
func TestWebhookNotifierRefusesWhatItCannotPost(t *testing.T) {
	out := RevertOutcome{Status: StatusNoCulprit, Branch: "main"}

	require.ErrorContains(t, (&WebhookNotifier{}).Notify(context.Background(), out), "no URL")
	require.ErrorContains(t, (&WebhookNotifier{URL: "http://[::1"}).Notify(context.Background(), out), "build webhook request")

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer failing.Close()
	require.ErrorContains(t, (&WebhookNotifier{URL: failing.URL}).Notify(context.Background(), out), "webhook answered")

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	require.NoError(t, (&WebhookNotifier{URL: ok.URL}).Notify(context.Background(), out))
}

// TestMapErrorKeepsTheSentinelTheWorkspaceReactsTo covers the translation of
// the SDK's errors: a missing object becomes workspace.ErrNotFound, any other
// failure is returned as it is so the caller still sees it.
func TestMapErrorKeepsTheSentinel(t *testing.T) {
	require.NoError(t, mapError(nil))
	require.True(t, errors.Is(mapError(status.Error(codes.NotFound, "no repository")), workspace.ErrNotFound))
	other := errors.New("the connection dropped")
	require.Equal(t, other, mapError(other))
}
