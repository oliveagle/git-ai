// Package mirrorsweep publishes the targets of a mirror queue.
//
// The queue of a client is a file of targets: the repositories that are
// published, the Git URLs they reach, the branches that may travel, and the
// schedule of every target. This package is the worker of that queue: it
// reads the changeset logs that say what changed, it decides which targets
// are due, it publishes them, and it records what every publication did.
//
// The package holds no secret. A target names the environment variable that
// carries its token, and the value is read when the task runs.
//
// One worker, two callers. The `graft mirror queue` commands of a client run
// it by hand, and the Temporal job of a deployment runs it on a schedule.
// There is a single implementation, so the two cannot drift apart.
package mirrorsweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/sdk"
)

const (
	// DefaultConcurrency is how many publications of one run happen at the
	// same time.
	DefaultConcurrency = 4
	// DefaultTaskTimeout bounds one publication, so that a mirror that hangs
	// cannot hold the run — and with it the schedule of every other target —
	// for as long as its own client keeps waiting.
	DefaultTaskTimeout = 2 * time.Minute
	// changesetPage is how many changesets one read of a log asks for. A
	// page that comes back full is read again from its last sequence.
	changesetPage = 500
	// changesetPages bounds the pages one read follows, so that a repository
	// that is far behind is caught up over several runs instead of holding
	// the run.
	changesetPages = 40
)

// Config is how a worker reaches the storage core and publishes.
type Config struct {
	// Address is the server the queue publishes from.
	Address string
	// Token is the shared token of the operator.
	Token string
	// Storage is the storage the repositories live in.
	Storage string
	// User is the acting user of a write.
	User string
}

// Options are the choices of one run.
type Options struct {
	// Target publishes one named target, whatever its schedule says.
	Target string
	// All publishes every enabled target, not only the ones that are due.
	All bool
	// Concurrency is how many publications run at the same time.
	Concurrency int
	// TaskTimeout bounds one publication. Zero waits forever.
	TaskTimeout time.Duration
	// NoEvents keeps the run from reading the changeset logs, so that the
	// interval is the only trigger.
	NoEvents bool
}

// Result is what a run did.
type Result struct {
	// Tasks are the publications of the run, one per target that was
	// published.
	Tasks []mirrorqueue.Task
	// Due is how many targets the run chose to publish.
	Due int
	// Targets is how many targets the queue holds.
	Targets int
	// Quiet is how many repositories the changeset logs said were quiet.
	Quiet int
}

// Runner publishes the targets of a queue.
type Runner struct {
	// Store keeps the queue and the history of its tasks.
	Store *mirrorqueue.FileStore
	// Config reaches the storage core.
	Config Config
	// Now is the clock of the runner. A nil clock is the clock of the
	// machine.
	Now func() time.Time
}

// now returns the time of the runner.
func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Sweep publishes the targets that are due and returns what every
// publication did.
//
// The changeset logs are the events of the queue: a repository whose log moved
// past its cursor is published right away, and the interval stays as the
// fallback. A run that cannot read a log keeps the schedule it had.
func (r *Runner) Sweep(ctx context.Context, opts Options) (Result, error) {
	queue, err := r.Store.Load()
	if err != nil {
		return Result{}, err
	}
	result := Result{Targets: len(queue.Targets)}

	changes := map[string]change{}
	if !opts.NoEvents {
		changes = r.observe(ctx, queue)
		result.Quiet = len(changes)
		if len(changes) > 0 {
			if err := r.recordChanges(changes); err != nil {
				return Result{}, err
			}
			if queue, err = r.Store.Load(); err != nil {
				return Result{}, err
			}
			result.Targets = len(queue.Targets)
		}
	}

	targets, err := r.selection(queue, opts)
	if err != nil {
		return Result{}, err
	}
	result.Due = len(targets)
	if len(targets) == 0 {
		return result, nil
	}
	result.Tasks = r.publish(ctx, opts, targets, changes)
	return result, nil
}

// Publish publishes one target of the queue, whatever its schedule says, and
// returns the task of the publication.
//
// A target that is named is published even when its schedule says it is not
// due: a person asked for it, or a job that owns the target did. The
// changeset logs are read as they are in a sweep, so that the run keeps the
// cursor of every repository up to date.
func (r *Runner) Publish(ctx context.Context, name string) (mirrorqueue.Task, error) {
	queue, err := r.Store.Load()
	if err != nil {
		return mirrorqueue.Task{}, err
	}
	target, ok := queue.Target(name)
	if !ok {
		return mirrorqueue.Task{}, fmt.Errorf("the mirror queue has no target named %q", name)
	}
	changes := map[string]change{}
	if err := r.observeAndRecord(ctx, queue, changes); err != nil {
		return mirrorqueue.Task{}, err
	}
	return r.publishTarget(ctx, target, changes[r.repoKey(target)])
}

// observeAndRecord reads the changeset logs of the queue and stores what they
// said, so that a run that publishes one target still keeps the cursor of
// every repository up to date.
func (r *Runner) observeAndRecord(ctx context.Context, queue *mirrorqueue.Queue, changes map[string]change) error {
	observed := r.observe(ctx, queue)
	if len(observed) == 0 {
		return nil
	}
	if err := r.recordChanges(observed); err != nil {
		return err
	}
	for key, c := range observed {
		changes[key] = c
	}
	return nil
}

// selection returns the targets of a run: the ones that are due, or the one
// the caller named, or every enabled target.
func (r *Runner) selection(queue *mirrorqueue.Queue, o Options) ([]*mirrorqueue.Target, error) {
	if o.Target != "" {
		target, ok := queue.Target(o.Target)
		if !ok {
			return nil, fmt.Errorf("the mirror queue has no target named %q", o.Target)
		}
		return []*mirrorqueue.Target{target}, nil
	}
	if o.All {
		var targets []*mirrorqueue.Target
		for _, t := range queue.Targets {
			if t.Enabled {
				targets = append(targets, t)
			}
		}
		return targets, nil
	}
	return queue.Due(r.now()), nil
}

// publish publishes the targets and records a task for each of them. The
// tasks run at the same time, at most as many as the concurrency allows.
func (r *Runner) publish(ctx context.Context, o Options, targets []*mirrorqueue.Target, changes map[string]change) []mirrorqueue.Task {
	concurrency := o.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency > len(targets) {
		concurrency = len(targets)
	}

	var (
		mu    sync.Mutex
		tasks []mirrorqueue.Task
		wg    sync.WaitGroup
	)
	slots := make(chan struct{}, concurrency)
	for _, target := range targets {
		wg.Add(1)
		go func(target *mirrorqueue.Target) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			taskCtx := ctx
			if o.TaskTimeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, o.TaskTimeout)
				defer cancel()
			}
			task, err := r.publishTarget(taskCtx, target, changes[r.repoKey(target)])
			if err != nil {
				// The outcome of the run could not be written to the queue.
				// The task says so, and the run counts as a failure: the
				// target keeps the state it had, and the next run tries
				// again.
				task.Status = mirrorqueue.TaskFailed
				task.Error = err.Error()
				task.FinishedAt = r.now()
			}
			mu.Lock()
			tasks = append(tasks, task)
			mu.Unlock()
		}(target)
	}
	wg.Wait()
	return tasks
}

// publishTarget publishes one target and records the task of the run in the
// history of the queue.
func (r *Runner) publishTarget(ctx context.Context, target *mirrorqueue.Target, c change) (mirrorqueue.Task, error) {
	now := r.now()
	task := &mirrorqueue.Task{
		ID:         TaskID(),
		Target:     target.Name,
		Repository: target.Repository,
		URL:        target.URL,
		Status:     mirrorqueue.TaskRunning,
		CreatedAt:  now,
		StartedAt:  now,
		Changed:    c.changed,
	}
	// The first record says that the run started. A queue that cannot be
	// written is not a reason to skip the publication: the second record
	// carries the outcome, and it is the one that has to be written.
	_ = r.record(target, task, c.observed)

	divergent, err := r.push(ctx, target)
	finished := r.now()
	task.FinishedAt = finished
	switch {
	case err != nil:
		task.Status = mirrorqueue.TaskFailed
		task.Error = err.Error()
	case len(divergent) > 0:
		task.Status = mirrorqueue.TaskDiverged
		task.DivergentRefs = divergent
	default:
		task.Status = mirrorqueue.TaskOK
	}
	// The outcome has to reach the queue: a target whose last run is not
	// recorded stays running for ever, and the mirror of it is never
	// published again. A queue that cannot be written is therefore an error
	// of the run, and the run is tried again.
	if err := r.record(target, task, c.observed); err != nil {
		return *task, err
	}

	return *task, nil
}

// record writes the task of a run into the history of the queue, and the
// outcome of the run into its target. Observed says whether the changeset log
// of the repository was read: a run that could not read it does not move the
// quiet count, because the schedule then knows nothing new.
func (r *Runner) record(target *mirrorqueue.Target, task *mirrorqueue.Task, observed bool) error {
	return r.Store.Update(func(queue *mirrorqueue.Queue) error {
		if existing, ok := queue.Task(task.ID); ok {
			*existing = *task
		} else {
			queue.AddTask(task)
		}
		current, ok := queue.Target(target.Name)
		if !ok {
			return nil
		}
		current.LastTask = task.ID
		current.LastStatus = task.Status
		current.LastError = task.Error
		current.UpdatedAt = task.FinishedAt
		if !task.FinishedAt.IsZero() {
			switch task.Status {
			case mirrorqueue.TaskOK:
				current.Observe(task.Changed, observed)
				current.ResetFailures()
				current.SetNextRun(task.FinishedAt)
			case mirrorqueue.TaskDiverged:
				// A branch the mirror kept with other history is not a quiet
				// mirror: the next run looks at it again on the interval.
				current.Observe(true, true)
				current.SetNextRun(task.FinishedAt)
			default:
				current.Retry(task.FinishedAt)
			}
		}
		return nil
	})
}

// push publishes the selected references of the target to its mirror. The
// credential is read from the environment when the task runs, so that the
// queue never holds a secret.
func (r *Runner) push(ctx context.Context, tgt *mirrorqueue.Target) ([]string, error) {
	address := tgt.Address
	if address == "" {
		address = r.Config.Address
	}
	if address == "" {
		return nil, fmt.Errorf("the mirror target %q names no server: set GRAFT_ADDRESS or pass -address", tgt.Name)
	}
	credential := Auth{
		Usage:    "mirror queue",
		URL:      tgt.URL,
		TokenEnv: tgt.TokenEnv,
		Username: tgt.Username,
		SSHKey:   tgt.SSHKey,
	}
	// The check that refuses a token in cleartext is made again here, because
	// the environment of the worker is not the environment of the operator who
	// added the target.
	if name, token := Token(credential); token != "" {
		if strings.HasPrefix(tgt.URL, "http://") && !tgt.AllowInsecureHTTP {
			return nil, fmt.Errorf("the mirror target %q refuses to send %s over http://%s: use HTTPS, or allow it on the target", tgt.Name, name, strings.TrimPrefix(tgt.URL, "http://"))
		}
	}
	header, err := Authorization(credential)
	if err != nil {
		return nil, err
	}

	// The queue keeps the paths of the SSH credential, not the secret itself;
	// the worker reads them when the task runs and sends the contents.
	sshKey, err := CredentialFile("mirror queue", "--ssh-key", tgt.SSHKey)
	if err != nil {
		return nil, err
	}
	knownHosts, err := CredentialFile("mirror queue", "--known-hosts", tgt.KnownHosts)
	if err != nil {
		return nil, err
	}

	client, err := r.dial(ctx, address, tgt.Storage)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	res, err := client.Repo(tgt.Repository).Mirror(ctx, sdk.MirrorOptions{
		URL:                     tgt.URL,
		Branches:                tgt.Branches,
		HTTPAuthorizationHeader: header,
		SSHKey:                  sshKey,
		KnownHosts:              knownHosts,
		// The default is the safety net: a mirror that has unrelated history
		// is a person's decision, not a retry's.
		KeepDivergentRefs: !tgt.Force,
		// The server records what the mirror holds under the name of the
		// target, which is what the mirror policy of the server decides the
		// next push with.
		Target: tgt.Name,
	})
	if err != nil {
		return nil, err
	}
	return res.DivergentRefs, nil
}

// dial reaches the server of a target.
func (r *Runner) dial(ctx context.Context, address, storage string) (*sdk.Client, error) {
	opts := []sdk.DialOption{sdk.WithStorage(pick(storage, r.Config.Storage))}
	if r.Config.Token != "" {
		opts = append(opts, sdk.WithToken(r.Config.Token))
	}
	return sdk.Dial(ctx, address, opts...)
}

// change is what the changeset log of one repository said during a run:
// whether a reference the mirror publishes moved since the cursor, and the
// cursor to store. Observed is false when the log could not be read, and then
// the run neither resets the quiet count nor stretches it.
type change struct {
	changed  bool
	observed bool
	seq      uint64
}

// repoKey identifies the repository a target publishes: the changeset log
// belongs to the repository, not to the destination, so the targets of one
// repository share the cursor and the event.
func (r *Runner) repoKey(t *mirrorqueue.Target) string {
	return pick(t.Address, r.Config.Address) + "|" + pick(t.Storage, r.Config.Storage) + "|" + t.Repository
}

// PublishesRef tells whether a changeset touched a reference a mirror
// publishes: branches and tags, never Graft's review or checkpoint
// references, so a review that writes refs/graft/ does not make a mirror due.
func PublishesRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")
}

// observe reads the changeset log of every repository the enabled targets of
// the queue publish. The log is the event source of the queue: every
// reference update the server accepts is appended to it, so a log that moved
// past its cursor is a change the mirror has to carry, and the interval stays
// as the fallback for what the log does not see.
func (r *Runner) observe(ctx context.Context, queue *mirrorqueue.Queue) map[string]change {
	changes := map[string]change{}
	clients := map[string]*sdk.Client{}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	for _, t := range queue.Targets {
		if !t.Enabled || t.Interval <= 0 {
			// A manual target is published when a person asks for it, so its
			// repository is not read here.
			continue
		}
		key := r.repoKey(t)
		if _, done := changes[key]; done {
			continue
		}
		address := pick(t.Address, r.Config.Address)
		if address == "" {
			changes[key] = change{}
			continue
		}
		client, ok := clients[address]
		if !ok {
			dialed, err := r.dial(ctx, address, t.Storage)
			if err != nil {
				changes[key] = change{}
				continue
			}
			client = dialed
			clients[address] = client
		}
		changes[key] = readChanges(ctx, client.Repo(t.Repository), queue.Repos[key])
	}
	return changes
}

// readChanges reads what the changeset log says about one repository since
// its cursor. A repository read for the first time starts at the tail of its
// log and is due once, so that a mirror that is behind catches up right away
// instead of waiting for its interval.
func readChanges(ctx context.Context, repo *sdk.Repo, cursor *mirrorqueue.RepoCursor) change {
	if cursor == nil {
		// The log is read for the first time: the cursor starts at the tail,
		// so that the queue does not replay the whole history of the
		// repository, and the repository counts as quiet until its log says
		// otherwise.
		tail, err := ChangesetTail(ctx, repo)
		if err != nil {
			return change{}
		}
		return change{observed: true, seq: tail}
	}

	seq := cursor.Seq
	changed := false
	for page := 0; page < changesetPages; page++ {
		entries, err := repo.Changesets(ctx, seq, changesetPage)
		if err != nil {
			return change{}
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			if PublishesRef(string(entry.GetReference())) {
				changed = true
			}
			seq = entry.GetSequence()
		}
		if uint32(len(entries)) < changesetPage {
			break
		}
	}
	return change{changed: changed, observed: true, seq: seq}
}

// ChangesetTail returns the last sequence of a repository's changeset log.
// Sequences are dense and start at 1, so a log with entries after n has a
// tail greater than n: the tail is found by growing a bound and bisecting
// it, which costs a handful of reads instead of replaying the log.
func ChangesetTail(ctx context.Context, repo *sdk.Repo) (uint64, error) {
	// Changesets answers the entries with a sequence greater than the one it
	// is given, so "does sequence s exist" is asked as Changesets(s-1, 1) and
	// the entry that comes back is s itself. Asking Changesets(s, 1) instead
	// would answer whether s+1 exists and make the tail one short, which
	// replays the newest entry of an imported repository as a change.
	exists := func(s uint64) (bool, error) {
		if s == 0 {
			return false, nil
		}
		entries, err := repo.Changesets(ctx, s-1, 1)
		if err != nil {
			return false, err
		}
		return len(entries) > 0 && entries[0].GetSequence() == s, nil
	}

	// lo is a sequence that exists (zero when the log may be empty), hi is a
	// sequence that does not.
	lo, hi := uint64(0), uint64(1)
	for i := 0; i < 40; i++ {
		ok, err := exists(hi)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := exists(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// LastChange is the time of the newest entry of the changeset log, the same
// log the events of the queue come from. It is the zero time for a
// repository whose log is empty.
func LastChange(ctx context.Context, repo *sdk.Repo) (time.Time, error) {
	tail, err := ChangesetTail(ctx, repo)
	if err != nil || tail == 0 {
		return time.Time{}, err
	}
	entries, err := repo.Changesets(ctx, tail-1, 1)
	if err != nil || len(entries) == 0 {
		return time.Time{}, err
	}
	return entries[0].GetRecordedAt().AsTime(), nil
}

// recordChanges stores what the changeset logs said: the cursor of every
// repository, and the targets that a change made due right away, so that the
// run that read the log publishes them without waiting for a tick.
func (r *Runner) recordChanges(changes map[string]change) error {
	now := r.now()
	return r.Store.Update(func(queue *mirrorqueue.Queue) error {
		for _, target := range queue.Targets {
			key := r.repoKey(target)
			c, ok := changes[key]
			if !ok {
				continue
			}
			if c.observed {
				if queue.Repos == nil {
					queue.Repos = map[string]*mirrorqueue.RepoCursor{}
				}
				queue.Repos[key] = &mirrorqueue.RepoCursor{Seq: c.seq, ObservedAt: now}
			}
			if c.changed && target.Enabled && target.Interval > 0 {
				if target.NextRun.IsZero() || target.NextRun.After(now) {
					target.NextRun = now
				}
			}
		}
		return nil
	})
}

// TaskID returns the identifier of a task of the history of a queue.
func TaskID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// The identifier only has to be unique inside one queue, so the time
		// is enough when the random source fails.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("task-%d-%s", time.Now().Unix(), hex.EncodeToString(buf[:]))
}

// pick returns the first value that is not empty, else the second.
func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DueTarget is one target of the queue that is due to be published.
type DueTarget struct {
	// Name is the name of the target.
	Name string
	// Repository is the graft repository the target publishes.
	Repository string
	// URL is the Git URL of the mirror.
	URL string
	// Changed tells whether the changeset log of the repository moved past
	// the cursor of the queue: a change is the event that makes a target
	// due before its interval says so.
	Changed bool
	// Observed tells whether the changeset log could be read. A run that
	// could not read it knows nothing new, and the schedule it keeps is the
	// one it had.
	Observed bool
}

// DueResult is what the queue says about its targets.
type DueResult struct {
	// Targets are the targets that are due to be published.
	Targets []DueTarget
}

// Due returns the targets of the queue that are due to be published.
//
// The changeset logs are read first, so that a repository that changed is due
// right away, whatever its interval says. The cursors the logs left behind
// are stored, so that the next run knows what the logs already said.
func (r *Runner) Due(ctx context.Context) (DueResult, error) {
	queue, err := r.Store.Load()
	if err != nil {
		return DueResult{}, err
	}
	changes := map[string]change{}
	if err := r.observeAndRecord(ctx, queue, changes); err != nil {
		return DueResult{}, err
	}
	if queue, err = r.Store.Load(); err != nil {
		return DueResult{}, err
	}
	var result DueResult
	for _, t := range queue.Due(r.now()) {
		c := changes[r.repoKey(t)]
		result.Targets = append(result.Targets, DueTarget{
			Name:       t.Name,
			Repository: t.Repository,
			URL:        t.URL,
			Changed:    c.changed,
			Observed:   c.observed,
		})
	}
	return result, nil
}

// PublishTarget publishes one target of the queue and records the task of the
// run in the history of the queue.
//
// Changed and Observed are what the changeset log said about the repository of
// the target: a change is why the target was published before its interval,
// and a log that could not be read moves neither the quiet count nor the
// schedule.
func (r *Runner) PublishTarget(ctx context.Context, name string, changed, observed bool) (mirrorqueue.Task, error) {
	queue, err := r.Store.Load()
	if err != nil {
		return mirrorqueue.Task{}, err
	}
	target, ok := queue.Target(name)
	if !ok {
		return mirrorqueue.Task{}, fmt.Errorf("the mirror queue has no target named %q", name)
	}
	return r.publishTarget(ctx, target, change{changed: changed, observed: observed})
}
