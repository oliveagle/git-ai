package observer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// nullOID is the object ID a reference carries when it does not exist, which
// the storage core records as a row of zeroes.
const nullOID = "0000000000000000000000000000000000000000"

// changelogMember is one member of a changelog entry: the change an agent
// wrote before the server folded it into the branch.
type changelogMember struct {
	OriginalCommit string   `json:"original_commit"`
	Commit         string   `json:"commit"`
	Actor          string   `json:"actor"`
	Rewritten      bool     `json:"rewritten"`
	ResolvedPaths  []string `json:"resolved_paths"`
}

// changelogLand is one land recorded in a changelog entry.
type changelogLand struct {
	Original string `json:"original"`
	Landed   string `json:"landed"`
}

// changelogEntry is the record the storage core keeps of what the server
// folded into a branch.
type changelogEntry struct {
	Sequence       *int64            `json:"sequence"`
	Reference      string            `json:"reference"`
	OldOID         string            `json:"old_oid"`
	NewOID         string            `json:"new_oid"`
	Operation      string            `json:"operation"`
	Actor          string            `json:"actor"`
	RecordedAt     string            `json:"recorded_at"`
	Rewritten      bool              `json:"rewritten"`
	Members        []changelogMember `json:"members"`
	Lands          []changelogLand   `json:"lands"`
	LandInvocation string            `json:"land_invocation"`
}

// refKind tells what a reference name is.
func refKind(name string) string {
	switch {
	case name == "HEAD":
		return "head"
	case strings.HasPrefix(name, "refs/heads/"):
		return "branch"
	case strings.HasPrefix(name, "refs/tags/"):
		return "tag"
	case strings.HasPrefix(name, "refs/graft/"):
		return "internal"
	default:
		return "other"
	}
}

// refShortName is the name of a reference without its namespace.
func refShortName(name string) string {
	switch {
	case name == "HEAD":
		return "HEAD"
	case strings.HasPrefix(name, "refs/heads/"):
		return strings.TrimPrefix(name, "refs/heads/")
	case strings.HasPrefix(name, "refs/tags/"):
		return strings.TrimPrefix(name, "refs/tags/")
	case strings.HasPrefix(name, "refs/graft/"):
		return strings.TrimPrefix(name, "refs/graft/")
	case strings.HasPrefix(name, "refs/"):
		return strings.TrimPrefix(name, "refs/")
	default:
		return name
	}
}

// toRefUpdates turns the reference rows of a changeset into the updates the
// clients play.
func toRefUpdates(name string, oldTarget, newTarget string) []RefUpdate {
	kind := refKind(name)
	var oldPtr, newPtr *string
	if oldTarget != "" && oldTarget != nullOID {
		oldPtr = &oldTarget
	}
	if newTarget != "" && newTarget != nullOID {
		newPtr = &newTarget
	}
	return []RefUpdate{{
		Name:      name,
		ShortName: refShortName(name),
		Kind:      kind,
		OldOID:    oldPtr,
		NewOID:    newPtr,
	}}
}

// primaryRef is the reference a changeset is about: the branch it moved, or
// the tag, or whatever it did touch.
func primaryRef(refs []RefUpdate) *RefUpdate {
	var branch, anyBranch, tag *RefUpdate
	for i := range refs {
		ref := &refs[i]
		switch {
		case ref.Kind == "branch" && !strings.HasPrefix(ref.Name, "refs/heads/graft/"):
			if branch == nil {
				branch = ref
			}
		case ref.Kind == "branch":
			if anyBranch == nil {
				anyBranch = ref
			}
		case ref.Kind == "tag" && tag == nil:
			tag = ref
		}
	}
	if branch != nil {
		return branch
	}
	if anyBranch != nil {
		return anyBranch
	}
	if tag != nil {
		return tag
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

// classification is what one operation means to a client.
type classification struct {
	kind     string
	severity string
	branch   *string
	summary  string
}

// classifyOperation turns the operation of a changeset into the event a
// client plays.
func classifyOperation(operation, actor, repo string, refs []RefUpdate, members []changelogMember) classification {
	ref := primaryRef(refs)
	var branch *string
	if ref != nil && ref.Kind != "head" {
		name := ref.ShortName
		branch = &name
	}
	who := actor
	if who == "" {
		who = "unknown"
	}
	target := ""
	if branch != nil {
		target = " on " + *branch
	}
	branchText := func(fallback string) string {
		if branch != nil {
			return *branch
		}
		return fallback
	}

	switch operation {
	case "create_repository", "repo_create":
		return classification{"repo.created", "success", branch, fmt.Sprintf("Repository %s materialized in the storage core", repo)}
	case "land_batch":
		count := len(members)
		amount := "several"
		if count > 0 {
			amount = fmt.Sprintf("%d", count)
		}
		return classification{"land.batch", "success", branch, fmt.Sprintf("%s batch-landed %s agent changes%s", who, amount, target)}
	case "land":
		if len(members) > 1 {
			return classification{"land.batch", "success", branch, fmt.Sprintf("%s folded %d changes into %s as one update", who, len(members), branchText("the branch"))}
		}
		commit := ""
		if len(members) > 0 {
			commit = members[0].Commit
		}
		if commit == "" && ref != nil && ref.NewOID != nil {
			commit = *ref.NewOID
		}
		return classification{"land", "success", branch, fmt.Sprintf("%s landed %s%s", who, shortOID(commit), target)}
	case "merge_branch", "merge":
		return classification{"merge", "success", branch, fmt.Sprintf("%s merged history into %s", who, branchText("the branch"))}
	case "ff_branch":
		newOID := ""
		if ref != nil && ref.NewOID != nil {
			newOID = *ref.NewOID
		}
		return classification{"branch.fast_forward", "info", branch, fmt.Sprintf("%s fast-forwarded %s to %s", who, branchText("a branch"), shortOID(newOID))}
	case "commit_files":
		commit := ""
		if len(members) > 0 {
			commit = members[0].Commit
		}
		if commit == "" && ref != nil && ref.NewOID != nil {
			commit = *ref.NewOID
		}
		return classification{"commit.created", "info", branch, fmt.Sprintf("%s wrote commit %s%s", who, shortOID(commit), target)}
	case "delete_branch":
		temporary := ref != nil && strings.HasPrefix(ref.Name, "refs/heads/graft/")
		short := "a temporary ref"
		if ref != nil {
			short = ref.ShortName
		}
		if temporary {
			return classification{"branch.deleted", "info", branch, fmt.Sprintf("%s released agent ref %s", who, short)}
		}
		return classification{"branch.deleted", "warn", branch, fmt.Sprintf("%s deleted branch %s", who, branchText("a branch"))}
	case "receive_pack":
		amount := fmt.Sprintf("%d", len(refs))
		if len(refs) == 0 {
			amount = "1"
		}
		plural := "s"
		if len(refs) == 1 {
			plural = ""
		}
		return classification{"push", "info", branch, fmt.Sprintf("%s pushed %s ref%s into %s", who, amount, plural, repo)}
	case "mirror", "mirror_push":
		return classification{"mirror.succeeded", "success", branch, fmt.Sprintf("%s mirrored %s to a downstream host", who, repo)}
	case "import", "seed", "shadow":
		return classification{"sync", "info", branch, fmt.Sprintf("%s synchronized %s with the storage core", who, repo)}
	case "gc":
		return classification{"gc.collected", "info", branch, fmt.Sprintf("Garbage collection ran on %s", repo)}
	}

	if len(refs) == 0 {
		name := operation
		if name == "" {
			name = "an update"
		}
		return classification{"sync", "info", nil, fmt.Sprintf("%s recorded %s in %s", who, name, repo)}
	}
	if ref != nil && ref.OldOID == nil && ref.NewOID != nil {
		if ref.Kind == "branch" {
			return classification{"branch.created", "success", branch, fmt.Sprintf("%s created branch %s", who, branchText("a branch"))}
		}
		return classification{"push", "success", branch, fmt.Sprintf("%s created %s", who, ref.ShortName)}
	}
	if ref != nil && ref.OldOID != nil && ref.NewOID == nil {
		return classification{"branch.deleted", "warn", branch, fmt.Sprintf("%s removed %s", who, ref.ShortName)}
	}
	short := "a reference"
	if ref != nil {
		short = ref.ShortName
	}
	kind := "push"
	if ref != nil && ref.Kind == "branch" {
		kind = "branch.fast_forward"
	}
	return classification{kind, "info", branch, fmt.Sprintf("%s updated %s%s", who, short, target)}
}

// changesetInput is what the storage core knows about one changeset.
type changesetInput struct {
	repoID    int64
	repo      string
	seq       int64
	operation string
	actor     string
	nodeID    *string
	packBytes int64
	forced    bool
	createdAt time.Time
	refs      []RefUpdate
}

// changesetToEvent turns one storage core changeset into a single playable
// event.
func changesetToEvent(input changesetInput, entry *changelogEntry) ActivityEvent {
	var members []changelogMember
	var lands []changelogLand
	rewritten := input.forced
	landInvocation := ""
	if entry != nil {
		members = entry.Members
		lands = entry.Lands
		rewritten = rewritten || entry.Rewritten
		landInvocation = entry.LandInvocation
	}
	class := classifyOperation(input.operation, input.actor, input.repo, input.refs, members)

	commit := ""
	if len(lands) > 0 && lands[0].Landed != "" {
		commit = lands[0].Landed
	}
	if commit == "" && len(members) > 0 && members[0].Commit != "" {
		commit = members[0].Commit
	}
	if commit == "" {
		if ref := primaryRef(input.refs); ref != nil && ref.NewOID != nil {
			commit = *ref.NewOID
		}
	}
	for _, member := range members {
		if member.Rewritten {
			rewritten = true
		}
	}

	severity := class.severity
	if input.forced && severity == "success" {
		severity = "warn"
	}

	meta := map[string]any{
		"seq":       input.seq,
		"packBytes": input.packBytes,
		"forced":    input.forced,
		"rewritten": rewritten,
	}
	if len(members) > 0 {
		items := make([]map[string]any, 0, len(members))
		for _, member := range members {
			actor := member.Actor
			if actor == "" {
				actor = input.actor
			}
			paths := member.ResolvedPaths
			if paths == nil {
				paths = []string{}
			}
			items = append(items, map[string]any{
				"original":      member.OriginalCommit,
				"commit":        member.Commit,
				"actor":         actor,
				"rewritten":     member.Rewritten,
				"resolvedPaths": paths,
			})
		}
		meta["members"] = items
	}
	if len(lands) > 0 {
		items := make([]map[string]any, 0, len(lands))
		for _, land := range lands {
			items = append(items, map[string]any{"original": land.Original, "landed": land.Landed})
		}
		meta["lands"] = items
	}
	if landInvocation != "" {
		meta["landInvocation"] = landInvocation
	}

	summary := class.summary
	if rewritten && (strings.Contains(summary, "landed") || strings.Contains(summary, "folded") || strings.Contains(summary, "merged")) {
		summary += " · history rewritten"
	}

	repo := input.repo
	repoID := input.repoID
	var actor, operation *string
	if input.actor != "" {
		value := input.actor
		actor = &value
	}
	if input.operation != "" {
		value := input.operation
		operation = &value
	}
	var commitPtr *string
	if commit != "" {
		value := commit
		commitPtr = &value
	}

	return ActivityEvent{
		ID:        fmt.Sprintf("cs:%d:%d", input.repoID, input.seq),
		At:        isoTime(input.createdAt),
		Kind:      class.kind,
		Severity:  severity,
		Repo:      &repo,
		RepoID:    &repoID,
		Actor:     actor,
		Node:      input.nodeID,
		Operation: operation,
		Branch:    class.branch,
		Commit:    commitPtr,
		Summary:   summary,
		Refs:      input.refs,
		Meta:      meta,
	}
}

// repoCreatedEvent is the event of a repository that appeared in the storage
// core.
func repoCreatedEvent(repoID int64, repo, storage string, createdAt time.Time) ActivityEvent {
	repoPtr := repo
	id := repoID
	operation := "create_repository"
	return ActivityEvent{
		ID:        fmt.Sprintf("repo:%d:created", repoID),
		At:        isoTime(createdAt),
		Kind:      "repo.created",
		Severity:  "success",
		Repo:      &repoPtr,
		RepoID:    &id,
		Operation: &operation,
		Summary:   fmt.Sprintf("Repository %s appeared in storage %s", repo, storage),
		Refs:      []RefUpdate{},
		Meta:      map[string]any{"storage": storage},
	}
}

// repoRemovedEvent is the event of a repository the storage core dropped.
func repoRemovedEvent(repoID int64, repo, storage string, removedAt time.Time) ActivityEvent {
	repoPtr := repo
	id := repoID
	operation := "remove_repository"
	return ActivityEvent{
		ID:        fmt.Sprintf("repo:%d:removed", repoID),
		At:        isoTime(removedAt),
		Kind:      "repo.removed",
		Severity:  "warn",
		Repo:      &repoPtr,
		RepoID:    &id,
		Operation: &operation,
		Summary:   fmt.Sprintf("Repository %s was removed from storage %s", repo, storage),
		Refs:      []RefUpdate{},
		Meta:      map[string]any{"storage": storage},
	}
}

// parseChangelogEntry reads the changelog entry of a changeset. A row that is
// not a JSON object is ignored: the events it carries are still shown.
func parseChangelogEntry(text string) *changelogEntry {
	if text == "" {
		return nil
	}
	var entry changelogEntry
	if err := json.Unmarshal([]byte(text), &entry); err != nil {
		// A row that is not a JSON object carries no operation; the events it
		// would have carried are still shown.
		return nil
	}
	return &entry
}

// shortOID is the short form of an object ID.
func shortOID(oid string) string {
	if oid == "" {
		return "∅"
	}
	if len(oid) <= 8 {
		return oid
	}
	return oid[:8]
}

// isoTime is the time format the clients read: RFC 3339 with milliseconds.
func isoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
