package eager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestNewSchedulerAppliesItsDefaults covers the zero durations and the one
// inversion a caller can hand the scheduler: a zero debounce and a zero max
// wait take the defaults, and a max wait shorter than the debounce is lifted
// to it, so a write is never postponed past its own bound.
func TestNewSchedulerAppliesItsDefaults(t *testing.T) {
	ctx := testhelper.Context(t)
	write := func(context.Context, storage.Repository) error { return nil }

	s := NewScheduler(ctx, testhelper.NewLogger(t), write, 0, 0)
	require.Equal(t, DefaultDebounce, s.debounce)
	require.Equal(t, DefaultMaxWait, s.maxWait)
	s.Close()

	s = NewScheduler(ctx, testhelper.NewLogger(t), write, time.Minute, time.Second)
	require.Equal(t, time.Minute, s.debounce)
	require.Equal(t, time.Minute, s.maxWait)
	s.Close()
}
