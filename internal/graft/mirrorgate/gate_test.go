package mirrorgate_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
)

// policy is the policy of the tests.
func policy() mirrorgate.Policy {
	return mirrorgate.Policy{Required: true, WarnLag: 10, DenyLag: 20}
}

// target returns a mirror of the branch main.
func target(name string, lag int) mirrorgate.Target {
	return mirrorgate.Target{
		Name:     name,
		URL:      "https://github.com/group/shop.git",
		Branches: []string{"main"},
		Enabled:  true,
		Synced:   true,
		Status:   "ok",
		Lag:      lag,
	}
}

// input returns the state of a push to main.
func input(p mirrorgate.Policy, targets ...mirrorgate.Target) mirrorgate.Input {
	return mirrorgate.Input{Policy: p, Repository: "shop.git", Branch: "main", Targets: targets}
}

func TestDecideAllowsWhenThePolicyIsOff(t *testing.T) {
	decision := mirrorgate.Decide(input(mirrorgate.Policy{}))
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, decision.Code)
	require.Empty(t, decision.Warning)
}

func TestDecideRefusesARepositoryWithoutATarget(t *testing.T) {
	decision := mirrorgate.Decide(input(policy()))

	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeNoTarget, decision.Code)
	require.Contains(t, decision.Reason, "shop.git")
	require.Contains(t, decision.Reason, "graft mirror queue add")
}

func TestDecideIgnoresADisabledTarget(t *testing.T) {
	disabled := target("github", 0)
	disabled.Enabled = false

	decision := mirrorgate.Decide(input(policy(), disabled))
	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeNoTarget, decision.Code)
}

func TestDecideRefusesAMirrorThatNeverPublished(t *testing.T) {
	never := target("github", 0)
	never.Synced = false
	never.Status = ""
	never.Lag = 0

	decision := mirrorgate.Decide(input(policy(), never))
	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeNeverSynced, decision.Code)
	require.Contains(t, decision.Reason, "graft mirror queue run")
}

func TestDecideAllowsAPublishedMirror(t *testing.T) {
	decision := mirrorgate.Decide(input(policy(), target("github", 0)))
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, decision.Code)
	require.Empty(t, decision.Warning)
}

func TestDecideWarnsWhenAMirrorIsBehind(t *testing.T) {
	decision := mirrorgate.Decide(input(policy(), target("github", 10)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Warning, "10 commits behind")
	require.Contains(t, decision.Warning, "graft mirror queue run")
}

func TestDecideRefusesWhenAMirrorIsTooFarBehind(t *testing.T) {
	decision := mirrorgate.Decide(input(policy(), target("github", 20)))

	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Reason, "20 commits behind")
}

func TestDecideWarnsWithoutARefusalLimit(t *testing.T) {
	p := mirrorgate.Policy{Required: true, WarnLag: 10}
	decision := mirrorgate.Decide(input(p, target("github", 100)))

	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Warning, "100 commits behind")
	require.NotContains(t, decision.Warning, "is refused")
}

func TestDecideRefusesADivergedMirror(t *testing.T) {
	diverged := target("github", 3)
	diverged.Diverged = true

	decision := mirrorgate.Decide(input(policy(), diverged))
	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeDiverged, decision.Code)
	require.Contains(t, decision.Reason, "--force")
}

func TestDecideWarnsAboutADivergedMirrorWhenItIsNotRequired(t *testing.T) {
	diverged := target("github", 3)
	diverged.Diverged = true

	decision := mirrorgate.Decide(input(mirrorgate.Policy{WarnLag: 10}, diverged))
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeDiverged, decision.Code)
	require.NotEmpty(t, decision.Warning)
}

func TestDecideWarnsAboutABranchNoMirrorPublishes(t *testing.T) {
	in := input(policy(), target("github", 0))
	in.Branch = "feature/x"

	decision := mirrorgate.Decide(in)
	require.True(t, decision.Allowed, "a branch a person works on is not the mainline")
	require.Equal(t, mirrorgate.CodeBranchNotMirrored, decision.Code)
	require.Contains(t, decision.Warning, "feature/x")
}

func TestDecideAllowsABranchNoMirrorPublishesWithoutThePolicy(t *testing.T) {
	in := input(mirrorgate.Policy{WarnLag: 10}, target("github", 0))
	in.Branch = "feature/x"

	decision := mirrorgate.Decide(in)
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, decision.Code)
	require.Empty(t, decision.Warning)
}

func TestDecideFollowsTheStrictestMirror(t *testing.T) {
	decision := mirrorgate.Decide(input(policy(), target("github", 0), target("gitee", 25)))

	require.False(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, decision.Code)
	require.Contains(t, decision.Reason, "gitee")
}

func TestDecideWarnsAboutATargetThatNeverPublished(t *testing.T) {
	never := target("gitee", 0)
	never.Synced = false
	never.Status = ""

	decision := mirrorgate.Decide(input(policy(), target("github", 0), never))
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeNotSynced, decision.Code)
	require.Contains(t, decision.Warning, "gitee")
}

func TestDecideIgnoresATargetOfAnotherBranch(t *testing.T) {
	release := target("release", 30)
	release.Branches = []string{"release/*"}

	decision := mirrorgate.Decide(input(policy(), target("github", 0), release))
	require.True(t, decision.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, decision.Code)
}

func TestTargetPublishes(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		branch  string
		want    bool
	}{
		{pattern: "main", branch: "main", want: true},
		{pattern: "main", branch: "mainline", want: false},
		{pattern: "*", branch: "main", want: true},
		{pattern: "release/*", branch: "release/1.0", want: true},
		{pattern: "release/*", branch: "main", want: false},
	} {
		target := mirrorgate.Target{Name: "t", Branches: []string{tc.pattern}}
		require.Equal(t, tc.want, target.Publishes(tc.branch), "pattern %q against %q", tc.pattern, tc.branch)
	}
	require.False(t, mirrorgate.Target{}.Publishes("main"))
}

func TestPolicyEnabled(t *testing.T) {
	require.False(t, mirrorgate.Policy{}.Enabled())
	require.True(t, mirrorgate.Policy{Required: true}.Enabled())
	require.True(t, mirrorgate.Policy{WarnLag: 10}.Enabled())
	require.True(t, mirrorgate.Policy{DenyLag: 10}.Enabled())
}
