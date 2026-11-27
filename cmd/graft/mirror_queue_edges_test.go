package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
)

// TestMirrorQueueRefusesAnUnknownSubcommand covers the dispatch of the queue:
// a bare command and a name it does not know both stop before the file.
func TestMirrorQueueRefusesAnUnknownSubcommand(t *testing.T) {
	code, _, stderr := runCLI(t, "mirror", "queue")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "mirror queue requires a subcommand")

	code, _, stderr = runCLI(t, "mirror", "queue", "bogus", "--queue-file", mirrorQueueFile(t))
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, `unknown mirror queue subcommand "bogus"`)
}

// TestMirrorQueueRefusesAnImpossibleSchedule covers the declaration checks
// that keep a target from carrying two triggers, or a fallback that fires
// sooner than the interval it is supposed to stretch.
func TestMirrorQueueRefusesAnImpossibleSchedule(t *testing.T) {
	queue := mirrorQueueFile(t)
	declare := []string{"mirror", "queue", "add", "gitee", "--repo", "shop.git",
		"--url", "https://gitee.com/x/y.git", "--branch", "main", "--queue-file", queue}

	code, _, stderr := runCLI(t, append(append([]string{}, declare...), "--manual", "--interval", "5m")...)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "either --interval or --manual")

	code, _, stderr = runCLI(t, append(append([]string{}, declare...), "--interval", "5m", "--idle-interval", "1m")...)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "the idle interval 1m0s is shorter than the interval 5m0s")

	code, _, stderr = runCLI(t, append(append([]string{}, declare...), "--interval", "5m", "--failure-interval", "1m")...)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "the failure interval 1m0s is shorter than the interval 5m0s")
}

// TestMirrorQueueShowPrintsTheCadence covers what a person reads of a target
// that a queue written by hand holds: the idle and failure fallbacks, and the
// runs that went quiet or failed.
func TestMirrorQueueShowPrintsTheCadence(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{
		{
			Name:            "gitee",
			Repository:      "shop.git",
			URL:             "https://gitee.com/x/y.git",
			Branches:        []string{"main"},
			Interval:        5 * time.Minute,
			IdleInterval:    time.Hour,
			FailureInterval: 2 * time.Hour,
			QuietRuns:       3,
			Failures:        2,
			Enabled:         true,
		},
		{
			Name:       "manual",
			Repository: "shop.git",
			URL:        "https://gitee.com/x/y.git",
			Branches:   []string{"main"},
			Enabled:    true,
		},
	}})

	code, stdout, stderr := runCLI(t, "mirror", "queue", "show", "gitee", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "every: 5m0s, idle 1h0m0s, failing 2h0m0s")
	require.Contains(t, stdout, "quiet runs: 3")
	require.Contains(t, stdout, "failed runs: 2")

	// A target without a schedule is manual, and the fallbacks are not shown
	// as if they existed.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "show", "manual", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "every: manual")

	// The schedule is a fallback only: a failure interval alone stretches a
	// target that never goes idle.
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{{
		Name:            "failing",
		Repository:      "shop.git",
		URL:             "https://gitee.com/x/y.git",
		Branches:        []string{"main"},
		Interval:        5 * time.Minute,
		FailureInterval: 2 * time.Hour,
		Enabled:         true,
	}}})
	code, stdout, stderr = runCLI(t, "mirror", "queue", "show", "failing", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "every: 5m0s, failing 2h0m0s")
}

// TestMirrorQueueIdleRefusals covers the command line of idle, and the target
// it cannot find.
func TestMirrorQueueIdleRefusals(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{{
		Name:       "gitee",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Interval:   5 * time.Minute,
		Enabled:    true,
	}}})

	code, _, stderr := runCLI(t, "mirror", "queue", "idle", "a", "b", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "at most the name of one target")

	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "requires --idle-interval DURATION, --failure-interval DURATION, or both")

	// A failure fallback shorter than the interval is refused, as the idle
	// fallback is.
	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "--failure-interval", "1m", "--queue-file", queue)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "the failure interval 1m0s is shorter than the interval 5m0s")

	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "--idle-interval", "1h", "absent", "--queue-file", queue)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, `no target named "absent"`)

	// Both fallbacks are set together, and the JSON output names them.
	code, stdout, stderr := runCLI(t, "mirror", "queue", "idle",
		"--idle-interval", "1h", "--failure-interval", "2h", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	var targets []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &targets))
	require.Len(t, targets, 1)
	require.Equal(t, "1h0m0s", targets[0].IdleInterval)
	require.Equal(t, "2h0m0s", targets[0].FailureInterval)

	// A queue that holds nothing says so instead of quietly doing nothing.
	empty := mirrorQueueFile(t)
	writeMirrorQueue(t, empty, &mirrorqueue.Queue{Version: 1})
	code, stdout, stderr = runCLI(t, "mirror", "queue", "idle", "--idle-interval", "1h", "--queue-file", empty)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the mirror queue holds no target")
}

// TestMirrorQueueTierRefusalsAndUnknownAge covers the command line of tier,
// and the target whose repository nobody can read: it keeps the tier it has.
func TestMirrorQueueTierRefusalsAndUnknownAge(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{{
		Name:       "gitee",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Interval:   5 * time.Minute,
		Enabled:    true,
	}}})

	code, _, stderr := runCLI(t, "mirror", "queue", "tier", "a", "b", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "at most the name of one target")

	code, _, stderr = runCLI(t, "mirror", "queue", "tier", "--active", "0s", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "each a positive duration")

	code, _, stderr = runCLI(t, "mirror", "queue", "tier",
		"--active-within", "24h", "--recent-within", "1h", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "longer than --active-within")

	// The target names no server and the run names none either: the age of its
	// repository is unknown, so the target keeps the tier and the fallback it
	// has. The JSON output is the empty list of changed targets.
	code, stdout, stderr := runCLI(t, "mirror", "queue", "tier", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.JSONEq(t, "[]", stdout)

	code, stdout, stderr = runCLI(t, "mirror", "queue", "tier", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "tiers 0 target(s): 1 kept")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "show", "gitee", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	var reloaded mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &reloaded))
	require.Equal(t, "5m0s", reloaded.Interval)
}

// TestMirrorQueueTierReportsTheAgeItRead covers the age as a report prints it:
// the next whole unit down, and "unknown age" for a repository nobody read.
func TestMirrorQueueTierReportsTheAge(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{-time.Second, "unknown age"},
		{30 * time.Second, "less than a minute"},
		{5*time.Minute + 30*time.Second, "5m"},
		{3*time.Hour + 30*time.Minute, "3h"},
		{50 * time.Hour, "2d"},
	} {
		require.Equal(t, tc.want, mirrorsweep.RepoAge(tc.age), tc.age.String())
	}

	// The tier of an age is the shortest one that still holds it.
	q := &mirrorQueueFlags{tierActive: time.Hour, tierRecent: 3 * time.Hour, tierDormant: 12 * time.Hour,
		tierActiveWithin: 24 * time.Hour, tierRecentWithin: 7 * 24 * time.Hour}
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{time.Minute, "active"},
		{48 * time.Hour, "recent"},
		{30 * 24 * time.Hour, "dormant"},
	} {
		_, tier := mirrorsweep.TierFor(tc.age, tierOptions(q))
		require.Equal(t, tc.want, tier, tc.age.String())
	}
}

// TestMirrorQueueRunRefusalsAndEmptyQueue covers the run that has nothing to
// publish, which is not an error, and names the queue it read.
func TestMirrorQueueRunRefusalsAndEmptyQueue(t *testing.T) {
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "run", "extra", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "mirror queue run takes no arguments")

	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1})
	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "holds no target")
	require.Contains(t, stdout, queue)

	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.JSONEq(t, "[]", stdout)

	// A target that a person publishes by hand is never due on its own.
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{{
		Name:       "manual",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Enabled:    true,
	}}})
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no mirror target is due in "+queue)
}

// TestMirrorTaskListDefaultsTheLimit covers the history of a queue that holds
// more tasks than a run is asked to print.
func TestMirrorTaskListDefaultsTheLimit(t *testing.T) {
	queue := mirrorQueueFile(t)
	finished := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tasks := make([]*mirrorqueue.Task, 0, 3)
	for i, id := range []string{"task-1", "task-2", "task-3"} {
		tasks = append(tasks, &mirrorqueue.Task{
			ID:         id,
			Target:     "gitee",
			Repository: "shop.git",
			Status:     mirrorqueue.TaskOK,
			FinishedAt: finished.Add(time.Duration(i) * time.Minute),
		})
	}
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Tasks: tasks})

	// The newest task is printed first, and a limit of zero reads as the
	// default instead of printing nothing.
	code, stdout, stderr := runCLI(t, "mirror", "task", "list", "--limit", "0", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "task-3")
	require.Contains(t, stdout, "task-1")

	code, stdout, stderr = runCLI(t, "mirror", "task", "list", "--limit", "1", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "task-3")
	require.NotContains(t, stdout, "task-1")
}
