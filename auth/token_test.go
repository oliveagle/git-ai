package gitalyauth

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/metadata"
	"github.com/stretchr/testify/require"
)

func TestTokenValidityFromEnv(t *testing.T) {
	testCases := []struct {
		desc string
		env  string
		want time.Duration
	}{
		{desc: "unset", env: "", want: defaultTokenValidityDuration},
		{desc: "wider window", env: "10m", want: 10 * time.Minute},
		{desc: "sub-second", env: "500ms", want: 500 * time.Millisecond},
		{desc: "invalid", env: "soon", want: defaultTokenValidityDuration},
		{desc: "zero", env: "0s", want: defaultTokenValidityDuration},
		{desc: "negative", env: "-1m", want: defaultTokenValidityDuration},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("GITALY_TOKEN_VALIDITY", tc.env)
			require.Equal(t, tc.want, tokenValidityFromEnv())
		})
	}
}

// TestCheckTokenToleratesClockSkew proves the purpose of the configurable
// window: a client whose clock lags the server's is denied with the default
// 30 seconds but accepted once the window is widened, as an operator widens
// it with GITALY_TOKEN_VALIDITY.
func TestCheckTokenToleratesClockSkew(t *testing.T) {
	const secret = "secret-token"

	// A token signed five minutes in the past, as a client with a slow clock
	// would send.
	token := signV2Token(t, secret, time.Now().Add(-5*time.Minute))

	ctx := testhelper.Context(t)
	md := metadata.MD{}
	md.Set("authorization", "Bearer "+token)

	require.Error(t, CheckToken(md.ToIncoming(ctx), secret, time.Now()),
		"the default window must reject a token five minutes old")

	defer func(d time.Duration) {
		tokenValidityDuration = d
	}(tokenValidityDuration)
	tokenValidityDuration = 10 * time.Minute

	require.NoError(t, CheckToken(md.ToIncoming(ctx), secret, time.Now()),
		"a widened window must accept the same token")
}

// signV2Token creates the v2 bearer token that a client signs for the given
// issued-at time.
func signV2Token(t *testing.T, secret string, issuedAt time.Time) string {
	t.Helper()

	message := strconv.FormatInt(issuedAt.Unix(), 10)
	signature := hmacSign([]byte(secret), message)

	return fmt.Sprintf("v2.%x.%s", signature, message)
}
