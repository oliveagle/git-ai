package storage

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestNeedsSync(t *testing.T) {
	ctx := testhelper.Context(t)

	type mockTransaction struct{ Transaction }

	require.True(t, NeedsSync(ctx))
	require.False(t, NeedsSync(ContextWithTransaction(ctx, &mockTransaction{})))
}
