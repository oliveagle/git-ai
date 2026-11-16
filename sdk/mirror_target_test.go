package sdk_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// fakeMirrors records the mirror targets a client declares and answers with
// the state of one published mirror.
type fakeMirrors struct {
	gitalypb.UnimplementedMirrorServiceServer

	registered   []*gitalypb.RegisterMirrorTargetRequest
	unregistered []*gitalypb.UnregisterMirrorTargetRequest
	listed       []*gitalypb.ListMirrorTargetsRequest
}

func (f *fakeMirrors) RegisterTarget(ctx context.Context, req *gitalypb.RegisterMirrorTargetRequest) (*gitalypb.RegisterMirrorTargetResponse, error) {
	f.registered = append(f.registered, req)
	return &gitalypb.RegisterMirrorTargetResponse{Target: &gitalypb.MirrorTarget{
		Name:     req.GetName(),
		Url:      req.GetUrl(),
		Branches: req.GetBranches(),
		Enabled:  req.GetEnabled(),
	}}, nil
}

func (f *fakeMirrors) UnregisterTarget(ctx context.Context, req *gitalypb.UnregisterMirrorTargetRequest) (*gitalypb.UnregisterMirrorTargetResponse, error) {
	f.unregistered = append(f.unregistered, req)
	return &gitalypb.UnregisterMirrorTargetResponse{}, nil
}

func (f *fakeMirrors) ListTargets(ctx context.Context, req *gitalypb.ListMirrorTargetsRequest) (*gitalypb.ListMirrorTargetsResponse, error) {
	f.listed = append(f.listed, req)
	return &gitalypb.ListMirrorTargetsResponse{
		Policy: &gitalypb.MirrorPolicy{Required: true, WarnLag: 10, DenyLag: 20},
		Targets: []*gitalypb.MirrorTarget{{
			Name:        "codeup",
			Url:         "https://example.com/shop.git",
			Branches:    []string{"main"},
			Enabled:     true,
			Status:      "ok",
			PublishedAt: "2026-11-17T01:02:03Z",
			Published:   map[string]string{"main": "abc"},
			Lag:         3,
		}},
	}, nil
}

func TestRegisterMirrorTargetSendsTheTarget(t *testing.T) {
	t.Parallel()

	fake := &fakeMirrors{}
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterMirrorServiceServer(srv, fake)
	})

	target, err := client.Repo("shop.git").RegisterMirrorTarget(t.Context(), sdk.MirrorTargetOptions{
		Name:     "codeup",
		URL:      "https://example.com/shop.git",
		Branches: []string{"main", "release/*"},
		Enabled:  true,
	})
	require.NoError(t, err)
	require.Equal(t, "codeup", target.Name)

	require.Len(t, fake.registered, 1)
	req := fake.registered[0]
	require.Equal(t, "shop.git", req.GetRepository().GetRelativePath())
	require.Equal(t, "codeup", req.GetName())
	require.Equal(t, "https://example.com/shop.git", req.GetUrl())
	require.Equal(t, []string{"main", "release/*"}, req.GetBranches())
	require.True(t, req.GetEnabled())
}

func TestRegisterMirrorTargetRefusesAnIncompleteTarget(t *testing.T) {
	t.Parallel()

	fake := &fakeMirrors{}
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterMirrorServiceServer(srv, fake)
	})

	for _, tc := range []struct {
		desc string
		opts sdk.MirrorTargetOptions
	}{
		{
			desc: "no name",
			opts: sdk.MirrorTargetOptions{URL: "https://example.com/shop.git", Branches: []string{"main"}},
		},
		{
			desc: "no URL",
			opts: sdk.MirrorTargetOptions{Name: "codeup", Branches: []string{"main"}},
		},
		{
			desc: "no branch pattern",
			opts: sdk.MirrorTargetOptions{Name: "codeup", URL: "https://example.com/shop.git"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			_, err := client.Repo("shop.git").RegisterMirrorTarget(t.Context(), tc.opts)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "error: %v", err)
		})
	}

	require.Empty(t, fake.registered)
}

func TestUnregisterMirrorTargetSendsTheName(t *testing.T) {
	t.Parallel()

	fake := &fakeMirrors{}
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterMirrorServiceServer(srv, fake)
	})

	require.NoError(t, client.Repo("shop.git").UnregisterMirrorTarget(t.Context(), "codeup"))

	require.Len(t, fake.unregistered, 1)
	require.Equal(t, "codeup", fake.unregistered[0].GetName())
	require.Equal(t, "shop.git", fake.unregistered[0].GetRepository().GetRelativePath())

	err := client.Repo("shop.git").UnregisterMirrorTarget(t.Context(), "  ")
	require.Equal(t, codes.InvalidArgument, status.Code(err), "error: %v", err)
}

func TestListMirrorTargetsReadsTheStateAndThePolicy(t *testing.T) {
	t.Parallel()

	fake := &fakeMirrors{}
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterMirrorServiceServer(srv, fake)
	})

	targets, policy, err := client.Repo("shop.git").ListMirrorTargets(t.Context(), "main")
	require.NoError(t, err)

	require.Len(t, fake.listed, 1)
	require.Equal(t, "main", fake.listed[0].GetBranch())

	require.True(t, policy.Required)
	require.Equal(t, 10, policy.WarnLag)
	require.Equal(t, 20, policy.DenyLag)

	require.Len(t, targets, 1)
	target := targets[0]
	require.Equal(t, "codeup", target.Name)
	require.Equal(t, "ok", target.Status)
	require.Equal(t, 3, target.Lag)
	require.Equal(t, map[string]string{"main": "abc"}, target.Published)
	require.False(t, target.PublishedAt.IsZero())
	require.Equal(t, "2026-11-17T01:02:03Z", target.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"))
}
