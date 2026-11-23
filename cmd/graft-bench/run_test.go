package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errWriter fails every write, as a closed pipe does.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

// nopWriter counts its writes and fails from the n-th one, so that a report
// that stops at its first, second, ... section can be told apart.
type nopWriter struct {
	writes   int
	failFrom int
}

func (w *nopWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failFrom {
		return 0, errors.New("the pipe is closed")
	}
	return len(p), nil
}

// TestRunReportsTheInvocationsItRefuses covers the exit codes and the messages
// of the entry point without a server: the flags are refused before the dial.
func TestRunReportsTheInvocationsItRefuses(t *testing.T) {
	t.Parallel()

	dead := "tcp://127.0.0.1:1"
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"help":                  {args: []string{"-h"}, code: 0},
		"unknown flag":          {args: []string{"--bogus"}, code: 2, want: "flag provided but not defined"},
		"create repo, no token": {args: []string{"--address", dead, "--repo", "r.git", "--create-repo"}, code: 2, want: "needs the shared --token"},
		"unknown format":        {args: []string{"--address", dead, "--repo", "r.git", "--format", "yaml"}, code: 2, want: "unknown format"},
		"both credentials":      {args: []string{"--address", dead, "--repo", "r.git", "--token", "t", "--agent-key", "k"}, code: 2, want: "mutually exclusive"},
		"invalid address":       {args: []string{"--address", "bogus", "--repo", "r.git"}, code: 1, want: "dial bogus"},
		"create repository":     {args: []string{"--address", dead, "--repo", "r.git", "--create-repo", "--token", "t"}, code: 1, want: "create repository r.git"},
		"nothing changes":       {args: []string{"--address", dead, "--repo", "r.git", "--agents", "1", "--changes", "1"}, code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			var stdout, stderr bytes.Buffer
			code := run(ctx, tc.args, &stdout, &stderr)
			require.Equal(t, tc.code, code, stderr.String())
			require.Contains(t, stderr.String(), tc.want)
		})
	}
}

// TestRunReportsAFailingStdout covers the output of a run whose result exists
// but cannot be written: the exit code says the output is lost.
func TestWriteMarkdownReportReportsAFailingWriter(t *testing.T) {
	t.Parallel()

	res := Result{
		Mode:           ModeLand,
		FailureClasses: map[string]int{"conflict": 2, "a|b": 1},
		Nodes:          []NodeResult{{Address: "a", Repos: 1}},
	}

	require.Error(t, writeMarkdownReport(errWriter{}, res))
	for failFrom := 1; failFrom <= 6; failFrom++ {
		w := &nopWriter{failFrom: failFrom}
		err := writeMarkdownReport(w, res)
		require.Error(t, err, "failFrom %d", failFrom)
	}
}

// TestRunCompareReportsTheFilesItCannotRead covers the compare entry point.
func TestRunCompareReportsTheFilesItCannotRead(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, runCompare([]string{"-h"}, &stdout, &stderr))
	require.Equal(t, 2, runCompare([]string{"--bogus"}, &stdout, &stderr))
	require.Equal(t, 2, runCompare(nil, &stdout, &stderr), "no result file")

	dir := t.TempDir()
	garbage := filepath.Join(dir, "result.json")
	require.NoError(t, os.WriteFile(garbage, []byte("not json"), 0o600))
	require.Equal(t, 1, runCompare([]string{garbage}, &stdout, &stderr))

	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte("{}"), 0o600))
	require.Equal(t, 2, runCompare([]string{empty}, errWriter{}, &stderr), "the comparison cannot be printed")
}

// TestBenchObjectFormatFollowsTheEnvironment covers the object format and the
// zero object ID of a run, which follow the SHA-256 setting of the test suite.
func TestBenchObjectFormatFollowsTheEnvironment(t *testing.T) {
	t.Setenv("GITALY_TEST_WITH_SHA256", "")
	require.Equal(t, 64, len(zeroOID()))

	t.Setenv("GITALY_TEST_WITH_SHA256", "")
	require.Equal(t, 64, len(zeroOID()))
	os.Unsetenv("GITALY_TEST_WITH_SHA256")
	require.Equal(t, 40, len(zeroOID()))
}

// TestCreateRepositoryReportsAnInvalidAddress covers the dial of the
// repository creation, which happens before the server is asked.
func TestCreateRepositoryReportsAnInvalidAddress(t *testing.T) {
	t.Parallel()

	err := createRepository(t.Context(), "bogus", "token", "default", "r.git")
	require.ErrorContains(t, err, "dial")
}

// TestConfigValidateRefusesAnImpossibleConflictRate covers the bounds of the
// configuration the CLI does not check itself.
func TestConfigValidateRefusesAnImpossibleConflictRate(t *testing.T) {
	t.Parallel()

	for _, rate := range []float64{-0.1, 1.5} {
		cfg := Config{Branch: "main", Agents: 1, ChangesPerAgent: 1, ConflictRate: rate}
		require.ErrorContains(t, cfg.Validate(), "conflict rate")
	}
}

// TestPercentileClampsTheRank covers the ranks at both ends of a distribution.
func TestPercentileClampsTheRank(t *testing.T) {
	t.Parallel()

	values := []float64{1, 2, 3}
	require.Equal(t, 1.0, percentile(values, 0))
	require.Equal(t, 3.0, percentile(values, 200))
	require.Equal(t, 2.0, percentile(values, 50))
	require.Equal(t, 7.0, percentile([]float64{7}, 99))
}
