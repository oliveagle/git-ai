package identity_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/identity"
)

func TestContext(t *testing.T) {
	t.Parallel()

	t.Run("an identity is read back", func(t *testing.T) {
		t.Parallel()

		ctx := identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
		id, ok := identity.FromContext(ctx)
		require.True(t, ok)
		require.Equal(t, identity.Identity{Name: "agent-a", Source: identity.SourceAgent}, id)
	})

	t.Run("a context without an identity has none", func(t *testing.T) {
		t.Parallel()

		_, ok := identity.FromContext(context.Background())
		require.False(t, ok)
	})

	t.Run("an identity without a name is not an identity", func(t *testing.T) {
		t.Parallel()

		ctx := identity.NewContext(context.Background(), identity.Identity{Source: identity.SourceAdmin})
		_, ok := identity.FromContext(ctx)
		require.False(t, ok)
	})

	t.Run("a value of another type is not an identity", func(t *testing.T) {
		t.Parallel()

		ctx := identity.NewContext(context.Background(), identity.Identity{Name: "a"})
		_, ok := identity.FromContext(ctx)
		require.True(t, ok)

		ctx = context.WithValue(context.Background(), struct{}{}, identity.Identity{Name: "a"})
		_, ok = identity.FromContext(ctx)
		require.False(t, ok)
	})
}

func TestActor(t *testing.T) {
	t.Parallel()

	t.Run("the name of the identity", func(t *testing.T) {
		t.Parallel()

		ctx := identity.NewContext(context.Background(), identity.Identity{Name: "agent-a"})
		require.Equal(t, "agent-a", identity.Actor(ctx, "fallback"))
	})

	t.Run("the fallback without an identity", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, "fallback", identity.Actor(context.Background(), "fallback"))
	})
}
