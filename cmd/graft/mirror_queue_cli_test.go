package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMirrorQueueUsageErrors refuses every command of the queue that cannot be
// run as written, before the queue or the server is touched.
func TestMirrorQueueUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "add without the name of a target",
			args: []string{"mirror", "queue", "add"},
			want: "takes the name of the target",
		},
		{
			desc: "update without the name of a target",
			args: []string{"mirror", "queue", "update"},
			want: "takes the name of the target",
		},
		{
			desc: "enable without the name of a target",
			args: []string{"mirror", "queue", "enable"},
			want: "takes the name of the target",
		},
		{
			desc: "disable without the name of a target",
			args: []string{"mirror", "queue", "disable"},
			want: "takes the name of the target",
		},
		{
			desc: "show without the name of a target",
			args: []string{"mirror", "queue", "show"},
			want: "takes the name of the target",
		},
		{
			desc: "show with two names",
			args: []string{"mirror", "queue", "show", "gitee", "github"},
			want: "takes the name of the target",
		},
		{
			desc: "list with an argument",
			args: []string{"mirror", "queue", "list", "extra"},
			want: "takes no arguments",
		},
		{
			desc: "run with an argument",
			args: []string{"mirror", "queue", "run", "extra"},
			want: "takes no arguments",
		},
		{
			desc: "worker with an argument",
			args: []string{"mirror", "queue", "worker", "extra"},
			want: "takes no arguments",
		},
		{
			desc: "register with two names",
			args: []string{"mirror", "queue", "register", "gitee", "github"},
			want: "takes at most the name of one target",
		},
		{
			desc: "add without a url",
			args: []string{"mirror", "queue", "add", "gitee", "--repo", "shop.git", "--branch", "main"},
			want: "requires --url URL",
		},
		{
			desc: "add with a schedule and the manual flag",
			args: []string{
				"mirror", "queue", "add", "gitee",
				"--repo", "shop.git", "--url", "https://gitee.com/group/shop.git", "--branch", "main",
				"--interval", "1h", "--manual",
			},
			want: "either --interval or --manual",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// TestMirrorQueueTargetsThatAreNotThere refuses a target the queue does not
// hold, and names the target it looked for.
func TestMirrorQueueTargetsThatAreNotThere(t *testing.T) {
	queue := mirrorQueueFile(t)

	for _, tc := range []struct {
		desc string
		args []string
	}{
		{"remove", []string{"mirror", "queue", "remove", "ghost", "--queue-file", queue}},
		{"enable", []string{"mirror", "queue", "enable", "ghost", "--queue-file", queue}},
		{"disable", []string{"mirror", "queue", "disable", "ghost", "--queue-file", queue}},
		{"show", []string{"mirror", "queue", "show", "ghost", "--queue-file", queue}},
		{"run of one target", []string{"mirror", "queue", "run", "--target", "ghost", "--queue-file", queue}},
		{"register of one target", []string{"mirror", "queue", "register", "ghost", "--queue-file", queue}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitError, code, stderr)
			require.Contains(t, stderr, "has no target named")
		})
	}
}

// TestMirrorQueueNamesItsFile checks the three places the file of the queue is
// named: the flag, then the environment, then the default of the user.
func TestMirrorQueueNamesItsFile(t *testing.T) {
	t.Run("the environment names the file", func(t *testing.T) {
		queue := filepath.Join(t.TempDir(), "from-env.json")
		t.Setenv("GRAFT_MIRROR_QUEUE", queue)

		code, stdout, stderr := runCLI(t, "mirror", "queue", "run")
		require.Equal(t, exitOK, code, stderr)
		require.Contains(t, stdout, queue)
	})

	t.Run("the default of the user names the file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
		t.Setenv("AppData", filepath.Join(home, "AppData"))

		code, stdout, stderr := runCLI(t, "mirror", "queue", "run")
		require.Equal(t, exitOK, code, stderr)
		require.Contains(t, stdout, "mirror-queue.json")
	})
}

// TestMirrorQueueJSONOutput checks that every command of the queue answers in
// JSON when it is asked to, so that a script reads the queue without parsing
// the table of a person.
func TestMirrorQueueJSONOutput(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)
	common := []string{"--queue-file", queue, "-address", env.addr, "-storage", env.storage}
	// The target publishes to a local remote, so that the run of the queue
	// publishes instead of failing on a host this machine cannot reach.
	remote := mirrorRemote(t, env)

	code, stdout, stderr := runCLI(t, append([]string{
		"mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main", "--json",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var added mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &added))
	require.Equal(t, "gitee", added.Name)
	require.Equal(t, remote, added.URL)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "show", "gitee", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var shown mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown))
	require.Equal(t, "gitee", shown.Name)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "list", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var listed []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &listed))
	require.Len(t, listed, 1)
	require.Equal(t, "gitee", listed[0].Name)

	code, stdout, stderr = runCLI(t, append([]string{
		"mirror", "queue", "update", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "2h", "--json",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var updated mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &updated))
	require.Equal(t, "gitee", updated.Name)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "disable", "gitee", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var disabled mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &disabled))
	require.False(t, disabled.Enabled)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "enable", "gitee", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var enabled mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &enabled))
	require.True(t, enabled.Enabled)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "register", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var registered []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &registered))
	require.Len(t, registered, 1)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "run", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var tasks []mirrorQueueTask
	require.NoError(t, json.Unmarshal([]byte(stdout), &tasks))
	require.NotEmpty(t, tasks)

	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "remove", "gitee", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var removed mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &removed))
	require.Equal(t, "gitee", removed.Name)
}

// TestMirrorQueueRegisterWithoutTargets says that there is nothing to
// register, instead of registering nothing silently.
func TestMirrorQueueRegisterWithoutTargets(t *testing.T) {
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "register", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no target to register")
}

// TestMirrorQueueKeepsTheQueueWhenTheServerRefuses checks that a target the
// server refuses leaves the queue as it was: the queue of a client and the
// state of a server are repaired separately.
func TestMirrorQueueKeepsTheQueueWhenTheServerRefuses(t *testing.T) {
	queue := mirrorQueueFile(t)

	// A server that is not there cannot count the target.
	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", "shop.git", "--url", "https://gitee.com/group/shop.git", "--branch", "main",
		"--queue-file", queue, "-address", "127.0.0.1:1", "-storage", "default")
	require.Equal(t, exitError, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the mirror queue is empty")
}

// TestMirrorQueueKeepsNoSecret checks that the queue file names the variable
// that carries the token and never the token itself.
func TestMirrorQueueKeepsNoSecret(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)
	t.Setenv("GITEE_TOKEN", "secret-token-value")

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", "https://gitee.com/group/shop.git", "--branch", "main",
		"--token-env", "GITEE_TOKEN",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	data, err := os.ReadFile(queue)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret-token-value")
	require.Contains(t, string(data), "GITEE_TOKEN")
}
