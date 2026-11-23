package main

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMirrorAuthorization covers the Authorization header of a mirror: the
// Basic user of the host convention, no header for an SSH identity, and the
// refusal of an HTTP token for an SSH URL.
func TestMirrorAuthorization(t *testing.T) {
	t.Setenv("GITEE_TOKEN", "secret")

	header, err := mirrorAuthorization(mirrorAuth{
		url: "https://gitee.com/x/y.git", tokenEnv: "GITEE_TOKEN", usage: "mirror add",
	})
	require.NoError(t, err)
	require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("oauth2:secret")), header)

	// The caller's user wins over the convention of the host.
	header, err = mirrorAuthorization(mirrorAuth{
		url: "https://github.com/x/y.git", tokenEnv: "GITEE_TOKEN", usage: "mirror add", username: "bot",
	})
	require.NoError(t, err)
	require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bot:secret")), header)

	// An SSH identity builds no header at all.
	header, err = mirrorAuthorization(mirrorAuth{
		url: "ssh://git@github.com/x/y.git", sshKey: "~/.ssh/id", tokenEnv: "GITEE_TOKEN", usage: "mirror add",
	})
	require.NoError(t, err)
	require.Empty(t, header)

	// An HTTP token for a URL that is not HTTP is a mistake of the command line.
	_, err = mirrorAuthorization(mirrorAuth{
		url: "ssh://git@gitee.com/x/y.git", tokenEnv: "GITEE_TOKEN", usage: "mirror add",
	})
	require.ErrorContains(t, err, "not an HTTP URL")
}
