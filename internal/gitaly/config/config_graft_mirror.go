package config

import (
	"fmt"

	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
)

// DefaultGraftMirrorWarnLag is the default GraftMirror.WarnLag: the number of
// commits a mirror may be behind before a push is warned about.
const DefaultGraftMirrorWarnLag = 10

// GraftMirror configures the mirror policy of the server ([graft.mirror]):
// every repository is published to a mirror, and a mirror that falls behind
// holds the pushes back (doc/graft/MIRROR.md).
//
// The policy is off by default: a server that does not configure it takes
// every push, the way that a server with no branch rule takes every update.
type GraftMirror struct {
	// Required refuses a push to a repository that has no mirror target, or
	// whose mirrors never published successfully.
	Required bool `json:"required" toml:"required,omitempty"`
	// WarnLag is the number of commits a mirror may be behind before a push
	// is warned about. Zero means DefaultGraftMirrorWarnLag, and a negative
	// value turns the warning off.
	WarnLag int `json:"warn_lag" toml:"warn_lag,omitempty"`
	// DenyLag is the number of commits a mirror may be behind before a push
	// is refused. Zero, the default, turns the refusal off: a server warns
	// first, and the operator turns the refusal on once every mirror is
	// published by a schedule. A negative value is refused.
	DenyLag int `json:"deny_lag" toml:"deny_lag,omitempty"`
}

// configured tells whether the server asked for the policy at all. A zero
// GraftMirror takes every push.
func (m GraftMirror) configured() bool {
	return m.Required || m.WarnLag > 0 || m.DenyLag > 0
}

// WarnLagOrDefault returns WarnLag, or its default. A negative value is the
// warning turned off.
func (m GraftMirror) WarnLagOrDefault() int {
	if m.WarnLag == 0 {
		return DefaultGraftMirrorWarnLag
	}
	return m.WarnLag
}

// Policy returns the policy the gate decides with. The zero GraftMirror is
// the policy turned off, not a policy that warns from the default lag: every
// value would otherwise turn the gate on, and a server that never configured
// it would hold every push of a repository without a mirror.
func (m GraftMirror) Policy() mirrorgate.Policy {
	if !m.configured() {
		return mirrorgate.Policy{}
	}

	warnLag := m.WarnLagOrDefault()
	if warnLag < 0 {
		warnLag = 0
	}

	return mirrorgate.Policy{
		Required: m.Required,
		WarnLag:  warnLag,
		DenyLag:  m.DenyLag,
	}
}

// validate reports a malformed mirror policy at load time.
func (m GraftMirror) validate() error {
	if m.DenyLag < 0 {
		return fmt.Errorf("graft mirror: deny_lag must not be negative")
	}
	if m.WarnLag < 0 && m.DenyLag > 0 {
		return fmt.Errorf("graft mirror: deny_lag needs the warning it follows, but warn_lag is negative")
	}
	if m.DenyLag > 0 && m.DenyLag <= m.WarnLagOrDefault() {
		return fmt.Errorf("graft mirror: deny_lag (%d) must be greater than warn_lag (%d), so that a push is warned about before it is refused", m.DenyLag, m.WarnLagOrDefault())
	}

	return nil
}
