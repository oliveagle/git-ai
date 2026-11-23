// Package mirrorqueue holds the mirror queue of a graft client: the targets a
// graft repository is published to, and the tasks that published them.
//
// The queue holds no secret. A target names the environment variable that
// carries its token, and the value is read when the task runs, so the file
// that holds the queue never holds a credential.
package mirrorqueue

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task statuses. A task is pending before it runs, running while it runs, and
// then one of the three outcomes a mirror push can have.
const (
	// TaskPending is a task that was written but not started yet.
	TaskPending = "pending"
	// TaskRunning is a task that is publishing its target now.
	TaskRunning = "running"
	// TaskOK is a task that published the selected references.
	TaskOK = "ok"
	// TaskDiverged is a task that kept a remote branch with unrelated history.
	TaskDiverged = "diverged"
	// TaskFailed is a task whose push did not finish.
	TaskFailed = "failed"
)

// Target is one destination of the mirror queue: a graft repository, the Git
// URL it is published to, and the branches that may reach it.
type Target struct {
	// Name identifies the target in the queue. It is unique.
	Name string `json:"name"`
	// Repository is the graft repository that is published, such as "shop.git".
	Repository string `json:"repository"`
	// Address is the server that holds the repository. It is empty when the
	// target is published through the address of the run.
	Address string `json:"address,omitempty"`
	// Storage is the storage name of the repository on that server. It is
	// empty when the target is published through the storage of the run.
	Storage string `json:"storage,omitempty"`
	// URL is the Git URL of the mirror: any host that speaks Git over HTTPS,
	// SSH or the scp-like form.
	URL string `json:"url"`
	// Branches are the branch patterns the mirror may publish, without a
	// refs/heads/ prefix. "*" is a wildcard. Tags are always published.
	Branches []string `json:"branches"`
	// TokenEnv is the name of the environment variable that holds the token
	// of an HTTPS mirror. It is empty when the host of the URL has no
	// convention and the mirror needs no credential.
	TokenEnv string `json:"token_env,omitempty"`
	// Username is the HTTP Basic user of the token.
	Username string `json:"username,omitempty"`
	// SSHKey is the path of the private key of an SSH mirror. The queue keeps
	// the path, never the key: the worker reads the file when the task runs,
	// so the queue file holds no secret.
	SSHKey string `json:"ssh_key,omitempty"`
	// KnownHosts is the path of the host identities that SSH checks, read in
	// the same way as SSHKey.
	KnownHosts string `json:"known_hosts,omitempty"`
	// AllowInsecureHTTP permits a token to be sent over http://.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`
	// Force overwrites a remote branch with unrelated history with the graft
	// branch. It is a decision of a person, so it is set per target.
	Force bool `json:"force,omitempty"`
	// Interval is how often the target is published. Zero means that the
	// target is only published when a person asks for it.
	Interval time.Duration `json:"interval"`
	// IdleInterval is the longest a quiet target waits between two
	// publications: a target whose repository has not changed stretches from
	// Interval towards IdleInterval, and a change puts it back on Interval.
	// Zero leaves the interval fixed, which is what a target of a server
	// without a changeset log gets.
	IdleInterval time.Duration `json:"idle_interval,omitempty"`
	// QuietRuns is the number of publications since the repository last
	// changed that found the mirror up to date. It is what stretches the
	// delay towards IdleInterval, and a change resets it.
	QuietRuns int `json:"quiet_runs,omitempty"`
	// Enabled tells whether the worker publishes the target on its schedule.
	Enabled bool `json:"enabled"`
	// CreatedAt is when the target was added to the queue.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the target was changed.
	UpdatedAt time.Time `json:"updated_at"`
	// NextRun is when the target is published next. It is the zero time when
	// the target has no schedule.
	NextRun time.Time `json:"next_run"`
	// LastTask is the identifier of the last task of the target.
	LastTask string `json:"last_task,omitempty"`
	// LastStatus is the outcome of the last task: TaskOK, TaskDiverged or
	// TaskFailed.
	LastStatus string `json:"last_status,omitempty"`
	// LastError is the reason of the last failure.
	LastError string `json:"last_error,omitempty"`
}

// Task is one attempt to publish a target.
type Task struct {
	// ID identifies the task in the history of the queue.
	ID string `json:"id"`
	// Target is the name of the target the task publishes.
	Target string `json:"target"`
	// Repository is the graft repository the task published.
	Repository string `json:"repository"`
	// URL is the Git URL the task published to.
	URL string `json:"url"`
	// Status is the outcome of the task: TaskPending, TaskRunning, TaskOK,
	// TaskDiverged or TaskFailed.
	Status string `json:"status"`
	// CreatedAt is when the task was written to the queue.
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when the task began to publish.
	StartedAt time.Time `json:"started_at,omitempty"`
	// FinishedAt is when the task ended.
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// DivergentRefs are the remote branches the task kept because they hold
	// history the graft branch does not have.
	DivergentRefs []string `json:"divergent_refs,omitempty"`
	// Changed tells whether the changeset log of the repository showed a
	// change of the references the mirror publishes since the last run of
	// the target. A run that is not triggered by a change is the sweep of the
	// schedule, and a run of a quiet target is what stretches its interval.
	Changed bool `json:"changed,omitempty"`
	// Error is the reason the task failed.
	Error string `json:"error,omitempty"`
}

// Queue is the whole mirror queue of a client: its targets and the tasks that
// published them.
type Queue struct {
	// Version is the version of the file the queue is stored in. A file of an
	// unknown version is refused, so that a newer client never loses a target
	// by reading it as an older one.
	Version int `json:"version"`
	// Targets are the destinations of the queue.
	Targets []*Target `json:"targets"`
	// Tasks are the tasks of the queue, oldest first.
	Tasks []*Task `json:"tasks"`
	// Repos is the cursor of every repository whose changeset log the worker
	// follows, by repository path. The log is the event source of the queue:
	// a repository whose log moved past its cursor has something to publish,
	// and a repository whose log did not move is quiet — which is what
	// stretches the interval of its targets towards their idle interval.
	Repos map[string]*RepoCursor `json:"repos,omitempty"`
}

// RepoCursor is how far the worker has read the changeset log of one
// repository.
type RepoCursor struct {
	// Seq is the last sequence of the log the worker read.
	Seq uint64 `json:"seq"`
	// ObservedAt is when the worker last read the log.
	ObservedAt time.Time `json:"observed_at"`
}

// queueVersion is the version of the queue file this package writes.
const queueVersion = 1

// maxTasks is the number of tasks the queue keeps. The history is a record of
// what the queue did, not a log that grows without a bound.
const maxTasks = 200

// New returns an empty queue.
func New() *Queue {
	return &Queue{Version: queueVersion}
}

// Validate refuses a queue that this package cannot work with.
func (q *Queue) Validate() error {
	if q.Version != queueVersion {
		return fmt.Errorf("the mirror queue has version %d, and this graft knows version %d", q.Version, queueVersion)
	}
	for _, t := range q.Targets {
		if err := t.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a target that cannot be published.
func (t *Target) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("a mirror target needs a name")
	}
	if strings.ContainsAny(t.Name, " \t\n") {
		return fmt.Errorf("the name %q of a mirror target holds a space", t.Name)
	}
	if strings.TrimSpace(t.Repository) == "" {
		return fmt.Errorf("the mirror target %q needs a repository", t.Name)
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
	if t.SSHKey != "" && t.TokenEnv != "" {
		return fmt.Errorf("the mirror target %q names both an SSH key and a token: it takes either, not both", t.Name)
	}
	if t.Interval < 0 {
		return fmt.Errorf("the interval of the mirror target %q is negative", t.Name)
	}
	if t.IdleInterval < 0 {
		return fmt.Errorf("the idle interval of the mirror target %q is negative", t.Name)
	}
	if t.IdleInterval > 0 && t.Interval > 0 && t.IdleInterval < t.Interval {
		return fmt.Errorf("the idle interval of the mirror target %q is shorter than its interval", t.Name)
	}
	return nil
}

// Target returns the target of the queue with that name.
func (q *Queue) Target(name string) (*Target, bool) {
	for _, t := range q.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}

// AddTarget adds a target to the queue. The name of a target is unique: a
// second target with the same name would publish the same repository twice,
// and the operator would not know which one the worker ran.
func (q *Queue) AddTarget(t *Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if _, ok := q.Target(t.Name); ok {
		return fmt.Errorf("the mirror queue already has a target named %q", t.Name)
	}
	q.Targets = append(q.Targets, t)
	return nil
}

// UpdateTarget replaces the declaration of a target the queue holds, and
// returns the target as it is stored: the history of the target — when it was
// added, and what its last task did — is kept, because a new schedule is not a
// new mirror, and the policy of the server reads that history.
func (q *Queue) UpdateTarget(t *Target) (*Target, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	for i, target := range q.Targets {
		if target.Name != t.Name {
			continue
		}
		t.CreatedAt = target.CreatedAt
		t.Enabled = target.Enabled
		t.LastTask = target.LastTask
		t.LastStatus = target.LastStatus
		t.LastError = target.LastError
		t.UpdatedAt = time.Now()
		q.Targets[i] = t
		return t, nil
	}
	return nil, fmt.Errorf("the mirror queue has no target named %q", t.Name)
}

// RemoveTarget takes a target out of the queue. The tasks of the target stay
// in the history: they record what the queue did.
func (q *Queue) RemoveTarget(name string) error {
	for i, t := range q.Targets {
		if t.Name == name {
			q.Targets = append(q.Targets[:i], q.Targets[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("the mirror queue has no target named %q", name)
}

// Due returns the targets that are due at that time: the enabled targets
// whose schedule says so. A target without a schedule is never due, because a
// person has to ask for it.
func (q *Queue) Due(now time.Time) []*Target {
	var due []*Target
	for _, t := range q.Targets {
		if !t.Enabled || t.Interval <= 0 {
			continue
		}
		if t.NextRun.After(now) {
			continue
		}
		due = append(due, t)
	}
	return due
}

// AddTask records a task in the history of the queue and trims the history to
// the number of tasks the queue keeps.
func (q *Queue) AddTask(t *Task) {
	q.Tasks = append(q.Tasks, t)
	q.TrimTasks(maxTasks)
}

// TrimTasks keeps the last tasks of the history.
func (q *Queue) TrimTasks(max int) {
	if max < 0 || len(q.Tasks) <= max {
		return
	}
	q.Tasks = append([]*Task(nil), q.Tasks[len(q.Tasks)-max:]...)
}

// Task returns the task of the history with that identifier.
func (q *Queue) Task(id string) (*Task, bool) {
	for _, t := range q.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// LastTasks returns the tasks of the history, newest first, at most limit of
// them. A limit of zero returns every task.
func (q *Queue) LastTasks(target string, limit int) []*Task {
	var tasks []*Task
	for i := len(q.Tasks) - 1; i >= 0; i-- {
		t := q.Tasks[i]
		if target != "" && t.Target != target {
			continue
		}
		tasks = append(tasks, t)
		if limit > 0 && len(tasks) >= limit {
			break
		}
	}
	return tasks
}

// SetNextRun sets the time the target is published next. A target without a
// schedule is never due. A quiet target waits longer than an active one: the
// delay doubles once per quiet run, up to the idle interval the operator
// named, so that an idle repository is checked as often as the fallback
// needs and an active one as often as its interval says.
func (t *Target) SetNextRun(now time.Time) {
	if t.Interval <= 0 {
		t.NextRun = time.Time{}
		return
	}
	t.NextRun = now.Add(t.NextDelay())
}

// NextDelay is how long the target waits after a run. It is the interval,
// doubled once per quiet run up to the idle interval; a target without an
// idle interval waits the interval whatever its quiet runs say.
func (t *Target) NextDelay() time.Duration {
	if t.IdleInterval <= t.Interval || t.QuietRuns <= 0 {
		return t.Interval
	}
	delay := t.Interval
	for i := 0; i < t.QuietRuns; i++ {
		if delay >= t.IdleInterval/2 {
			return t.IdleInterval
		}
		delay *= 2
	}
	if delay > t.IdleInterval {
		return t.IdleInterval
	}
	return delay
}

// Retry schedules a run that failed. A failure is not silence: it waits the
// interval, not the stretched delay of a quiet target, so that a broken
// mirror is repaired as often as the operator asked and not as often as the
// fallback allows.
func (t *Target) Retry(now time.Time) {
	if t.Interval <= 0 {
		t.NextRun = time.Time{}
		return
	}
	t.NextRun = now.Add(t.Interval)
}

// Observe records the outcome of one publication for the schedule: a run
// that carried a change makes the target active again, and a run that found
// the mirror up to date lets it stretch towards its idle interval. A run
// whose changeset log could not be read does not tell either way, so it
// leaves the quiet count alone.
func (t *Target) Observe(changed, observed bool) {
	if !observed {
		return
	}
	if changed {
		t.QuietRuns = 0
		return
	}
	t.QuietRuns++
}
