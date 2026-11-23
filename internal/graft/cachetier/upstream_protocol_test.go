package cachetier

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestGetObjectsRejectsAnUpstreamThatBreaksTheProtocol checks every answer an
// upstream must never give: the cache trusts what it stores, so a single
// object it cannot vouch for fails the whole request instead of being cached.
func TestGetObjectsRejectsAnUpstreamThatBreaksTheProtocol(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		request func(a, b string) []string
		frames  func(a, b string) []*gitalypb.GetObjectsResponse
		want    string
	}{
		{
			name:    "an object nobody asked for",
			frames:  func(a, b string) []*gitalypb.GetObjectsResponse { return frames(b, 1, []byte("x")) },
			request: func(a, b string) []string { return []string{a} },
			want:    "unrequested object",
		},
		{
			name: "the same object twice",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				first := frames(a, 5, []byte("first"))
				second := frames(b, 6, []byte("second"))
				return append(append(first, second...), frames(a, 5, []byte("first"))...)
			},
			request: func(a, b string) []string { return []string{a, b} },
			want:    "twice",
		},
		{
			name: "a first chunk at an offset",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{{Oid: a, Type: gitalypb.ObjectType_BLOB, Size: 3, Offset: 3, Data: []byte("x")}}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "starts at offset",
		},
		{
			name: "a negative size",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{{Oid: a, Type: gitalypb.ObjectType_BLOB, Size: -1}}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "negative size",
		},
		{
			name: "a type that is not a git type",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{{Oid: a, Type: gitalypb.ObjectType(99), Size: 1}}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "unsupported type",
		},
		{
			name: "a chunk out of place",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{
					{Oid: a, Type: gitalypb.ObjectType_BLOB, Size: 3, Offset: 0, Data: []byte("x")},
					{Oid: a, Offset: 5, Data: []byte("y")},
				}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "expected 1",
		},
		{
			name: "more data than the size it declared",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{{Oid: a, Type: gitalypb.ObjectType_BLOB, Size: 1, Data: []byte("xy")}}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "more data than its declared size",
		},
		{
			name: "fewer bytes than the size it declared",
			frames: func(a, b string) []*gitalypb.GetObjectsResponse {
				return []*gitalypb.GetObjectsResponse{{Oid: a, Type: gitalypb.ObjectType_BLOB, Size: 4, Data: []byte("xy")}}
			},
			request: func(a, b string) []string { return []string{a} },
			want:    "got 2 of 4 bytes",
		},
		{
			name:    "nothing at all",
			frames:  func(a, b string) []*gitalypb.GetObjectsResponse { return nil },
			request: func(a, b string) []string { return []string{a} },
			want:    "did not return object",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			repo := testRepo(fmt.Sprintf("@hashed/%s.git", names(tc.name)))
			upstream := newFakeObjectService()
			a := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("first"), false)
			b := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("second"), false)

			upstream.mu.Lock()
			if upstream.raw == nil {
				upstream.raw = map[string][]*gitalypb.GetObjectsResponse{}
			}
			upstream.raw[a] = tc.frames(a, b)
			upstream.mu.Unlock()

			tier := newTier(t, upstream, Config{})

			err := objectsErr(ctx, tier.client, &gitalypb.GetObjectsRequest{Repository: repo, Oids: tc.request(a, b)})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// frames is one object answered in a single message: the whole object at offset
// zero, with the size and type the object has.
func frames(oid string, size int64, data []byte) []*gitalypb.GetObjectsResponse {
	return []*gitalypb.GetObjectsResponse{{
		Oid:  oid,
		Type: gitalypb.ObjectType_BLOB,
		Size: size,
		Data: data,
	}}
}

// names turns a subtest name into a repository path.
func names(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
