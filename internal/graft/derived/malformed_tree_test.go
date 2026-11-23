package derived

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// writeRawTreeBytes writes raw bytes as a tree object without validation, so that
// the reader can meet the trees a damaged or hand-made object database can hold.
func writeRawTreeBytes(tb testing.TB, cfg config.Cfg, repoPath string, raw []byte) git.ObjectID {
	tb.Helper()

	out := gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: bytes.NewReader(raw)},
		"-C", repoPath, "hash-object", "-t", "tree", "--literally", "-w", "--stdin")
	oid, err := gittest.DefaultObjectHash.FromHex(string(bytes.TrimSpace(out)))
	require.NoError(tb, err)
	return oid
}

// TestLastMod_RejectsMalformedTrees checks that a tree whose bytes do not decode as
// tree entries is reported as an error instead of being read as an empty or a
// partial tree: a damaged object must not be mistaken for "the path is absent".
func TestLastMod_RejectsMalformedTrees(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("x\n"))
	raw, err := hex.DecodeString(blob.String())
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"missing mode":        {raw: []byte("100644"), want: "malformed tree: missing mode"},
		"invalid mode":        {raw: bytes.Join([][]byte{[]byte("1006x4 a\x00"), raw}, nil), want: "malformed tree: invalid mode"},
		"missing name":        {raw: []byte("100644 a"), want: "malformed tree: missing name"},
		"truncated object ID": {raw: bytes.Join([][]byte{[]byte("100644 a\x00"), raw[:6]}, nil), want: "malformed tree: truncated object ID"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			broken := writeRawTreeBytes(t, cfg, repoPath, tc.raw)
			tree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
				{Mode: "040000", Path: "d", OID: broken},
			})
			commit := gittest.WriteCommit(t, cfg, repoPath,
				gittest.WithBranch("malformed"),
				gittest.WithMessage(name),
				gittest.WithTree(tree),
			)

			deriver := newTestDeriver(t, ctx, cfg, repo, false)
			_, err := deriver.LastMod(ctx, commit, "d/x")
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "d")
		})
	}
}
