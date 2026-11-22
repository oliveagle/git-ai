package auth

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
)

// TestInterceptors_TransitioningWithAgentsFailsAtStartup checks that transitioning
// mode is refused when agents are configured. In transitioning mode a request
// that fails authentication has no identity, so a request with a wrong agent
// token would skip the allowlist. Without agents, transitioning mode is unchanged.
func TestInterceptors_TransitioningWithAgentsFailsAtStartup(t *testing.T) {
	t.Parallel()

	agents, _, _ := testAgents(t)
	for _, external := range []bool{true, false} {
		_, _, err := Interceptors(gitalycfgauth.Config{Token: "shared", Transitioning: true}, Config{Agents: agents}, external)
		require.ErrorContains(t, err, "auth.transitioning must be false")
	}

	_, _, err := Interceptors(gitalycfgauth.Config{Token: "shared", Transitioning: true}, Config{}, true)
	require.NoError(t, err)

	// A server that requires authentication refuses the mode as well: a
	// request without a credential would be let through.
	_, _, err = Interceptors(gitalycfgauth.Config{Token: "shared", Transitioning: true}, Config{RequireAuthentication: true}, true)
	require.ErrorContains(t, err, "auth.transitioning must be false")
}
