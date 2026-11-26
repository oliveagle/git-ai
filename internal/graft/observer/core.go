package observer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// historyChangesets is how many changesets the service reads to build the
	// history of the fleet.
	historyChangesets = 260
	// pollOverlap is how far before the last poll the service reads again, so a
	// changeset that landed while the database was busy is not lost.
	pollOverlap = 5 * time.Second
	// sizeRefresh is how often the pack bytes of the repositories are read
	// again: they move slowly.
	sizeRefresh = time.Minute
)

// flexTime is a timestamp of the storage core. The database keeps it as text
// in SQLite and as a timestamp with time zone in Postgres.
type flexTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (f *flexTime) Scan(value any) error {
	switch typed := value.(type) {
	case nil:
		f.Time, f.Valid = time.Time{}, false
		return nil
	case time.Time:
		f.Time, f.Valid = typed, true
		return nil
	case []byte:
		return f.parse(string(typed))
	case string:
		return f.parse(typed)
	default:
		return fmt.Errorf("observer: cannot read %T as a timestamp", value)
	}
}

// parse reads a timestamp of either dialect.
func (f *flexTime) parse(text string) error {
	if text == "" {
		f.Time, f.Valid = time.Time{}, false
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			f.Time, f.Valid = parsed, true
			return nil
		}
	}
	f.Time, f.Valid = time.Time{}, false
	return nil
}

// repoRow is one row of the repositories table.
type repoRow struct {
	ID           int64
	Storage      string
	RelativePath string
	ObjectFormat string
	HeadSeq      int64
	LogSeq       int64
	CreatedAt    time.Time
}

// refRow is one row of the references table.
type refRow struct {
	RepoID int64
	Name   string
	Target string
	Seq    int64
}

// nodeRow is one row of the nodes table.
type nodeRow struct {
	RepoID     int64
	NodeID     string
	AppliedSeq int64
	UpdatedAt  time.Time
}

// mirrorRow is one mirror target with the state of its last publication.
type mirrorRow struct {
	Repo         string
	Name         string
	URL          string
	Branches     string
	Enabled      bool
	SyncStatus   string
	PublishedAt  time.Time
	HasPublished bool
	Error        string
}

// reviewRow is one row of the reviews table.
type reviewRow struct {
	RepoID       int64
	ReviewID     int64
	Version      int64
	TargetBranch string
	Closed       bool
	UpdatedAt    time.Time
}

// checkpointRow is one row of the checkpoints table.
type checkpointRow struct {
	RepoID    int64
	Seq       int64
	PackKey   string
	PackBytes int64
	CreatedAt time.Time
}

// changesetGroup is one changeset of the storage core, with the references
// it moved.
type changesetGroup struct {
	repoID    int64
	repo      string
	storage   string
	seq       int64
	operation string
	actor     string
	nodeID    *string
	packBytes int64
	forced    bool
	createdAt time.Time
	refs      []RefUpdate
	entry     *changelogEntry
}

// collector reads the storage core and keeps the picture of the fleet.
type collector struct {
	db  *sql.DB
	hub *EventHub

	mu               sync.RWMutex
	repoRows         []repoRow
	refRows          []refRow
	nodeRows         []nodeRow
	mirrorRows       []mirrorRow
	reviewRows       []reviewRow
	checkpointRows   []checkpointRow
	recentChangesets []changesetGroup
	repoSizeBytes    map[int64]int64
	sizesAt          time.Time
	lastPollAt       time.Time
	reviewState      map[string]string
	nodeState        map[string]int64
	checkpointsSeen  map[string]struct{}
	initialized      bool
	warnings         []string
	lastError        error
	startedAt        time.Time
	lastSuccessAt    time.Time
	polling          bool
}

// newCollector returns a collector that reads the storage core.
func newCollector(db *sql.DB, hub *EventHub) *collector {
	return &collector{
		db:              db,
		hub:             hub,
		repoSizeBytes:   map[int64]int64{},
		reviewState:     map[string]string{},
		nodeState:       map[string]int64{},
		checkpointsSeen: map[string]struct{}{},
		startedAt:       time.Now(),
		lastPollAt:      time.Now().Add(-time.Minute),
	}
}

// start reads the baseline of the fleet and returns the first error that
// stops it.
func (c *collector) start(ctx context.Context) error {
	present, err := c.tableExists(ctx, "graft_repositories")
	if err != nil {
		return err
	}
	if !present {
		c.mu.Lock()
		c.warnings = append(c.warnings, "graft_repositories is missing: the storage core is off or this is the wrong database")
		c.mu.Unlock()
		return nil
	}
	if err := c.refreshBaseline(ctx, true); err != nil {
		return err
	}
	c.mu.Lock()
	c.lastSuccessAt = time.Now()
	c.initialized = true
	c.mu.Unlock()
	return nil
}

// tableExists tells whether the storage core has a table.
func (c *collector) tableExists(ctx context.Context, name string) (bool, error) {
	var found bool
	err := c.db.QueryRowContext(ctx, `select exists (select 1 from information_schema.tables where table_name = $1)`, name).Scan(&found)
	if err != nil {
		// A database that does not know information_schema is a SQLite one;
		// the tables are there.
		return true, nil
	}
	return found, nil
}

// query runs a query and returns its rows. A failure keeps the last good
// picture: a wall display must not blink empty during a restart.
func (c *collector) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		c.mu.Lock()
		c.lastError = err
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.lastError = nil
	c.mu.Unlock()
	return rows, nil
}

// loadCore reads the repositories, their references and the nodes.
func (c *collector) loadCore(ctx context.Context) error {
	repos, err := c.query(ctx, "select repo_id, storage, relative_path, object_format, head_seq, log_seq, created_at from graft_repositories order by repo_id")
	if err != nil {
		return err
	}
	var repoRows []repoRow
	for repos.Next() {
		var row repoRow
		var created flexTime
		if err := repos.Scan(&row.ID, &row.Storage, &row.RelativePath, &row.ObjectFormat, &row.HeadSeq, &row.LogSeq, &created); err != nil {
			repos.Close()
			return err
		}
		if created.Valid {
			row.CreatedAt = created.Time
		}
		repoRows = append(repoRows, row)
	}
	repos.Close()
	if err := repos.Err(); err != nil {
		return err
	}

	refs, err := c.query(ctx, "select repo_id, name, target, seq from graft_refs order by repo_id, seq")
	if err != nil {
		return err
	}
	var refRows []refRow
	for refs.Next() {
		var row refRow
		var name []byte
		if err := refs.Scan(&row.RepoID, &name, &row.Target, &row.Seq); err != nil {
			refs.Close()
			return err
		}
		row.Name = string(name)
		refRows = append(refRows, row)
	}
	refs.Close()
	if err := refs.Err(); err != nil {
		return err
	}

	nodes, err := c.query(ctx, "select repo_id, node_id, applied_seq, updated_at from graft_nodes order by repo_id, node_id")
	if err != nil {
		return err
	}
	var nodeRows []nodeRow
	for nodes.Next() {
		var row nodeRow
		var updated flexTime
		if err := nodes.Scan(&row.RepoID, &row.NodeID, &row.AppliedSeq, &updated); err != nil {
			nodes.Close()
			return err
		}
		if updated.Valid {
			row.UpdatedAt = updated.Time
		}
		nodeRows = append(nodeRows, row)
	}
	nodes.Close()
	if err := nodes.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.repoRows = repoRows
	c.refRows = refRows
	c.nodeRows = nodeRows
	c.mu.Unlock()
	return nil
}

// loadHistory reads the newest changesets of the fleet.
func (c *collector) loadHistory(ctx context.Context) error {
	rows, err := c.query(ctx, `select c.repo_id, c.seq, c.operation, c.actor, c.node_id, c.pack_bytes, c.forced, c.created_at,
       r.relative_path, r.storage,
       cr.ordinal, cr.name, cr.old_target, cr.new_target,
       g.entry
  from (select * from graft_changesets order by created_at desc limit $1) c
  join graft_repositories r on r.repo_id = c.repo_id
  left join graft_changeset_refs cr on cr.repo_id = c.repo_id and cr.seq = c.seq
  left join graft_changelog g on g.repo_id = c.repo_id and g.log_seq = c.seq
 order by c.created_at asc, cr.ordinal asc`, historyChangesets)
	if err != nil {
		return err
	}
	groups, err := scanChangesetRows(rows)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.recentChangesets = tailGroups(groups, historyChangesets)
	newest := time.Now()
	for _, group := range groups {
		if group.createdAt.After(newest) {
			newest = group.createdAt
		}
	}
	c.lastPollAt = newest.Add(-pollOverlap)
	c.mu.Unlock()
	return nil
}

// scanChangesetRows reads the rows of the changeset query and folds the rows
// of one changeset together.
func scanChangesetRows(rows *sql.Rows) ([]changesetGroup, error) {
	defer rows.Close()
	groups := map[string]*changesetGroup{}
	var order []string
	for rows.Next() {
		var (
			repoID    int64
			seq       int64
			operation string
			actor     string
			nodeID    string
			packBytes int64
			forced    bool
			created   flexTime
			relative  string
			storage   string
			ordinal   sql.NullInt64
			name      []byte
			oldTarget sql.NullString
			newTarget sql.NullString
			entry     sql.NullString
		)
		if err := rows.Scan(&repoID, &seq, &operation, &actor, &nodeID, &packBytes, &forced, &created,
			&relative, &storage, &ordinal, &name, &oldTarget, &newTarget, &entry); err != nil {
			return nil, err
		}
		if repoID == 0 || seq == 0 {
			continue
		}
		key := fmt.Sprintf("%d:%d", repoID, seq)
		group, ok := groups[key]
		if !ok {
			node := nodeID
			var nodePtr *string
			if node != "" {
				nodePtr = &node
			}
			group = &changesetGroup{
				repoID:    repoID,
				repo:      relative,
				storage:   storage,
				seq:       seq,
				operation: operation,
				actor:     actor,
				nodeID:    nodePtr,
				packBytes: packBytes,
				forced:    forced,
				createdAt: created.Time,
				refs:      []RefUpdate{},
			}
			groups[key] = group
			order = append(order, key)
		}
		if name != nil {
			group.refs = append(group.refs, toRefUpdates(string(name), oldTarget.String, newTarget.String)...)
		}
		if group.entry == nil && entry.Valid && entry.String != "" {
			group.entry = parseChangelogEntry(entry.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]changesetGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].createdAt.Equal(out[j].createdAt) {
			return out[i].createdAt.Before(out[j].createdAt)
		}
		return out[i].seq < out[j].seq
	})
	return out, nil
}

// tailGroups keeps the newest groups.
func tailGroups(groups []changesetGroup, max int) []changesetGroup {
	if len(groups) <= max {
		return groups
	}
	return groups[len(groups)-max:]
}

// loadSizes reads the pack bytes of every repository.
func (c *collector) loadSizes(ctx context.Context) error {
	rows, err := c.query(ctx, "select repo_id, coalesce(sum(pack_bytes), 0) as bytes from graft_changesets group by repo_id")
	if err != nil {
		return err
	}
	defer rows.Close()
	sizes := map[int64]int64{}
	for rows.Next() {
		var repoID, bytes int64
		if err := rows.Scan(&repoID, &bytes); err != nil {
			return err
		}
		sizes[repoID] = bytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.repoSizeBytes = sizes
	c.sizesAt = time.Now()
	c.mu.Unlock()
	return nil
}

// loadReviewsAndCheckpoints reads the reviews and the checkpoints.
func (c *collector) loadReviewsAndCheckpoints(ctx context.Context, initial bool) error {
	rows, err := c.query(ctx, "select repo_id, review_id, version, target_branch, closed, updated_at from graft_reviews order by updated_at desc limit 200")
	if err != nil {
		return err
	}
	var reviews []reviewRow
	for rows.Next() {
		var row reviewRow
		var branch []byte
		var closed int64
		var updated flexTime
		if err := rows.Scan(&row.RepoID, &row.ReviewID, &row.Version, &branch, &closed, &updated); err != nil {
			rows.Close()
			return err
		}
		row.TargetBranch = string(branch)
		row.Closed = closed != 0
		if updated.Valid {
			row.UpdatedAt = updated.Time
		}
		reviews = append(reviews, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	checkpoints, err := c.query(ctx, "select repo_id, seq, pack_key, pack_bytes, created_at from graft_checkpoints order by created_at desc limit 200")
	if err != nil {
		return err
	}
	var checkpointRows []checkpointRow
	for checkpoints.Next() {
		var row checkpointRow
		var created flexTime
		if err := checkpoints.Scan(&row.RepoID, &row.Seq, &row.PackKey, &row.PackBytes, &created); err != nil {
			checkpoints.Close()
			return err
		}
		if created.Valid {
			row.CreatedAt = created.Time
		}
		checkpointRows = append(checkpointRows, row)
	}
	checkpoints.Close()
	if err := checkpoints.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.reviewRows = reviews
	c.checkpointRows = checkpointRows
	for _, review := range reviews {
		c.reviewState[fmt.Sprintf("%d:%d", review.RepoID, review.ReviewID)] = fmt.Sprintf("%d:%t", review.Version, review.Closed)
	}
	for _, checkpoint := range checkpointRows {
		c.checkpointsSeen[fmt.Sprintf("%d:%d", checkpoint.RepoID, checkpoint.Seq)] = struct{}{}
	}
	c.mu.Unlock()

	if initial {
		cutoff := time.Now().Add(-6 * time.Hour)
		var events []ActivityEvent
		for _, review := range reviews {
			if review.UpdatedAt.Before(cutoff) {
				continue
			}
			repo, ok := c.repoByID(review.RepoID)
			if !ok {
				continue
			}
			kind := "review.updated"
			if review.Version == 1 {
				kind = "review.opened"
			}
			events = append(events, c.reviewEvent(review, repo, kind))
		}
		if len(events) > 0 {
			c.hub.PublishMany(events)
		}
	}
	return nil
}

// repoByID returns the repository of an ID.
func (c *collector) repoByID(id int64) (repoRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, repo := range c.repoRows {
		if repo.ID == id {
			return repo, true
		}
	}
	return repoRow{}, false
}

// reviewEvent is the event of a review that was opened or updated.
func (c *collector) reviewEvent(review reviewRow, repo repoRow, kind string) ActivityEvent {
	repoID := review.RepoID
	reviewID := review.ReviewID
	severity := "info"
	if review.Closed {
		severity = "success"
	}
	operation := "review_update"
	summary := fmt.Sprintf("Review #%d reached version %d", reviewID, review.Version)
	if kind == "review.opened" {
		operation = "review_open"
		branch := review.TargetBranch
		if branch == "" {
			branch = "a branch"
		}
		summary = fmt.Sprintf("Review #%d opened on %s", reviewID, branch)
	}
	if review.Closed {
		summary += " · closed"
	}
	repoPath := repo.RelativePath
	var branchPtr *string
	if review.TargetBranch != "" {
		branch := review.TargetBranch
		branchPtr = &branch
	}
	return ActivityEvent{
		ID:        fmt.Sprintf("review:%d:%d:%d:%s", review.RepoID, review.ReviewID, review.Version, kind),
		At:        isoTime(review.UpdatedAt),
		Kind:      kind,
		Severity:  severity,
		Repo:      &repoPath,
		RepoID:    &repoID,
		Operation: &operation,
		Branch:    branchPtr,
		Summary:   summary,
		Refs:      []RefUpdate{},
		Meta: map[string]any{
			"reviewId": review.ReviewID,
			"version":  review.Version,
			"closed":   review.Closed,
		},
	}
}

// loadMirrors reads the mirror targets and the state of their last
// publication.
func (c *collector) loadMirrors(ctx context.Context) error {
	rows, err := c.query(ctx, `select t.relative_path as repo, t.name, t.url, t.branches, t.enabled,
       s.status as sync_status, s.published_at, s.error
  from graft_mirror_targets t
  left join graft_mirror_syncs s
    on s.storage = t.storage and s.relative_path = t.relative_path and s.name = t.name
 order by t.relative_path, t.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var mirrors []mirrorRow
	for rows.Next() {
		var row mirrorRow
		var published sql.NullTime
		var enabled bool
		if err := rows.Scan(&row.Repo, &row.Name, &row.URL, &row.Branches, &enabled, &row.SyncStatus, &published, &row.Error); err != nil {
			return err
		}
		row.Enabled = enabled
		if published.Valid {
			row.PublishedAt = published.Time
			row.HasPublished = true
		}
		mirrors = append(mirrors, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.mirrorRows = mirrors
	c.mu.Unlock()
	return nil
}

// refreshBaseline reads the whole fleet and publishes the events of its
// history.
func (c *collector) refreshBaseline(ctx context.Context, initial bool) error {
	if err := c.loadCore(ctx); err != nil {
		return err
	}
	if err := c.loadHistory(ctx); err != nil {
		return err
	}
	if err := c.loadSizes(ctx); err != nil {
		return err
	}
	if err := c.loadReviewsAndCheckpoints(ctx, initial); err != nil {
		return err
	}
	if err := c.loadMirrors(ctx); err != nil {
		return err
	}
	if !initial {
		return nil
	}

	c.mu.RLock()
	repos := append([]repoRow(nil), c.repoRows...)
	groups := append([]changesetGroup(nil), c.recentChangesets...)
	checkpoints := append([]checkpointRow(nil), c.checkpointRows...)
	c.mu.RUnlock()

	var events []ActivityEvent
	for _, repo := range repos {
		events = append(events, repoCreatedEvent(repo.ID, repo.RelativePath, repo.Storage, repo.CreatedAt))
	}
	for _, group := range groups {
		events = append(events, changesetToEvent(changesetInput{
			repoID:    group.repoID,
			repo:      group.repo,
			seq:       group.seq,
			operation: group.operation,
			actor:     group.actor,
			nodeID:    group.nodeID,
			packBytes: group.packBytes,
			forced:    group.forced,
			createdAt: group.createdAt,
			refs:      group.refs,
		}, group.entry))
	}

	removed, err := c.query(ctx, "select repo_id, storage, relative_path, removed_at from graft_gc_removed order by removed_at asc limit 80")
	if err != nil {
		return err
	}
	for removed.Next() {
		var repoID int64
		var storage, relative string
		var at flexTime
		if err := removed.Scan(&repoID, &storage, &relative, &at); err != nil {
			removed.Close()
			return err
		}
		events = append(events, repoRemovedEvent(repoID, relative, storage, at.Time))
	}
	removed.Close()
	if err := removed.Err(); err != nil {
		return err
	}

	for _, checkpoint := range checkpoints {
		repo, ok := c.repoByID(checkpoint.RepoID)
		if !ok {
			continue
		}
		events = append(events, c.checkpointEvent(checkpoint, repo))
	}
	sortEvents(events)
	if len(events) > 0 {
		c.hub.PublishMany(events)
	}

	c.mu.Lock()
	for _, review := range c.reviewRows {
		c.reviewState[fmt.Sprintf("%d:%d", review.RepoID, review.ReviewID)] = fmt.Sprintf("%d:%t", review.Version, review.Closed)
	}
	for _, node := range c.nodeRows {
		c.nodeState[fmt.Sprintf("%d:%s", node.RepoID, node.NodeID)] = node.AppliedSeq
	}
	c.mu.Unlock()
	return nil
}

// sortEvents orders the events by their time, then by their id.
func sortEvents(events []ActivityEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At != events[j].At {
			return events[i].At < events[j].At
		}
		return events[i].ID < events[j].ID
	})
}

// checkpointEvent is the event of a checkpoint that sealed a pack.
func (c *collector) checkpointEvent(checkpoint checkpointRow, repo repoRow) ActivityEvent {
	repoID := checkpoint.RepoID
	repoPath := repo.RelativePath
	operation := "checkpoint"
	return ActivityEvent{
		ID:        fmt.Sprintf("checkpoint:%d:%d", checkpoint.RepoID, checkpoint.Seq),
		At:        isoTime(checkpoint.CreatedAt),
		Kind:      "checkpoint.recorded",
		Severity:  "success",
		Repo:      &repoPath,
		RepoID:    &repoID,
		Operation: &operation,
		Summary:   fmt.Sprintf("Checkpoint %d sealed %d bytes for %s", checkpoint.Seq, checkpoint.PackBytes, repo.RelativePath),
		Refs:      []RefUpdate{},
		Meta: map[string]any{
			"seq":       checkpoint.Seq,
			"packBytes": checkpoint.PackBytes,
			"packKey":   checkpoint.PackKey,
		},
	}
}

// poll reads what changed since the last poll and publishes the events of
// the change.
func (c *collector) poll(ctx context.Context) error {
	c.mu.Lock()
	if c.polling || !c.initialized {
		c.mu.Unlock()
		return nil
	}
	c.polling = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.polling = false
		c.mu.Unlock()
	}()

	var events []ActivityEvent

	c.mu.RLock()
	previousRepoIDs := map[int64]struct{}{}
	for _, repo := range c.repoRows {
		previousRepoIDs[repo.ID] = struct{}{}
	}
	since := c.lastPollAt.Add(-pollOverlap)
	c.mu.RUnlock()

	if err := c.loadCore(ctx); err != nil {
		return err
	}
	if err := c.loadMirrors(ctx); err != nil {
		return err
	}

	c.mu.RLock()
	sizesDue := time.Since(c.sizesAt) > sizeRefresh
	c.mu.RUnlock()
	if sizesDue {
		if err := c.loadSizes(ctx); err != nil {
			return err
		}
	}

	c.mu.RLock()
	repos := append([]repoRow(nil), c.repoRows...)
	c.mu.RUnlock()
	for _, repo := range repos {
		if _, ok := previousRepoIDs[repo.ID]; ok {
			continue
		}
		events = append(events, repoCreatedEvent(repo.ID, repo.RelativePath, repo.Storage, repo.CreatedAt))
	}

	rows, err := c.query(ctx, `select c.repo_id, c.seq, c.operation, c.actor, c.node_id, c.pack_bytes, c.forced, c.created_at,
       r.relative_path, r.storage,
       cr.ordinal, cr.name, cr.old_target, cr.new_target,
       g.entry
  from graft_changesets c
  join graft_repositories r on r.repo_id = c.repo_id
  left join graft_changeset_refs cr on cr.repo_id = c.repo_id and cr.seq = c.seq
  left join graft_changelog g on g.repo_id = c.repo_id and g.log_seq = c.seq
 where c.created_at >= $1
 order by c.created_at asc, cr.ordinal asc
 limit 600`, since)
	if err != nil {
		return err
	}
	groups, err := scanChangesetRows(rows)
	if err != nil {
		return err
	}
	for _, group := range groups {
		events = append(events, changesetToEvent(changesetInput{
			repoID:    group.repoID,
			repo:      group.repo,
			seq:       group.seq,
			operation: group.operation,
			actor:     group.actor,
			nodeID:    group.nodeID,
			packBytes: group.packBytes,
			forced:    group.forced,
			createdAt: group.createdAt,
			refs:      group.refs,
		}, group.entry))
	}
	c.mu.Lock()
	c.recentChangesets = tailGroups(append(c.recentChangesets, groups...), 400)
	c.mu.Unlock()

	if err := c.loadReviewsAndCheckpoints(ctx, false); err != nil {
		return err
	}
	c.mu.RLock()
	reviews := append([]reviewRow(nil), c.reviewRows...)
	checkpoints := append([]checkpointRow(nil), c.checkpointRows...)
	c.mu.RUnlock()
	for _, review := range reviews {
		key := fmt.Sprintf("%d:%d", review.RepoID, review.ReviewID)
		signature := fmt.Sprintf("%d:%t", review.Version, review.Closed)
		c.mu.Lock()
		previous, known := c.reviewState[key]
		c.mu.Unlock()
		if known && previous == signature {
			continue
		}
		repo, ok := c.repoByID(review.RepoID)
		if !ok {
			continue
		}
		kind := "review.updated"
		if !known {
			kind = "review.opened"
		}
		c.mu.Lock()
		c.reviewState[key] = signature
		c.mu.Unlock()
		events = append(events, c.reviewEvent(review, repo, kind))
	}
	for _, checkpoint := range checkpoints {
		key := fmt.Sprintf("%d:%d", checkpoint.RepoID, checkpoint.Seq)
		c.mu.Lock()
		_, seen := c.checkpointsSeen[key]
		if !seen {
			c.checkpointsSeen[key] = struct{}{}
		}
		c.mu.Unlock()
		if seen {
			continue
		}
		repo, ok := c.repoByID(checkpoint.RepoID)
		if !ok {
			continue
		}
		events = append(events, c.checkpointEvent(checkpoint, repo))
	}

	nodes, err := c.query(ctx, "select repo_id, node_id, applied_seq, updated_at from graft_nodes order by repo_id, node_id")
	if err != nil {
		return err
	}
	var nodeRows []nodeRow
	for nodes.Next() {
		var row nodeRow
		var updated flexTime
		if err := nodes.Scan(&row.RepoID, &row.NodeID, &row.AppliedSeq, &updated); err != nil {
			nodes.Close()
			return err
		}
		if updated.Valid {
			row.UpdatedAt = updated.Time
		}
		nodeRows = append(nodeRows, row)
	}
	nodes.Close()
	if err := nodes.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.nodeRows = nodeRows
	c.mu.Unlock()
	for _, node := range nodeRows {
		key := fmt.Sprintf("%d:%s", node.RepoID, node.NodeID)
		c.mu.Lock()
		previous, known := c.nodeState[key]
		c.nodeState[key] = node.AppliedSeq
		c.mu.Unlock()
		if !known || node.AppliedSeq <= previous {
			continue
		}
		repo, ok := c.repoByID(node.RepoID)
		if !ok {
			continue
		}
		repoPath := repo.RelativePath
		nodeID := node.NodeID
		operation := "sync"
		events = append(events, ActivityEvent{
			ID:        fmt.Sprintf("node:%s:%d", key, node.AppliedSeq),
			At:        isoTime(node.UpdatedAt),
			Kind:      "node.synced",
			Severity:  "success",
			Repo:      &repoPath,
			RepoID:    &node.RepoID,
			Node:      &nodeID,
			Operation: &operation,
			Summary:   fmt.Sprintf("%s caught up to sequence %d on %s", node.NodeID, node.AppliedSeq, repo.RelativePath),
			Refs:      []RefUpdate{},
			Meta: map[string]any{
				"appliedSeq":  node.AppliedSeq,
				"previousSeq": previous,
			},
		})
	}

	removed, err := c.query(ctx, "select repo_id, storage, relative_path, removed_at from graft_gc_removed order by removed_at desc limit 80")
	if err != nil {
		return err
	}
	for removed.Next() {
		var repoID int64
		var storage, relative string
		var at flexTime
		if err := removed.Scan(&repoID, &storage, &relative, &at); err != nil {
			removed.Close()
			return err
		}
		id := fmt.Sprintf("repo:%d:removed", repoID)
		if c.hubHasEvent(id) {
			continue
		}
		events = append(events, repoRemovedEvent(repoID, relative, storage, at.Time))
	}
	removed.Close()
	if err := removed.Err(); err != nil {
		return err
	}

	// Keep the repo-created timestamps in the timeline even when the clock of
	// the database is ahead of the clock of this host.
	newest := time.Now()
	c.mu.RLock()
	lastPoll := c.lastPollAt
	c.mu.RUnlock()
	if lastPoll.After(newest) {
		newest = lastPoll
	}
	for _, event := range events {
		if at, err := time.Parse("2006-01-02T15:04:05.000Z07:00", event.At); err == nil && at.After(newest) {
			newest = at
		}
	}
	c.mu.Lock()
	c.lastPollAt = newest.Add(-pollOverlap)
	c.mu.Unlock()

	if len(events) > 0 {
		c.hub.PublishMany(events)
	}
	c.mu.Lock()
	c.lastSuccessAt = time.Now()
	c.mu.Unlock()
	return nil
}

// hubHasEvent tells whether the hub already knows an event.
func (c *collector) hubHasEvent(id string) bool {
	for _, event := range c.hub.All() {
		if event.ID == id {
			return true
		}
	}
	return false
}

// snapshot builds the picture of the fleet.
func (c *collector) snapshot(source SourceInfo) Snapshot {
	c.mu.RLock()
	repos := append([]repoRow(nil), c.repoRows...)
	refs := append([]refRow(nil), c.refRows...)
	nodes := append([]nodeRow(nil), c.nodeRows...)
	mirrors := append([]mirrorRow(nil), c.mirrorRows...)
	groups := append([]changesetGroup(nil), c.recentChangesets...)
	sizes := make(map[int64]int64, len(c.repoSizeBytes))
	for key, value := range c.repoSizeBytes {
		sizes[key] = value
	}
	c.mu.RUnlock()

	now := time.Now()
	summaries := make([]RepoSummary, 0, len(repos))
	for _, repo := range repos {
		repoRefs := make([]RepoRef, 0)
		for _, ref := range refs {
			if ref.RepoID != repo.ID {
				continue
			}
			repoRefs = append(repoRefs, RepoRef{
				Name:      ref.Name,
				ShortName: refShortName(ref.Name),
				Target:    ref.Target,
				Kind:      refKind(ref.Name),
				Seq:       ref.Seq,
			})
		}
		branches := []RepoRef{}
		tags := []RepoRef{}
		internal := []RepoRef{}
		for _, ref := range repoRefs {
			switch ref.Kind {
			case "branch":
				branches = append(branches, ref)
			case "tag":
				tags = append(tags, ref)
			case "internal":
				internal = append(internal, ref)
			}
		}

		actors := actorsFor(groups, repo.ID)
		var lastActivity *string
		for _, group := range groups {
			if group.repoID != repo.ID {
				continue
			}
			at := isoTime(group.createdAt)
			lastActivity = &at
		}

		age := time.Duration(1<<62 - 1)
		if lastActivity != nil {
			if at, err := time.Parse("2006-01-02T15:04:05.000Z07:00", *lastActivity); err == nil {
				age = now.Sub(at)
			}
		}
		health := "stale"
		switch {
		case age < 2*time.Minute:
			health = "active"
		case age < 30*time.Minute:
			health = "idle"
		}

		summaries = append(summaries, RepoSummary{
			ID:             repo.ID,
			Storage:        repo.Storage,
			Path:           repo.RelativePath,
			ObjectFormat:   repo.ObjectFormat,
			CreatedAt:      isoTime(repo.CreatedAt),
			HeadSeq:        repo.HeadSeq,
			LogSeq:         repo.LogSeq,
			Changesets:     repo.HeadSeq,
			SizeBytes:      sizes[repo.ID],
			LastActivityAt: lastActivity,
			Actors:         actors,
			Branches:       branches,
			Tags:           tags,
			InternalRefs:   internal,
			Activity:       c.activityFor(repo.ID, now),
			Health:         health,
		})
	}

	nodeSummaries := buildNodes(repos, nodes, now)
	mirrorSummaries := buildMirrors(c.hub, mirrors)
	events := c.hub.Recent(320)
	stats := buildStats(summaries, nodeSummaries, mirrorSummaries, c.hub, groups, now)

	return Snapshot{
		Source:      source,
		GeneratedAt: isoTime(now),
		Cursor:      c.hub.Cursor(),
		Repos:       summaries,
		Nodes:       nodeSummaries,
		Mirrors:     mirrorSummaries,
		Events:      events,
		Stats:       stats,
	}
}

// actorsFor counts what every actor changed in a repository.
func actorsFor(groups []changesetGroup, repoID int64) []RepoActor {
	type entry struct {
		count  int64
		lastAt string
	}
	counts := map[string]*entry{}
	for _, group := range groups {
		if group.repoID != repoID || group.actor == "" {
			continue
		}
		item, ok := counts[group.actor]
		if !ok {
			item = &entry{}
			counts[group.actor] = item
		}
		item.count++
		at := isoTime(group.createdAt)
		if item.lastAt == "" || at > item.lastAt {
			item.lastAt = at
		}
	}
	out := make([]RepoActor, 0, len(counts))
	for name, item := range counts {
		actor := RepoActor{Name: name, Count: item.count}
		if item.lastAt != "" {
			last := item.lastAt
			actor.LastAt = &last
		}
		out = append(out, actor)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// activityFor counts the events of a repository in 32 buckets of 30 seconds.
func (c *collector) activityFor(repoID int64, now time.Time) []int {
	buckets := make([]int, 32)
	span := 32 * 30 * time.Second
	for _, event := range c.hub.All() {
		if event.RepoID == nil || *event.RepoID != repoID {
			continue
		}
		at, err := time.Parse("2006-01-02T15:04:05.000Z07:00", event.At)
		if err != nil {
			continue
		}
		age := now.Sub(at)
		if age > span || age < 0 {
			continue
		}
		index := 31 - int(age/30*time.Second)
		if index < 0 {
			index = 0
		}
		if index > 31 {
			index = 31
		}
		buckets[index]++
	}
	return buckets
}

// buildNodes folds the node rows into one summary per machine.
func buildNodes(repos []repoRow, nodes []nodeRow, now time.Time) []NodeSummary {
	headByRepo := map[int64]int64{}
	for _, repo := range repos {
		headByRepo[repo.ID] = repo.HeadSeq
	}
	type entry struct {
		repos     map[int64]struct{}
		applied   int64
		head      int64
		lag       int64
		updatedAt time.Time
	}
	byNode := map[string]*entry{}
	for _, node := range nodes {
		item, ok := byNode[node.NodeID]
		if !ok {
			item = &entry{repos: map[int64]struct{}{}}
			byNode[node.NodeID] = item
		}
		item.repos[node.RepoID] = struct{}{}
		head := headByRepo[node.RepoID]
		if node.AppliedSeq > item.applied {
			item.applied = node.AppliedSeq
		}
		if head > item.head {
			item.head = head
		}
		if lag := head - node.AppliedSeq; lag > item.lag {
			item.lag = lag
		}
		if node.UpdatedAt.After(item.updatedAt) {
			item.updatedAt = node.UpdatedAt
		}
	}
	out := make([]NodeSummary, 0, len(byNode))
	for id, item := range byNode {
		summary := NodeSummary{
			ID:         id,
			Repos:      len(item.repos),
			AppliedSeq: item.applied,
			MaxHeadSeq: item.head,
			Lag:        item.lag,
		}
		if !item.updatedAt.IsZero() {
			updated := isoTime(item.updatedAt)
			summary.UpdatedAt = &updated
		}
		switch {
		case item.updatedAt.IsZero() || now.Sub(item.updatedAt) >= 15*time.Minute:
			summary.Status = "idle"
		case item.lag > 5:
			summary.Status = "lagging"
		default:
			summary.Status = "live"
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildMirrors folds the mirror targets of the storage core and the mirror
// events into one list.
func buildMirrors(hub *EventHub, rows []mirrorRow) []MirrorTarget {
	targets := map[string]MirrorTarget{}
	for _, event := range hub.All() {
		url, ok := event.Meta["mirrorUrl"].(string)
		if !ok || url == "" {
			continue
		}
		repo := ""
		if event.Repo != nil {
			repo = *event.Repo
		}
		host := remoteHost(url)
		if value, ok := event.Meta["mirrorHost"].(string); ok && value != "" {
			host = value
		}
		key := repo + ":" + url
		previous, exists := targets[key]
		branches := []string{}
		if exists {
			branches = previous.Branches
		}
		if list, ok := event.Meta["branches"].([]any); ok {
			branches = branches[:0]
			for _, item := range list {
				if text, ok := item.(string); ok {
					branches = append(branches, text)
				}
			}
		}
		status := "idle"
		if exists {
			status = previous.Status
		}
		switch event.Kind {
		case "mirror.started":
			status = "syncing"
		case "mirror.succeeded":
			status = "ok"
		case "mirror.diverged":
			status = "diverged"
		case "mirror.failed":
			status = "error"
		}
		at := event.At
		lastEvent := event.ID
		detail := event.Summary
		targets[key] = MirrorTarget{
			ID:          key,
			Repo:        repo,
			URL:         url,
			Host:        host,
			Branches:    branches,
			Status:      status,
			LastPushAt:  &at,
			LastEventID: &lastEvent,
			Detail:      &detail,
		}
	}
	// The storage core tables are authoritative: one row per registered
	// target with its last publication. The events only add what the tables
	// do not know.
	for _, row := range rows {
		if row.URL == "" {
			continue
		}
		key := row.Repo + ":" + row.URL
		branches := []string{}
		if err := json.Unmarshal([]byte(row.Branches), &branches); err != nil || branches == nil {
			branches = []string{}
		}
		status := "idle"
		switch {
		case !row.Enabled:
			status = "idle"
		case row.SyncStatus == "ok":
			status = "ok"
		case row.SyncStatus == "failed":
			status = "error"
		}
		target := MirrorTarget{
			ID:       key,
			Repo:     row.Repo,
			URL:      row.URL,
			Host:     remoteHost(row.URL),
			Branches: branches,
			Status:   status,
		}
		if row.HasPublished {
			published := isoTime(row.PublishedAt)
			target.LastPushAt = &published
		}
		if row.Error != "" {
			detail := row.Error
			if len(detail) > 240 {
				detail = detail[:240]
			}
			target.Detail = &detail
		}
		targets[key] = target
	}
	out := make([]MirrorTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, target)
	}
	sort.SliceStable(out, func(i, j int) bool {
		left, right := "", ""
		if out[i].LastPushAt != nil {
			left = *out[i].LastPushAt
		}
		if out[j].LastPushAt != nil {
			right = *out[j].LastPushAt
		}
		return left > right
	})
	return out
}

// buildStats counts what the top bar shows.
func buildStats(repos []RepoSummary, nodes []NodeSummary, mirrors []MirrorTarget, hub *EventHub, groups []changesetGroup, now time.Time) UniverseStats {
	events := hub.All()
	recent := 0
	lands := 0
	var lastEventAt *string
	for _, event := range events {
		at, err := time.Parse("2006-01-02T15:04:05.000Z07:00", event.At)
		if err != nil {
			continue
		}
		if now.Sub(at) <= 60*time.Second {
			recent++
		}
		if now.Sub(at) <= 5*time.Minute {
			switch event.Kind {
			case "land", "land.batch", "merge":
				lands++
			}
		}
	}
	if len(events) > 0 {
		last := events[len(events)-1].At
		lastEventAt = &last
	}
	actors := map[string]struct{}{}
	branches := 0
	var changesets int64
	for _, repo := range repos {
		branches += len(repo.Branches)
		changesets += repo.Changesets
		for _, actor := range repo.Actors {
			actors[actor.Name] = struct{}{}
		}
	}
	var bytes int64
	for _, group := range groups {
		bytes += group.packBytes
	}
	healthy := 0
	for _, mirror := range mirrors {
		if mirror.Status == "ok" {
			healthy++
		}
	}
	return UniverseStats{
		Repos:           len(repos),
		Branches:        branches,
		Changesets:      changesets,
		Actors:          len(actors),
		Nodes:           len(nodes),
		EventsPerMinute: recent,
		LandsPerMinute:  float64(int(float64(lands)*10+0.5)) / 10,
		MirrorTargets:   len(mirrors),
		MirrorsHealthy:  healthy,
		LastEventAt:     lastEventAt,
		BytesWritten:    bytes,
	}
}

// remoteHost is the host name of a Git remote, for the labels and the mirror
// keys.
func remoteHost(url string) string {
	value := strings.TrimSpace(url)
	if value == "" {
		return "unknown"
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") ||
		strings.HasPrefix(value, "file:") || strings.HasPrefix(value, "~/") {
		return "local"
	}
	if index := strings.Index(url, "://"); index > 0 {
		rest := value[index+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		// The host of a URL keeps its port: two mirrors on the same host but
		// on different ports are two different destinations.
		if rest != "" {
			return strings.ToLower(rest)
		}
		return "unknown"
	}
	// The scp-like shorthand: [user@]host:path.
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	if colon := strings.Index(value, ":"); colon > 0 && !strings.HasPrefix(value[colon:], "//") {
		return value[:colon]
	}
	if slash := strings.Index(value, "/"); slash > 0 {
		return value[:slash]
	}
	return value
}
