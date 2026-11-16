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
