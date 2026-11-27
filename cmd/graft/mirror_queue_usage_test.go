package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
)

// TestMirrorQueueReadsACadence covers how the command prints the schedule of a
// target: a target with no interval runs only when a person asks for it, and
// the idle and failure fallbacks read next to the interval they stretch.
func TestMirrorQueueReadsACadence(t *testing.T) {
	t.Parallel()

	require.Equal(t, "manual", mirrorQueueInterval(0))
	require.Equal(t, "5m0s", mirrorQueueInterval(5*time.Minute))
	require.Equal(t, "", mirrorQueueIdleInterval(&mirrorqueue.Target{}))
	require.Equal(t, "", mirrorQueueFailureInterval(&mirrorqueue.Target{}))
	require.Equal(t, "manual", mirrorQueueSchedule(&mirrorqueue.Target{}))
	require.Equal(t, "5m0s", mirrorQueueSchedule(&mirrorqueue.Target{Interval: 5 * time.Minute}))
	require.Equal(t, "5m0s, idle 1h0m0s, failing 1h0m0s",
		mirrorQueueSchedule(&mirrorqueue.Target{Interval: 5 * time.Minute, IdleInterval: time.Hour}))
	require.Equal(t, "5m0s, idle 1h0m0s, failing 2h0m0s",
		mirrorQueueSchedule(&mirrorqueue.Target{Interval: 5 * time.Minute, IdleInterval: time.Hour, FailureInterval: 2 * time.Hour}))
	require.Equal(t, "5m0s, failing 2h0m0s",
		mirrorQueueSchedule(&mirrorqueue.Target{Interval: 5 * time.Minute, FailureInterval: 2 * time.Hour}))
}

// TestMirrorQueueReadsAnAge covers the age of the last change as a report
// prints it: the next whole unit down, and a repository whose age is not known.
func TestMirrorQueueReadsAnAge(t *testing.T) {
	t.Parallel()

	require.Equal(t, "unknown age", mirrorsweep.RepoAge(-time.Second))
	require.Equal(t, "less than a minute", mirrorsweep.RepoAge(30*time.Second))
	require.Equal(t, "5m", mirrorsweep.RepoAge(5*time.Minute))
	require.Equal(t, "3h", mirrorsweep.RepoAge(3*time.Hour))
	require.Equal(t, "2d", mirrorsweep.RepoAge(50*time.Hour))
}

// TestMirrorQueueTiersAnAge covers the tier of a repository: the interval and
// the name the report prints, by how long ago its last change was.
// tierOptions are the tiering choices of the command line, in the shape the
// shared worker takes them.
func tierOptions(q *mirrorQueueFlags) mirrorsweep.TierOptions {
	return mirrorsweep.TierOptions{
		Active:       q.tierActive,
		Recent:       q.tierRecent,
		Dormant:      q.tierDormant,
		ActiveWithin: q.tierActiveWithin,
		RecentWithin: q.tierRecentWithin,
	}
}

func TestMirrorQueueTiersAnAge(t *testing.T) {
	t.Parallel()

	q := &mirrorQueueFlags{
		tierActive:       time.Hour,
		tierRecent:       3 * time.Hour,
		tierDormant:      12 * time.Hour,
		tierActiveWithin: 24 * time.Hour,
		tierRecentWithin: 7 * 24 * time.Hour,
	}

	interval, tier := mirrorsweep.TierFor(time.Hour, tierOptions(q))
	require.Equal(t, time.Hour, interval)
	require.Equal(t, "active", tier)

	interval, tier = mirrorsweep.TierFor(48*time.Hour, tierOptions(q))
	require.Equal(t, 3*time.Hour, interval)
	require.Equal(t, "recent", tier)

	interval, tier = mirrorsweep.TierFor(200*time.Hour, tierOptions(q))
	require.Equal(t, 12*time.Hour, interval)
	require.Equal(t, "dormant", tier)
}

// TestMirrorQueuePublishesOnlyBranchesAndTags covers the references a change of
// which makes a mirror due: the review and checkpoint references of Graft must
// not.
func TestMirrorQueuePublishesOnlyBranchesAndTags(t *testing.T) {
	t.Parallel()

	require.True(t, mirrorsweep.PublishesRef("refs/heads/main"))
	require.True(t, mirrorsweep.PublishesRef("refs/tags/v1.0.0"))
	require.False(t, mirrorsweep.PublishesRef("refs/graft/reviews/1"))
	require.False(t, mirrorsweep.PublishesRef("HEAD"))
}

// TestMirrorQueueIdleRefusesWhatItCannotSet covers the declaration checks of
// mirror queue idle: at most one target, at least one interval, a fallback
// that fires sooner than the interval it stretches, and a name it does not
// hold.
func TestMirrorQueueIdleRefusesWhatItCannotSet(t *testing.T) {
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
	require.Contains(t, stderr, "requires --idle-interval")

	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "--queue-file", queue, "--idle-interval", "1m")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "shorter than the interval")

	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "nosuch", "--queue-file", queue, "--idle-interval", "10m")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, `no target named "nosuch"`)
}

// TestMirrorQueueIdleSetsTheFailureInterval covers the fallback that stretches
// while a target fails: it is set alone, and the command says so.
func TestMirrorQueueIdleSetsTheFailureInterval(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{{
		Name:       "gitee",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Interval:   5 * time.Minute,
		Enabled:    true,
	}}})

	code, stdout, stderr := runCLI(t, "mirror", "queue", "idle", "--queue-file", queue, "--failure-interval", "10m")
	require.Equal(t, exitOK, code, "%s", stderr)
	require.Contains(t, stdout, "between two attempts while it fails")

	store := mirrorqueue.NewFileStore(queue)
	loaded, err := store.Load()
	require.NoError(t, err)
	target, ok := loaded.Target("gitee")
	require.True(t, ok)
	require.Equal(t, 10*time.Minute, target.FailureInterval)
	require.Zero(t, target.IdleInterval, "the idle interval is left alone")
}

// TestMirrorQueueTierRefusesWhatItCannotSet covers the declaration checks of
// mirror queue tier: at most one target, three positive intervals, and a
// window that holds a recent repository between an active and a dormant one.
func TestMirrorQueueTierRefusesWhatItCannotSet(t *testing.T) {
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "tier", "a", "b", "--queue-file", queue)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "at most the name of one target")

	code, _, stderr = runCLI(t, "mirror", "queue", "tier", "--queue-file", queue,
		"--active", "0s", "--recent", "3h", "--dormant", "12h")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "each a positive duration")

	code, _, stderr = runCLI(t, "mirror", "queue", "tier", "--queue-file", queue,
		"--active", "1h", "--recent", "3h", "--dormant", "12h",
		"--active-within", "192h", "--recent-within", "168h")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "longer than --active-within")
}
