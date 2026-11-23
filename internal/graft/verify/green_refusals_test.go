package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/greenref"
)

// TestAdvanceGreenReportsAReferenceItCannotRead covers the reads of the move
// that publishes a checkpoint: the checkpoint, the tree it certifies, and the
// suites it certifies are read before anything moves, and a ref store that
// refuses one of them stops the move.
func TestAdvanceGreenReportsAReferenceItCannotRead(t *testing.T) {
	gone := errors.New("the ref store is gone")

	t.Run("the checkpoint", func(t *testing.T) {
		fx := newFixture(t, nil)
		tip := fx.commit("a.txt", "one")
		v := fx.verifier()
		fx.greens.failCurrentOn = map[string]error{fx.cfg.GreenRef: gone}

		_, err := v.advanceGreen(context.Background(), tip)
		require.ErrorContains(t, err, "read green ref")
	})

	t.Run("the tree of the commit", func(t *testing.T) {
		fx := newFixture(t, nil)
		tip := fx.commit("a.txt", "one")
		v := fx.verifier()
		fx.greens.failTree = gone

		_, err := v.advanceGreen(context.Background(), tip)
		require.ErrorContains(t, err, "reading the tree of")
	})

	t.Run("the tree the checkpoint certifies", func(t *testing.T) {
		fx := newFixture(t, nil)
		tip := fx.commit("a.txt", "one")
		v := fx.verifier()
		fx.greens.failCurrentOn = map[string]error{greenref.TreeNameFor(fx.cfg.GreenRef): gone}

		_, err := v.advanceGreen(context.Background(), tip)
		require.ErrorContains(t, err, "read the tree the checkpoint certifies")
	})

	t.Run("a suite", func(t *testing.T) {
		fx := newFixture(t, func(cfg *Config) { cfg.Test.Suites = []string{"unit"} })
		tip := fx.commit("a.txt", "one")
		v := fx.verifier()
		fx.greens.failCurrentOn = map[string]error{greenref.SuitesName("unit", fx.cfg.Branch): gone}

		_, err := v.advanceGreen(context.Background(), tip)
		require.ErrorContains(t, err, `the commit the suite "unit" certified`)
	})
}

// TestAdvanceGreenAnswersACheckpointThatIsAlreadyThere covers the checkpoint
// that already names the tip and the tree: nothing moves, and the verifier
// learns the green tip from the store instead of publishing it again.
func TestAdvanceGreenAnswersACheckpointThatIsAlreadyThere(t *testing.T) {
	fx := newFixture(t, nil)
	tip := fx.commit("a.txt", "one")
	fx.greens.refs[fx.cfg.GreenRef] = tip
	fx.greens.refs[greenref.TreeNameFor(fx.cfg.GreenRef)] = "tree-of-" + tip

	v := fx.verifier()
	moved, err := v.advanceGreen(context.Background(), tip)
	require.NoError(t, err)
	require.False(t, moved, "the checkpoint already names the tip")
	require.Equal(t, tip, v.green, "the verifier read the green tip")
	require.Zero(t, fx.greens.moves, "nothing moved")
}

// TestSuitesPublishedReportsASuiteThatIsNotThere covers the checkpoint whose
// tree is certified but whose suites are not: the checkpoint does not carry
// them, so it is not published as it is.
func TestSuitesPublishedReportsASuiteThatIsNotThere(t *testing.T) {
	fx := newFixture(t, func(cfg *Config) { cfg.Test.Suites = []string{"unit"} })
	tip := fx.commit("a.txt", "one")
	fx.greens.refs[fx.cfg.GreenRef] = tip
	fx.greens.refs[greenref.TreeNameFor(fx.cfg.GreenRef)] = "tree-of-" + tip

	v := fx.verifier()
	require.False(t, v.suitesPublished(context.Background(), tip), "the suite certified nothing")

	moved, err := v.advanceGreen(context.Background(), tip)
	require.NoError(t, err)
	require.False(t, moved, "the checkpoint itself did not move")
	require.Equal(t, tip, fx.greens.get(greenref.SuitesName("unit", fx.cfg.Branch)))
}

// TestCommitReportsAStateFileItCannotWrite covers the cursor that cannot be
// persisted: the verifier reports where it could not write, and does not
// pretend the cursor moved.
func TestCommitReportsAStateFileItCannotWrite(t *testing.T) {
	fx := newFixture(t, nil)
	v := fx.verifier()

	// The state path becomes a directory: the temporary file is written, and
	// replacing the directory with it is refused.
	v.cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.Mkdir(v.cfg.StateFile, 0o755))

	err := v.commit(7)
	require.ErrorContains(t, err, "replace state file")
	require.Zero(t, v.cursor, "the cursor did not move")
}

// TestRunReportsThatItIsBusy covers the second run of one verifier: the lock
// is held, and the run says so instead of following the log twice.
func TestRunReportsThatItIsBusy(t *testing.T) {
	fx := newFixture(t, nil)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	require.True(t, v.running.TryLock())
	defer v.running.Unlock()

	require.ErrorIs(t, v.Run(context.Background()), ErrBusy)
}
