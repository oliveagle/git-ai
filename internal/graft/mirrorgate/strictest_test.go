package mirrorgate_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
)

// TestDecideTakesTheFirstOfTwoEquallyBehindMirrors pins which of two mirrors an
// equal lag leaves as the one that decides: the first one, so that the message
// is stable and does not depend on which mirror the policy happened to read
// last.
func TestDecideTakesTheFirstOfTwoEquallyBehindMirrors(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{WarnLag: 1}, target("github", 5), target("gitee", 5)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Warning, "mirror github")
}

// TestDecideKeepsTheMirrorThatIsFurtherBehind checks that a mirror that is
// further behind decides, even when another one is diverged.
func TestDecideKeepsTheMirrorThatIsFurtherBehind(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{WarnLag: 1}, target("github", 10), target("gitee", 3)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Warning, "mirror github")
	require.Contains(t, decision.Warning, "10 commits behind")
}

// TestDecideLetsADivergedMirrorDecide checks that a diverged mirror decides over
// one that is equally behind but not diverged.
func TestDecideLetsADivergedMirrorDecide(t *testing.T) {
	diverged := target("gitee", 5)
	diverged.Diverged = true

	decision := mirrorgate.Decide(input(mirrorgate.Policy{Required: true}, target("github", 5), diverged))

	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeDiverged, decision.Code)
	require.Contains(t, decision.Reason, "gitee")
}

// TestPolicyEnabledAtTheSmallestLimit checks that a limit of one is a limit: a
// policy that is off only when every field is zero.
func TestPolicyEnabledAtTheSmallestLimit(t *testing.T) {
	require.True(t, mirrorgate.Policy{WarnLag: 1}.Enabled())
	require.True(t, mirrorgate.Policy{DenyLag: 1}.Enabled())
}

// TestDecideRefusesAtTheSmallestDenyLimit checks that a deny limit of one
// refuses a mirror one commit behind.
func TestDecideRefusesAtTheSmallestDenyLimit(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{DenyLag: 1}, target("github", 1)))

	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
}

// TestDecideDoesNotWarnWithoutAWarnLimit checks that a warn limit of zero is
// off: the decision says nothing about the lag.
func TestDecideDoesNotWarnWithoutAWarnLimit(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{Required: true}, target("github", 100)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, decision.Code)
	require.Empty(t, decision.Warning)
}

// TestDecideWarnsWithTheDenyLimit checks that the warning names the refusal
// limit whenever the policy has one, whatever its size.
func TestDecideWarnsWithTheDenyLimit(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{WarnLag: 1, DenyLag: 5}, target("github", 1)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Warning, "refused from 5 commits behind")
}
