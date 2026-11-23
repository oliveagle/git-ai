package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPrintPolicyCheck covers the text of a candidate policy: a rule without an
// actor applies to everyone, a repository without changesets is named as such,
// and the denials of a repository are listed with their changesets.
func TestPrintPolicyCheck(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printPolicyCheck(&out, policyCheckResult{
		File:   "policy.toml",
		SHA256: "abc",
		Rules: []policyRule{
			{Number: 1, Allow: true},
			{Number: 2, Allow: false, Refs: []string{"refs/heads/main"}},
		},
		Repositories: []policyRepository{
			{Repository: "empty.git"},
			{
				Repository:    "shop.git",
				Checked:       3,
				FirstSequence: 1,
				LastSequence:  3,
				Denied: []policyDenial{{
					Actor: "alice", Reference: "refs/heads/main", Reason: "not_verified", Sequences: []uint64{2, 3},
				}},
			},
		},
	})

	text := out.String()
	require.Contains(t, text, "policy of policy.toml is valid: 2 rules, sha256 abc\n")
	require.Contains(t, text, "  1. allow *: every reference\n")
	require.Contains(t, text, "  2. deny *: refs/heads/main\n")
	require.Contains(t, text, "empty.git: no changesets\n")
	require.Contains(t, text, "shop.git: 3 changesets (1..3), 1 updates the candidate would deny\n")
	require.Contains(t, text, "  alice refs/heads/main (2 changesets, last 3): not_verified\n")
}
