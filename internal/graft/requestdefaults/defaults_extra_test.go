package requestdefaults

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestFillDefaultsIgnoresWhatIsNotThere covers the request that is not a
// message and the request without a repository: nothing is created.
func TestFillDefaultsIgnoresWhatIsNotThere(t *testing.T) {
	ctx := context.Background()

	fillDefaults(ctx, (*gitalypb.UserCreateBranchRequest)(nil))
	fillDefaults(ctx, &gitalypb.UserCreateBranchRequest{})

	req := &gitalypb.UserCreateBranchRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "shop.git"}}
	fillDefaults(ctx, req)
	require.Equal(t, "graft:default/shop.git", string(req.GetRepository().GetGlRepository()))
}
