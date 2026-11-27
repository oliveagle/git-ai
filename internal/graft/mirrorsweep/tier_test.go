package mirrorsweep

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTierForDividesTheRepositoriesByTheAgeOfTheirLastChange(t *testing.T) {
	options := TierOptions{
		Active:       time.Hour,
		Recent:       3 * time.Hour,
		Dormant:      12 * time.Hour,
		ActiveWithin: 24 * time.Hour,
		RecentWithin: 7 * 24 * time.Hour,
	}

	interval, tier := TierFor(time.Minute, options)
	require.Equal(t, time.Hour, interval)
	require.Equal(t, "active", tier)

	interval, tier = TierFor(24*time.Hour, options)
	require.Equal(t, time.Hour, interval)
	require.Equal(t, "active", tier)

	interval, tier = TierFor(25*time.Hour, options)
	require.Equal(t, 3*time.Hour, interval)
	require.Equal(t, "recent", tier)

	interval, tier = TierFor(8*24*time.Hour, options)
	require.Equal(t, 12*time.Hour, interval)
	require.Equal(t, "dormant", tier)
}

func TestRepoAgePrintsTheAgeWithoutANanosecond(t *testing.T) {
	require.Equal(t, "unknown age", RepoAge(-time.Second))
	require.Equal(t, "less than a minute", RepoAge(30*time.Second))
	require.Equal(t, "5m", RepoAge(5*time.Minute))
	require.Equal(t, "3h", RepoAge(3*time.Hour+30*time.Minute))
	require.Equal(t, "2d", RepoAge(49*time.Hour))
}

func TestPublishesRefKeepsTheInternalReferencesOut(t *testing.T) {
	require.True(t, PublishesRef("refs/heads/main"))
	require.True(t, PublishesRef("refs/tags/v1.0.0"))
	require.False(t, PublishesRef("refs/graft/reviews/12"))
	require.False(t, PublishesRef("refs/graft/checkpoints/abc"))
	require.False(t, PublishesRef("HEAD"))
}

func TestTaskIDHoldsTheMomentOfTheTask(t *testing.T) {
	first := TaskID()
	second := TaskID()
	require.NotEmpty(t, first)
	require.NotEqual(t, first, second)
}
