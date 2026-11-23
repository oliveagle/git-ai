package agentrefs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
)

// TestVisibleToAgents checks both rules: the namespace of the server is hidden,
// and so is every internal prefix that git itself keeps hidden from users, while
// the other internal prefixes stay readable.
func TestVisibleToAgents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ref  string
		want bool
	}{
		{"refs/heads/main", true},
		{"refs/tags/v1.0.0", true},
		{"refs/graft/proposals/123", false},
		{"refs/graft/green/main", false},
		{"refs/keep-around/abc", false},
		{"refs/remotes/origin/main", false},
		{"refs/tmp/tmp-1", false},
		// Internal to GitLab, but not hidden: an agent may see them.
		{"refs/environments/1", true},
		{"refs/merge-requests/1/head", true},
		{"refs/pipelines/42", true},
	}

	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, agentrefs.VisibleToAgents(tc.ref), "VisibleToAgents(%q)", tc.ref)
		})
	}
}
