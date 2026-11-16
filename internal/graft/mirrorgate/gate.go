// Package mirrorgate decides whether a push may go through the mirror policy
// of the server ([[graft.mirror]]): every repository is published to at least
// one mirror, and a mirror that falls behind holds the pushes back.
//
// It is pure Go: the caller supplies the policy and the state of the mirrors
// of the repository, and gets back the decision. The reference-transaction
// hook decides every update of a branch with it (internal/gitaly/hook), and
// graft mirror status asks the same question to tell an operator what to
// repair (cmd/graft). See doc/graft/MIRROR.md.
package mirrorgate

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The codes of a decision, for a machine to read.
const (
	// CodeAllowed is the code of a push the policy allows.
	CodeAllowed = "allowed"
	// CodeNoTarget is the code of a repository without a mirror target.
	CodeNoTarget = "no_mirror_target"
	// CodeNeverSynced is the code of a repository whose mirrors never
	// published successfully.
	CodeNeverSynced = "mirror_never_synced"
	// CodeDiverged is the code of a mirror that holds history the repository
	// does not have.
	CodeDiverged = "mirror_diverged"
	// CodeLagging is the code of a mirror that is behind the repository.
	CodeLagging = "mirror_lagging"
	// CodeBranchNotMirrored is the code of a branch no target publishes. It
	// is a warning, not a refusal: a repository publishes its mainline and
	// still takes the branches a person works on.
	CodeBranchNotMirrored = "branch_not_mirrored"
	// CodeNotSynced is the code of a target that never published
	// successfully, among the targets of a repository whose other targets
	// did. It is a warning, not a refusal.
	CodeNotSynced = "mirror_not_synced"
)

// Policy is the mirror policy the server enforces ([graft.mirror]).
type Policy struct {
	// Required refuses a push to a repository that has no mirror target, or
	// whose mirrors never published successfully.
	Required bool
	// WarnLag is the number of commits a mirror may be behind before a push
	// is warned about. Zero turns the warning off.
	WarnLag int
	// DenyLag is the number of commits a mirror may be behind before a push
	// is refused. Zero turns the refusal off.
	DenyLag int
}

// Enabled tells whether the policy decides anything at all.
func (p Policy) Enabled() bool {
	return p.Required || p.WarnLag > 0 || p.DenyLag > 0
}

// Target is one mirror of the repository, with the state of its last
// publication and how far it is behind.
type Target struct {
	// Name is the name of the target, such as "github".
	Name string
	// URL is the Git URL of the mirror, with the user information of the URL
	// removed.
	URL string
	// Branches are the branch patterns the target publishes, without a
	// refs/heads/ prefix.
	Branches []string
	// Enabled tells whether the target is published.
	Enabled bool
	// Synced tells whether the target published successfully at least once.
	Synced bool
	// Status is the outcome of the last publication, empty when the target
	// never published.
	Status string
	// PublishedAt is when the mirror was published last.
	PublishedAt time.Time
	// Error is the reason of the last failure.
	Error string
	// Published are the commits the mirror holds, by branch name. A branch
	// the mirror never published is absent.
	Published map[string]string
	// Lag is the number of commits of the branch the mirror does not hold.
	// It is zero when the branch is on the mirror, and it is meaningless when
	// the target never published the branch.
	Lag int
	// Diverged tells whether the mirror holds the branch at a commit that is
	// not an ancestor of the commit the push writes.
	Diverged bool
}

// Publishes tells whether the target publishes the branch.
func (t Target) Publishes(branch string) bool {
	for _, pattern := range t.Branches {
		if matchBranchPattern(pattern, branch) {
			return true
		}
	}
	return false
}

// Input is the state one decision is made with.
type Input struct {
	// Policy is the mirror policy of the server.
	Policy Policy
	// Repository is the repository the push writes, as the operator names
	// it, for the reason of a refusal.
	Repository string
	// Branch is the branch the push writes, without the refs/heads/ prefix.
	Branch string
	// Targets are the mirror targets of the repository, with the state of
	// their last publication.
	Targets []Target
}

// Decision is the answer of the gate.
type Decision struct {
	// Allowed tells whether the push may go through.
	Allowed bool
	// Code names the outcome, for a machine to read.
	Code string
	// Reason explains the outcome for a human.
	Reason string
	// Warning is what the pusher is told when the push goes through but
	// something is wrong with a mirror. It is empty when nothing is.
	Warning string
}

// Decide decides one push.
func Decide(in Input) Decision {
	policy := in.Policy
	if !policy.Enabled() {
		return Decision{Allowed: true, Code: CodeAllowed}
	}

	enabled := make([]Target, 0, len(in.Targets))
	for _, target := range in.Targets {
		if target.Enabled {
			enabled = append(enabled, target)
		}
	}

	if policy.Required {
		if len(enabled) == 0 {
			return Decision{
				Code: CodeNoTarget,
				Reason: fmt.Sprintf(
					"the repository %s has no mirror target, and a repository accepts a push only once it is published to a mirror: add one with \"graft mirror queue add <name> --repo %s --url <url> --branch <pattern>\", then publish it with \"graft mirror queue run\"",
					in.Repository, in.Repository),
			}
		}

		synced := 0
		for _, target := range enabled {
			if target.Synced {
				synced++
			}
		}
		if synced == 0 {
			return Decision{
				Code: CodeNeverSynced,
				Reason: fmt.Sprintf(
					"no mirror of the repository %s ever published successfully: publish one with \"graft mirror queue run\" (or \"graft mirror push --target <name> ...\") and push again",
					in.Repository),
			}
		}
	}

	covering := make([]Target, 0, len(enabled))
	for _, target := range enabled {
		if target.Publishes(in.Branch) {
			covering = append(covering, target)
		}
	}

	if len(covering) == 0 {
		// The repository publishes its mainline; it still takes the branches
		// a person works on. The pusher is told, so that a branch that must be
		// mirrored is added to a target.
		decision := Decision{Allowed: true, Code: CodeAllowed}
		if policy.Required {
			decision.Code = CodeBranchNotMirrored
			decision.Warning = fmt.Sprintf(
				"the branch %s is not published to any mirror target of %s: add it to a target with \"graft mirror queue add <name> --repo %s --url <url> --branch %s\" if it must be mirrored",
				in.Branch, in.Repository, in.Repository, in.Branch)
		}
		return decision
	}

	// The strictest mirror of the branch decides: a mirror that is behind
	// holds the push back, whatever the others hold.
	worst := covering[0]
	for _, target := range covering[1:] {
		if target.Lag > worst.Lag || (target.Lag == worst.Lag && !worst.Diverged && target.Diverged) {
			worst = target
		}
		if target.Diverged && !worst.Diverged {
			worst.Diverged = true
		}
	}

	if worst.Diverged {
		if policy.Required {
			return Decision{
				Code: CodeDiverged,
				Reason: fmt.Sprintf(
					"the mirror %s of %s holds the branch %s at a commit the repository does not have: inspect it, then publish it again with \"graft mirror queue add <name> --repo %s --url <url> --branch %s --force\" and \"graft mirror queue run\"",
					worst.Name, in.Repository, in.Branch, in.Repository, in.Branch),
			}
		}
		return Decision{
			Allowed: true,
			Code:    CodeDiverged,
			Warning: fmt.Sprintf("the mirror %s of %s holds the branch %s at a commit the repository does not have: inspect it before it is published again", worst.Name, in.Repository, in.Branch),
		}
	}

	if policy.DenyLag > 0 && worst.Lag >= policy.DenyLag {
		return Decision{
			Code: CodeLagging,
			Reason: fmt.Sprintf(
				"the mirror %s of %s is %d commits behind the branch %s, and %d commits hold a push back: publish it with \"graft mirror queue run\" and push again",
				worst.Name, in.Repository, worst.Lag, in.Branch, policy.DenyLag),
		}
	}

	if policy.WarnLag > 0 && worst.Lag >= policy.WarnLag {
		limit := ""
		if policy.DenyLag > 0 {
			limit = fmt.Sprintf(" (a push is refused from %d commits behind)", policy.DenyLag)
		}
		return Decision{
			Allowed: true,
			Code:    CodeLagging,
			Warning: fmt.Sprintf(
				"the mirror %s of %s is %d commits behind the branch %s%s: publish it with \"graft mirror queue run\"",
				worst.Name, in.Repository, worst.Lag, in.Branch, limit),
		}
	}

	// A target that never published is not a mirror yet: the repository is
	// published by the others, and the operator is told which one is missing.
	for _, target := range enabled {
		if !target.Synced {
			return Decision{
				Allowed: true,
				Code:    CodeNotSynced,
				Warning: fmt.Sprintf("the mirror target %s of %s never published successfully: publish it with \"graft mirror queue run\"", target.Name, in.Repository),
			}
		}
	}

	return Decision{Allowed: true, Code: CodeAllowed}
}

// matchBranchPattern tells whether the branch matches the pattern. The
// pattern may hold "*" as a wildcard, which matches any text, including the
// empty one.
func matchBranchPattern(pattern, branch string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == branch
	}

	parts := strings.Split(pattern, "*")
	var sb strings.Builder
	sb.WriteString("^")
	for i, part := range parts {
		if i > 0 {
			sb.WriteString(".*")
		}
		sb.WriteString(regexp.QuoteMeta(part))
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	return re.MatchString(branch)
}
