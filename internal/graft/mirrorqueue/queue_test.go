package mirrorqueue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTarget returns a target that is valid, for the tests to change.
func newTarget(name string) *Target {
	return &Target{
		Name:       name,
		Repository: "shop.git",
		URL:        "https://gitee.com/group/shop.git",
		Branches:   []string{"main"},
		Enabled:    true,
	}
}

func TestAddTargetRefusesAnIncompleteTarget(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		change func(*Target)
		want   string
	}{
		{desc: "without a name", change: func(t *Target) { t.Name = "" }, want: "needs a name"},
		{desc: "with a space in the name", change: func(t *Target) { t.Name = "two words" }, want: "holds a space"},
		{desc: "without a repository", change: func(t *Target) { t.Repository = "" }, want: "needs a repository"},
		{desc: "without a URL", change: func(t *Target) { t.URL = "" }, want: "needs a URL"},
		{desc: "without a branch", change: func(t *Target) { t.Branches = nil }, want: "needs at least one branch pattern"},
		{desc: "with an empty branch", change: func(t *Target) { t.Branches = []string{" "} }, want: "is empty"},
		{desc: "with two credentials", change: func(t *Target) { t.SSHKey = "/tmp/key"; t.TokenEnv = "GITEE_TOKEN" }, want: "not both"},
		{desc: "with a negative interval", change: func(t *Target) { t.Interval = -time.Minute }, want: "is negative"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			target := newTarget("gitee")
			tc.change(target)
			q := New()
			require.ErrorContains(t, q.AddTarget(target), tc.want)
			require.Empty(t, q.Targets)
		})
	}
}

func TestAddTargetRefusesASecondTargetOfTheSameName(t *testing.T) {
	q := New()
	require.NoError(t, q.AddTarget(newTarget("gitee")))
	require.ErrorContains(t, q.AddTarget(newTarget("gitee")), "already has a target named")
	require.Len(t, q.Targets, 1)
}

func TestRemoveTarget(t *testing.T) {
	q := New()
	require.NoError(t, q.AddTarget(newTarget("gitee")))
	require.NoError(t, q.AddTarget(newTarget("github")))

	require.NoError(t, q.RemoveTarget("gitee"))
	require.Len(t, q.Targets, 1)
	_, ok := q.Target("gitee")
	require.False(t, ok)

	require.ErrorContains(t, q.RemoveTarget("gitee"), "has no target named")
}

func TestDueSelectsTheTargetsWhoseScheduleSaysSo(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	q := New()

	due := newTarget("due")
	due.Interval = 5 * time.Minute
	due.NextRun = now.Add(-time.Minute)
	require.NoError(t, q.AddTarget(due))

	later := newTarget("later")
	later.Interval = 5 * time.Minute
	later.NextRun = now.Add(time.Minute)
	require.NoError(t, q.AddTarget(later))

	disabled := newTarget("disabled")
	disabled.Interval = 5 * time.Minute
	disabled.NextRun = now.Add(-time.Minute)
	disabled.Enabled = false
	require.NoError(t, q.AddTarget(disabled))

	manual := newTarget("manual")
	require.NoError(t, q.AddTarget(manual))

	dueTargets := q.Due(now)
	require.Len(t, dueTargets, 1)
	require.Equal(t, "due", dueTargets[0].Name)
}

func TestSetNextRun(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

	scheduled := newTarget("scheduled")
	scheduled.Interval = 5 * time.Minute
	scheduled.SetNextRun(now)
	require.Equal(t, now.Add(5*time.Minute), scheduled.NextRun)

	manual := newTarget("manual")
	manual.SetNextRun(now)
	require.True(t, manual.NextRun.IsZero(), "a target without a schedule is never due")
}

// TestQuietTargetStretchesTowardsItsIdleInterval checks the schedule of a
// target nobody touched: the delay doubles once per quiet run, up to the
// idle interval, and a change puts the target back on its interval.
func TestQuietTargetStretchesTowardsItsIdleInterval(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	target := newTarget("quiet")
	target.Interval = 5 * time.Minute
	target.IdleInterval = time.Hour

	target.SetNextRun(now)
	require.Equal(t, 5*time.Minute, target.NextDelay())

	// A run that found the mirror up to date doubles the delay.
	target.Observe(false, true)
	require.Equal(t, 1, target.QuietRuns)
	require.Equal(t, 10*time.Minute, target.NextDelay())

	target.Observe(false, true)
	require.Equal(t, 20*time.Minute, target.NextDelay())

	// The idle interval is the ceiling.
	for i := 0; i < 10; i++ {
		target.Observe(false, true)
	}
	require.Equal(t, time.Hour, target.NextDelay())

	// A change makes the target active again.
	target.Observe(true, true)
	require.Equal(t, 0, target.QuietRuns)
	require.Equal(t, 5*time.Minute, target.NextDelay())

	// A run that could not read the changeset log of the repository says
	// nothing, so the schedule stays where it was.
	target.Observe(false, false)
	require.Equal(t, 0, target.QuietRuns)
	require.Equal(t, 5*time.Minute, target.NextDelay())
}

// TestTargetWithoutIdleIntervalKeepsItsInterval checks that a target whose
// operator named no idle interval is published on its interval, whatever its
// quiet runs say.
func TestTargetWithoutIdleIntervalKeepsItsInterval(t *testing.T) {
	target := newTarget("fixed")
	target.Interval = 5 * time.Minute

	target.Observe(false, true)
	target.Observe(false, true)
	require.Equal(t, 5*time.Minute, target.NextDelay())
}

// TestTargetRefusesAnIdleIntervalShorterThanItsInterval checks the
// declaration of a target: the fallback may not be shorter than the schedule
// it backs up.
func TestTargetRefusesAnIdleIntervalShorterThanItsInterval(t *testing.T) {
	target := newTarget("wrong")
	target.Interval = 5 * time.Minute
	target.IdleInterval = time.Minute
	require.ErrorContains(t, target.Validate(), "shorter than its interval")

	target.IdleInterval = 5 * time.Minute
	require.NoError(t, target.Validate())
}

// TestRetryKeepsAFailedTargetOnItsInterval checks that a failure is not
// silence: a target whose publication failed waits its interval, not the
// stretched delay of a quiet target.
func TestRetryKeepsAFailedTargetOnItsInterval(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	target := newTarget("failed")
	target.Interval = 5 * time.Minute
	target.IdleInterval = time.Hour
	target.Observe(false, true)
	target.Observe(false, true)
	require.Equal(t, 20*time.Minute, target.NextDelay())

	target.Retry(now)
	require.Equal(t, now.Add(5*time.Minute), target.NextRun)
}

func TestLastTasksAreTheNewestOnesOfTheHistory(t *testing.T) {
	q := New()
	for i, name := range []string{"one", "two", "three"} {
		q.AddTask(&Task{ID: name, Target: "gitee", Status: TaskOK, CreatedAt: time.Now().Add(time.Duration(i) * time.Second)})
	}
	q.AddTask(&Task{ID: "other", Target: "github", Status: TaskOK})

	tasks := q.LastTasks("", 0)
	require.Len(t, tasks, 4)
	require.Equal(t, "other", tasks[0].ID, "the newest task comes first")

	tasks = q.LastTasks("", 2)
	require.Len(t, tasks, 2)
	require.Equal(t, "other", tasks[0].ID)
	require.Equal(t, "three", tasks[1].ID)

	tasks = q.LastTasks("gitee", 0)
	require.Len(t, tasks, 3)
	for _, task := range tasks {
		require.Equal(t, "gitee", task.Target)
	}
}

func TestTrimTasksKeepsTheLastOnes(t *testing.T) {
	q := New()
	for i := 0; i < 10; i++ {
		q.AddTask(&Task{ID: string(rune('a' + i))})
	}
	q.TrimTasks(3)
	require.Len(t, q.Tasks, 3)
	require.Equal(t, "h", q.Tasks[0].ID)
	require.Equal(t, "j", q.Tasks[2].ID)
}

func TestValidateRefusesAQueueOfAnUnknownVersion(t *testing.T) {
	q := New()
	q.Version = 99
	require.ErrorContains(t, q.Validate(), "version 99")
}

// TestUpdateTargetKeepsTheHistory checks that restating a target — a new
// schedule, a new URL, other branches — is not the target being added again:
// what the queue recorded about it, and whether a person disabled it, stays.
func TestUpdateTargetKeepsTheHistory(t *testing.T) {
	q := New()
	added := newTarget("gitee")
	added.CreatedAt = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	added.Interval = time.Hour
	added.LastTask = "task-1"
	added.LastStatus = TaskOK
	require.NoError(t, q.AddTarget(added))

	restated := newTarget("gitee")
	restated.URL = "https://gitee.com/group/shop-mirror.git"
	restated.Branches = []string{"main", "release/*"}
	restated.Interval = 5 * time.Minute
	restated.Enabled = false
	stored, err := q.UpdateTarget(restated)
	require.NoError(t, err)

	require.Equal(t, "https://gitee.com/group/shop-mirror.git", stored.URL)
	require.Equal(t, []string{"main", "release/*"}, stored.Branches)
	require.Equal(t, 5*time.Minute, stored.Interval)
	require.True(t, stored.Enabled, "a restatement of the schedule is not a target that a person enabled")
	require.Equal(t, added.CreatedAt, stored.CreatedAt)
	require.Equal(t, "task-1", stored.LastTask)
	require.Equal(t, TaskOK, stored.LastStatus)
	require.False(t, stored.UpdatedAt.IsZero())

	require.Len(t, q.Targets, 1, "an update replaces the target instead of adding one")
}

// TestUpdateTargetRefusesATargetTheQueueDoesNotHold checks that an update
// cannot create a target: that is what add is for, and a typo in a name must
// not declare a mirror.
func TestUpdateTargetRefusesATargetTheQueueDoesNotHold(t *testing.T) {
	q := New()
	_, err := q.UpdateTarget(newTarget("gitee"))
	require.ErrorContains(t, err, "no target named")
	require.Empty(t, q.Targets)
}
