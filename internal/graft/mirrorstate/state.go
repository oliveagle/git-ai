// Package mirrorstate keeps the mirror targets of a repository and the state
// of their last synchronisation.
//
// The state belongs to the server: it says which mirrors a repository is
// published to, and how far every mirror is behind. The clients publish the
// mirrors and tell the server what they published; the server decides whether
// a push may go through (doc/graft/MIRROR.md).
//
// The state holds no credential: a target names the mirror, not the token
// that reaches it.
package mirrorstate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The statuses of a synchronisation.
const (
	// SyncOK is a mirror that holds the commits it was published with.
	SyncOK = "ok"
	// SyncDiverged is a mirror that holds history the repository does not
	// have: it was published by somebody else, or it was rewritten.
	SyncDiverged = "diverged"
	// SyncFailed is a mirror the last publication did not reach.
	SyncFailed = "failed"
)

// ErrTargetNotFound is returned when a target is not registered.
var ErrTargetNotFound = errors.New("the repository has no mirror target with that name")

// Target is one mirror of a repository: where it is published, and which
// branches may reach it.
type Target struct {
	// Name identifies the target. It is unique in a repository, and it is
	// the name the client and the operator use.
	Name string `json:"name"`
	// URL is the Git URL of the mirror: any host that speaks Git over
	// HTTPS, SSH or the scp-like form.
	URL string `json:"url"`
	// Branches are the branch patterns the mirror may publish, without a
	// refs/heads/ prefix. "*" is a wildcard.
	Branches []string `json:"branches"`
	// Enabled tells whether the target is published. A disabled target keeps
	// its history, and the gate ignores it.
	Enabled bool `json:"enabled"`
	// CreatedAt is when the target was registered.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the target was changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// Sync is the state of the last publication of a mirror.
type Sync struct {
	// Target is the name of the target this state belongs to.
	Target string `json:"target"`
	// Status is the outcome of the publication: SyncOK, SyncDiverged or
	// SyncFailed.
	Status string `json:"status"`
	// Branches are the commits the mirror holds, by branch name. A branch
	// that was not published is absent.
	Branches map[string]string `json:"branches,omitempty"`
	// PublishedAt is when the publication happened.
	PublishedAt time.Time `json:"published_at"`
	// Error is the reason of the last failure.
	Error string `json:"error,omitempty"`
}

// Key identifies the repository whose mirror state is read or written.
type Key struct {
	// Storage is the name of the storage that holds the repository.
	Storage string `json:"storage"`
	// RelativePath is the path of the repository in that storage.
	RelativePath string `json:"relative_path"`
	// Path is the git directory of the repository on this node. It is empty
	// when the state is not kept in files.
	Path string `json:"path,omitempty"`
}

// Store keeps the mirror targets of a repository and the state of their last
// synchronisation.
//
// Implementations: filestore (the state of a repository in the files of its
// git directory) and sqlstore (the state of a repository in the database of
// the storage core).
type Store interface {
	// RegisterTarget records a target of the repository. A target that is
	// registered already is replaced, and its history is kept.
	RegisterTarget(ctx context.Context, key Key, target Target) error
	// UnregisterTarget takes a target out of the repository. The state of
	// its last synchronisation is kept, so that the history of the
	// repository says what it published. ErrTargetNotFound when the
	// repository has no target with that name.
	UnregisterTarget(ctx context.Context, key Key, name string) error
	// ListTargets returns the targets of the repository, by name.
	ListTargets(ctx context.Context, key Key) ([]Target, error)
	// RecordSync records the state of the last publication of a target.
	RecordSync(ctx context.Context, key Key, sync Sync) error
	// ListSyncs returns the state of the last publication of every target
	// of the repository, by target name.
	ListSyncs(ctx context.Context, key Key) ([]Sync, error)
}

// Validate refuses a target that cannot be published.
func (t Target) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("a mirror target needs a name")
	}
	if !validName(t.Name) {
		return fmt.Errorf("the name %q of a mirror target is not a plain name", t.Name)
	}
	if strings.TrimSpace(t.URL) == "" {
		return fmt.Errorf("the mirror target %q needs a URL", t.Name)
	}
	if len(t.Branches) == 0 {
		return fmt.Errorf("the mirror target %q needs at least one branch pattern", t.Name)
	}
	for i, b := range t.Branches {
		if strings.TrimSpace(b) == "" {
			return fmt.Errorf("the branch pattern %d of the mirror target %q is empty", i+1, t.Name)
		}
	}
	return nil
}

// Validate refuses a state of a synchronisation that cannot be read.
func (s Sync) Validate() error {
	if strings.TrimSpace(s.Target) == "" {
		return errors.New("a mirror synchronisation needs the name of its target")
	}
	if !validName(s.Target) {
		return fmt.Errorf("the target %q of a mirror synchronisation is not a plain name", s.Target)
	}
	switch s.Status {
	case SyncOK, SyncDiverged, SyncFailed:
	case "":
		return fmt.Errorf("the synchronisation of the mirror target %q needs a status", s.Target)
	default:
		return fmt.Errorf("the synchronisation of the mirror target %q has the unknown status %q", s.Target, s.Status)
	}
	return nil
}

// MatchesBranch reports whether the target publishes the branch. The patterns
// are the ones of the mirror queue: "*" is a wildcard, and a pattern without
// a wildcard names one branch.
func (t Target) MatchesBranch(branch string) bool {
	for _, pattern := range t.Branches {
		if matchBranchPattern(pattern, branch) {
			return true
		}
	}
	return false
}

// Published returns the commit the mirror holds for the branch, empty when
// the mirror holds none.
func (s Sync) Published(branch string) string {
	return s.Branches[branch]
}

// Synced tells whether the mirror published successfully at least once: a
// mirror that failed, or that never ran, published nothing.
func (s Sync) Synced() bool {
	return s.Status == SyncOK || s.Status == SyncDiverged
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

// validName tells whether the name may be used as the name of a file: it
// names one target of one repository, and it is written in a path.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return !strings.ContainsAny(name, " \t\n\r")
}
