package changedpaths

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rawTreeEntry encodes one entry of the raw content of a tree.
func rawTreeEntry(mode, name string, oid []byte) string {
	return mode + " " + name + "\x00" + string(oid)
}

// TestParseTreeRefusesMalformedContent covers the content of a tree that git
// never writes but a corrupted object may hold: the walk must refuse it with
// the reason, not read a truncated object ID as an entry.
func TestParseTreeRefusesMalformedContent(t *testing.T) {
	t.Parallel()

	oid := strings.Repeat("\x01", 20)

	for _, tc := range []struct {
		name    string
		data    string
		message string
	}{
		{"no mode", "100644", "missing mode"},
		{"mode that is not octal", "10x644 a\x00" + oid, "invalid mode"},
		{"no name", "100644 a-without-a-nul", "missing name"},
		{"object ID cut short", "100644 a\x00" + oid[:19], "truncated object ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseTree([]byte(tc.data), 20)
			require.ErrorContains(t, err, tc.message)
		})
	}
}

// TestParseTreeCanonicalisesModes covers the modes a tree holds: canonMode is
// what makes an old 100664 file equal to a 100644 one, so a change of the mode
// bits git ignores must not show up as a changed path.
func TestParseTreeCanonicalisesModes(t *testing.T) {
	t.Parallel()

	oid := []byte(strings.Repeat("\x02", 20))
	data := []string{
		rawTreeEntry("100644", "file", oid),
		rawTreeEntry("100664", "old-file", oid),
		rawTreeEntry("100755", "script", oid),
		rawTreeEntry("120000", "link", oid),
		rawTreeEntry("040000", "dir", oid),
		rawTreeEntry("160000", "submodule", oid),
		rawTreeEntry("140000", "odd", oid),
	}

	entries, err := parseTree([]byte(strings.Join(data, "")), 20)
	require.NoError(t, err)

	expected := []Entry{
		{Name: "file", Mode: 0o100644, OID: hex.EncodeToString(oid)},
		{Name: "old-file", Mode: 0o100644, OID: hex.EncodeToString(oid)},
		{Name: "script", Mode: 0o100755, OID: hex.EncodeToString(oid)},
		{Name: "link", Mode: 0o120000, OID: hex.EncodeToString(oid)},
		{Name: "dir", Mode: treeMode, OID: hex.EncodeToString(oid)},
		{Name: "submodule", Mode: 0o160000, OID: hex.EncodeToString(oid)},
		{Name: "odd", Mode: 0o160000, OID: hex.EncodeToString(oid)},
	}
	require.Equal(t, expected, entries)

	empty, err := parseTree(nil, 20)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestTreeReadsNothingForAnAbsentTree covers the empty ID, which stands for an
// absent tree: it is read as no entry, never as the tree of the empty ID.
func TestTreeReadsNothingForAnAbsentTree(t *testing.T) {
	t.Parallel()

	reader := &ObjectReader{}
	entries, err := reader.Tree(context.Background(), "")
	require.NoError(t, err)
	require.Nil(t, entries)
}
