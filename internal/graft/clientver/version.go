// Package clientver declares and enforces which client versions a server
// answers: the client reports its version on every request, and the server
// refuses a client older than the minimum it is configured with, with the
// command that upgrades it.
//
// The contract exists because the tools and the server are released apart: the
// release carries graft, graft-cache, graft-verify and graft-bench, while the
// server is built from source and may be newer. A client that only knows the
// RPCs of its release fails on a newer server with "unknown method", which
// says nothing about what to do; a server that names the version it needs and
// the command that installs it turns the same failure into an upgrade.
package clientver

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// Version is the version of a client build: a release (0.9.1), a pre-release
// (0.9.1-rc.2), or a build between two releases, which git describe spells as
// the release it counts away from and the number of commits (0.9.1-2-g1a2b3c4).
type Version struct {
	Major, Minor, Patch int
	// Pre is the pre-release suffix without the dash ("rc.2"), empty for a release.
	Pre string
	// Commits is how many commits the build is past the release it names.
	Commits int
}

// Parse reads the version a build reports. It accepts the shapes `graft
// version` prints: an optional "v", three numbers, and then either nothing, a
// pre-release suffix or a describe suffix. A build made without a version
// (go build from a checkout, which the release workflow does not do) reports
// nothing and is not parsed.
func Parse(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, false
	}

	core, rest, hasRest := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}

	var v Version
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return Version{}, false
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		default:
			v.Patch = n
		}
	}
	if !hasRest {
		return v, true
	}
	if rest == "" {
		// "0.9.1-": a dash with nothing after it names no build.
		return Version{}, false
	}

	// <release>-<commits>-g<sha>: the build is that many commits past the
	// release, and so newer than it.
	if commits, sha, ok := strings.Cut(rest, "-g"); ok && sha != "" {
		if n, err := strconv.Atoi(commits); err == nil && n >= 0 {
			v.Commits = n
			return v, true
		}
	}

	v.Pre = rest
	return v, true
}

// Compare orders two versions: negative when v is older than o, zero when they
// are the same, positive when v is newer. A release is newer than a pre-release
// of the same numbers, and a build past a release is newer than the release.
func (v Version) Compare(o Version) int {
	if c := cmp.Compare(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Patch, o.Patch); c != 0 {
		return c
	}

	switch {
	case v.Pre == o.Pre:
		return cmp.Compare(v.Commits, o.Commits)
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	default:
		return strings.Compare(v.Pre, o.Pre)
	}
}

// String prints the version without its describe suffix, which says how far
// past the release a build is rather than which release it is.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		return s + "-" + v.Pre
	}
	return s
}
