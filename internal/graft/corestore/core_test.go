package corestore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestKeyOfNormalizesTheRelativePath(t *testing.T) {
	t.Parallel()

	want := RepoKey{Storage: "default", RelativePath: "group/project.git"}
	for _, rel := range []string{
		"group/project.git",
		"group//project.git",
		"./group/project.git",
		"group/./project.git",
		"group/project.git/",
		"/group/project.git",
		"group/sub/../project.git",
	} {
		require.Equal(t, want, KeyOf(&gitalypb.Repository{StorageName: "default", RelativePath: rel}), rel)
	}

	require.Equal(t, "@hashed/ab/cd/abcd.git", NormalizeRelativePath("@hashed/ab/cd/abcd.git"))
}
