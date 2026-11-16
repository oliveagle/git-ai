package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/sdk"
)

// mirrorQueueSynopsis is the command line of graft mirror queue.
const mirrorQueueSynopsis = "usage: graft mirror queue <add|remove|list|show|enable|disable|run|worker> [flags]"

// mirrorQueueUsage lists the subcommands of graft mirror queue.
const mirrorQueueUsage = "mirror queue requires a subcommand: " + mirrorQueueSynopsis

// mirrorTaskSynopsis is the command line of graft mirror task.
const mirrorTaskSynopsis = "usage: graft mirror task <list|show> [flags]"

// mirrorTaskUsage lists the subcommands of graft mirror task.
const mirrorTaskUsage = "mirror task requires a subcommand: " + mirrorTaskSynopsis

// mirrorQueueDefaults are the defaults of the queue commands.
const (
	// defaultQueueInterval is how often a target is published when the
	// operator does not name another interval.
	defaultQueueInterval = 5 * time.Minute
	// defaultQueueTick is how often the worker looks for targets that are due.
	defaultQueueTick = 30 * time.Second
	// defaultQueueConcurrency is how many tasks run at the same time.
	defaultQueueConcurrency = 4
	// defaultTaskLimit is how many tasks the task list prints.
	defaultTaskLimit = 20
)

// mirrorQueueTarget is one target of the queue, as the command prints it.
type mirrorQueueTarget struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	URL        string   `json:"url"`
	Branches   []string `json:"branches"`
	TokenEnv   string   `json:"token_env,omitempty"`
	Username   string   `json:"username,omitempty"`
	SSHKey     string   `json:"ssh_key,omitempty"`
	KnownHosts string   `json:"known_hosts,omitempty"`
	Force      bool     `json:"force,omitempty"`
	Interval   string   `json:"interval"`
	Enabled    bool     `json:"enabled"`
	NextRun    string   `json:"next_run,omitempty"`
	LastTask   string   `json:"last_task,omitempty"`
	LastStatus string   `json:"last_status,omitempty"`
	LastError  string   `json:"last_error,omitempty"`
}

// mirrorQueueTask is one task of the history, as the command prints it.
type mirrorQueueTask struct {
	ID            string   `json:"id"`
	Target        string   `json:"target"`
	Repository    string   `json:"repository"`
	URL           string   `json:"url"`
	Status        string   `json:"status"`
	CreatedAt     string   `json:"created_at"`
	StartedAt     string   `json:"started_at,omitempty"`
	FinishedAt    string   `json:"finished_at,omitempty"`
	DivergentRefs []string `json:"divergent_refs,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// mirrorQueueTargetJSON returns the target as the command prints it.
func mirrorQueueTargetJSON(t *mirrorqueue.Target) mirrorQueueTarget {
	out := mirrorQueueTarget{
		Name:       t.Name,
		Repository: t.Repository,
		URL:        t.URL,
		Branches:   append([]string(nil), t.Branches...),
		TokenEnv:   t.TokenEnv,
		Username:   t.Username,
		SSHKey:     t.SSHKey,
		KnownHosts: t.KnownHosts,
		Force:      t.Force,
		Interval:   mirrorQueueInterval(t.Interval),
		Enabled:    t.Enabled,
		LastTask:   t.LastTask,
		LastStatus: t.LastStatus,
		LastError:  t.LastError,
	}
	if !t.NextRun.IsZero() {
		out.NextRun = t.NextRun.Format(time.RFC3339)
	}
	return out
}

// mirrorQueueTaskJSON returns the task as the command prints it.
func mirrorQueueTaskJSON(t *mirrorqueue.Task) mirrorQueueTask {
	out := mirrorQueueTask{
		ID:            t.ID,
		Target:        t.Target,
		Repository:    t.Repository,
		URL:           t.URL,
		Status:        t.Status,
		DivergentRefs: append([]string(nil), t.DivergentRefs...),
		Error:         t.Error,
	}
	if !t.CreatedAt.IsZero() {
		out.CreatedAt = t.CreatedAt.Format(time.RFC3339)
	}
	if !t.StartedAt.IsZero() {
		out.StartedAt = t.StartedAt.Format(time.RFC3339)
	}
	if !t.FinishedAt.IsZero() {
		out.FinishedAt = t.FinishedAt.Format(time.RFC3339)
	}
	return out
}

// mirrorQueueInterval returns the interval of a target as a person reads it.
// A target without a schedule is only published when a person asks for it.
func mirrorQueueInterval(interval time.Duration) string {
	if interval <= 0 {
		return "manual"
	}
	return interval.String()
}

// mirrorQueueStore returns the store of the queue. The file is named by the
// flag, else by the environment, else by the default of the user.
func mirrorQueueStore(q *mirrorQueueFlags) (*mirrorqueue.FileStore, error) {
	path := q.queueFile
	if path == "" {
		path = os.Getenv("GRAFT_MIRROR_QUEUE")
	}
	if path == "" {
		defaultPath, err := mirrorqueue.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = defaultPath
	}
	return mirrorqueue.NewFileStore(path), nil
}

// cmdMirrorQueue dispatches a mirror queue subcommand. The command line is
// checked before the queue is touched.
func cmdMirrorQueue(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", mirrorQueueUsage)
	}
	switch args[0] {
	case "add":
		return mirrorQueueAdd(ctx, out, o, f, q, args[1:])
	case "remove":
		return mirrorQueueRemove(ctx, out, o, f, q, args[1:])
	case "enable":
		return mirrorQueueToggle(ctx, out, o, f, q, args[1:], true)
	case "disable":
		return mirrorQueueToggle(ctx, out, o, f, q, args[1:], false)
	case "list":
		return mirrorQueueList(ctx, out, o, f, q, args[1:])
	case "show":
		return mirrorQueueShow(ctx, out, o, f, q, args[1:])
	case "run":
		return mirrorQueueRun(ctx, out, o, f, q, args[1:])
	case "worker":
		return mirrorQueueWorker(ctx, out, o, f, q, args[1:])
	default:
		return usagef("unknown mirror queue subcommand %q: %s", args[0], mirrorQueueUsage)
	}
}

// mirrorQueueAdd puts a target into the queue.
func mirrorQueueAdd(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 1 {
		return usagef("mirror queue add takes the name of the target")
	}
	if f.repo == "" {
		return usagef("mirror queue add requires --repo REPO")
	}
	if f.url == "" {
		return usagef("mirror queue add requires --url URL")
	}
	if len(f.branches) == 0 {
		return usagef("mirror queue add requires --branch PATTERN: name the branches the mirror may publish")
	}
	if f.sshKey != "" && f.tokenEnv != "" {
		return usagef("mirror queue add takes either --token-env or --ssh-key, not both")
	}
	interval := q.interval
	switch {
	case q.manual && interval >= 0:
		return usagef("mirror queue add takes either --interval or --manual, not both")
	case q.manual:
		interval = 0
	case interval < 0:
		interval = defaultQueueInterval
	}
	credential := mirrorAuth{
		usage:    "mirror queue add",
		url:      f.url,
		tokenEnv: f.tokenEnv,
		username: f.username,
		sshKey:   f.sshKey,
	}
	// The queue names the variable that carries the token, so that an
	// operator reads the queue and knows what to put in the environment of
	// the worker. The value of the variable is never stored.
	tokenEnv := credential.tokenEnv
	if tokenEnv == "" {
		tokenEnv = mirrorDefaults(f.url).tokenEnv
	}
	// The token is not stored, so the check that refuses a token in cleartext
	// is made here and again when the task runs.
	if name, token := mirrorToken(credential); token != "" && strings.HasPrefix(f.url, "http://") && !f.allowInsecureHTTP {
		return usagef("mirror queue add refuses to send %s over http://%s: use HTTPS, or pass --allow-insecure-http", name, strings.TrimPrefix(f.url, "http://"))
	}
	now := time.Now()
	target := &mirrorqueue.Target{
		Name:              args[0],
		Repository:        f.repo,
		Address:           o.address,
		Storage:           o.storage,
		URL:               f.url,
		Branches:          append([]string(nil), f.branches...),
		TokenEnv:          tokenEnv,
		Username:          f.username,
		SSHKey:            f.sshKey,
		KnownHosts:        f.knownHosts,
		AllowInsecureHTTP: f.allowInsecureHTTP,
		Force:             f.force,
		Interval:          interval,
		Enabled:           true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	// A target that was added is due right away, so that the queue publishes
	// it without waiting for the next tick of the worker.
	if target.Interval > 0 {
		target.NextRun = now
	}

	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		return queue.AddTarget(target)
	}); err != nil {
		return err
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(target))
	}
	fmt.Fprintf(out, "added the mirror target %s: %s -> %s (branches: %s, every %s)\n",
		target.Name, target.Repository, mirrorDisplayURL(target.URL),
		strings.Join(target.Branches, ", "), mirrorQueueInterval(target.Interval))
	return nil
}

// mirrorQueueRemove takes a target out of the queue.
func mirrorQueueRemove(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 1 {
		return usagef("mirror queue remove takes the name of the target")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	var removed *mirrorqueue.Target
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		target, ok := queue.Target(args[0])
		if !ok {
			return fmt.Errorf("the mirror queue has no target named %q", args[0])
		}
		removed = target
		return queue.RemoveTarget(args[0])
	}); err != nil {
		return err
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(removed))
	}
	fmt.Fprintf(out, "removed the mirror target %s\n", removed.Name)
	return nil
}

// mirrorQueueToggle enables or disables a target. A disabled target keeps its
// schedule and its history, and the worker leaves it alone.
func mirrorQueueToggle(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string, enabled bool) error {
	if len(args) != 1 {
		return usagef("mirror queue %s takes the name of the target", map[bool]string{true: "enable", false: "disable"}[enabled])
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	var changed *mirrorqueue.Target
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		target, ok := queue.Target(args[0])
		if !ok {
			return fmt.Errorf("the mirror queue has no target named %q", args[0])
		}
		target.Enabled = enabled
		target.UpdatedAt = time.Now()
		if enabled {
			// An enabled target is due right away, so that turning it on
			// publishes it without waiting for the next tick.
			target.NextRun = time.Now()
		} else {
			target.NextRun = time.Time{}
		}
		changed = target
		return nil
	}); err != nil {
		return err
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(changed))
	}
	if enabled {
		fmt.Fprintf(out, "enabled the mirror target %s\n", changed.Name)
	} else {
		fmt.Fprintf(out, "disabled the mirror target %s\n", changed.Name)
	}
	return nil
}

// mirrorQueueList prints the targets of the queue.
func mirrorQueueList(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 0 {
		return usagef("mirror queue list takes no arguments")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}
	targets := append([]*mirrorqueue.Target(nil), queue.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	if o.jsonOut {
		list := make([]mirrorQueueTarget, 0, len(targets))
		for _, t := range targets {
			list = append(list, mirrorQueueTargetJSON(t))
		}
		return mirrorQueueWriteJSON(out, list)
	}
	if len(targets) == 0 {
		fmt.Fprintln(out, "the mirror queue is empty")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPOSITORY\tURL\tBRANCHES\tENABLED\tEVERY\tNEXT RUN\tLAST")
	for _, t := range targets {
		next := "-"
		if !t.NextRun.IsZero() {
			next = t.NextRun.Format(time.RFC3339)
		}
		last := t.LastStatus
		if last == "" {
			last = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, t.Repository, mirrorDisplayURL(t.URL), strings.Join(t.Branches, ","),
			map[bool]string{true: "yes", false: "no"}[t.Enabled],
			mirrorQueueInterval(t.Interval), next, last)
	}
	return w.Flush()
}

// mirrorQueueShow prints one target of the queue.
func mirrorQueueShow(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 1 {
		return usagef("mirror queue show takes the name of the target")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}
	target, ok := queue.Target(args[0])
	if !ok {
		return fmt.Errorf("the mirror queue has no target named %q", args[0])
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(target))
	}
	fmt.Fprintf(out, "target: %s\n", target.Name)
	fmt.Fprintf(out, "repository: %s\n", target.Repository)
	fmt.Fprintf(out, "url: %s\n", mirrorDisplayURL(target.URL))
	fmt.Fprintf(out, "branches: %s\n", strings.Join(target.Branches, ", "))
	fmt.Fprintf(out, "every: %s\n", mirrorQueueInterval(target.Interval))
	fmt.Fprintf(out, "enabled: %s\n", map[bool]string{true: "yes", false: "no"}[target.Enabled])
	if !target.NextRun.IsZero() {
		fmt.Fprintf(out, "next run: %s\n", target.NextRun.Format(time.RFC3339))
	}
	if target.TokenEnv != "" {
		fmt.Fprintf(out, "token: %s\n", target.TokenEnv)
	}
	if target.Username != "" {
		fmt.Fprintf(out, "username: %s\n", target.Username)
	}
	if target.SSHKey != "" {
		fmt.Fprintf(out, "ssh key: %s\n", target.SSHKey)
	}
	if target.Force {
		fmt.Fprintln(out, "force: yes")
	}
	if target.LastStatus != "" {
		fmt.Fprintf(out, "last: %s", target.LastStatus)
		if target.LastError != "" {
			fmt.Fprintf(out, " (%s)", target.LastError)
		}
		fmt.Fprintln(out)
	}
	return nil
}

// mirrorQueueRun publishes the targets that are due, and exits. It is the
// run of a cron job: the queue does the work, the schedule of the cron job
// decides how often it is looked at.
func mirrorQueueRun(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 0 {
		return usagef("mirror queue run takes no arguments")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}
	targets, err := mirrorQueueSelection(queue, q)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		if o.jsonOut {
			return mirrorQueueWriteJSON(out, []mirrorQueueTask{})
		}
		fmt.Fprintln(out, "no mirror target is due")
		return nil
	}
	tasks := mirrorQueuePublish(ctx, out, o, f, q, store, targets)
	return mirrorQueueReport(out, o, tasks)
}

// mirrorQueueWorker publishes the targets that are due, over and over, until
// the command is interrupted. It is the supervisor the mirror needs: the
// schedule of the targets decides what is published, and the worker keeps
// running.
func mirrorQueueWorker(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 0 {
		return usagef("mirror queue worker takes no arguments")
	}
	tick := q.tick
	if tick <= 0 {
		tick = defaultQueueTick
	}
	for {
		if err := mirrorQueueRun(ctx, out, o, f, q, nil); err != nil {
			// A failed task is a fact of the run, not a reason to stop the
			// worker: the next tick tries again.
			fmt.Fprintf(o.stderr, "graft mirror queue worker: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tick):
		}
	}
}

// mirrorQueueSelection returns the targets of a run: the ones that are due,
// or the one the caller named, or every enabled target.
func mirrorQueueSelection(queue *mirrorqueue.Queue, q *mirrorQueueFlags) ([]*mirrorqueue.Target, error) {
	if q.target != "" {
		target, ok := queue.Target(q.target)
		if !ok {
			return nil, fmt.Errorf("the mirror queue has no target named %q", q.target)
		}
		return []*mirrorqueue.Target{target}, nil
	}
	if q.all {
		var targets []*mirrorqueue.Target
		for _, t := range queue.Targets {
			if t.Enabled {
				targets = append(targets, t)
			}
		}
		return targets, nil
	}
	return queue.Due(time.Now()), nil
}

// mirrorQueuePublish publishes the targets and records a task for each of
// them. The tasks run at the same time, at most as many as the concurrency
// allows.
func mirrorQueuePublish(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, store *mirrorqueue.FileStore, targets []*mirrorqueue.Target) []mirrorqueue.Task {
	concurrency := q.concurrency
	if concurrency <= 0 {
		concurrency = defaultQueueConcurrency
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

			task := mirrorQueuePublishTarget(ctx, o, f, store, target)
			mu.Lock()
			tasks = append(tasks, task)
			mu.Unlock()
		}(target)
	}
	wg.Wait()

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Target < tasks[j].Target })
	return tasks
}

// mirrorQueuePublishTarget publishes one target and records the task of the
// run in the history of the queue.
func mirrorQueuePublishTarget(ctx context.Context, o *options, f *mirrorFlags, store *mirrorqueue.FileStore, target *mirrorqueue.Target) mirrorqueue.Task {
	now := time.Now()
	task := &mirrorqueue.Task{
		ID:         mirrorTaskID(),
		Target:     target.Name,
		Repository: target.Repository,
		URL:        target.URL,
		Status:     mirrorqueue.TaskRunning,
		CreatedAt:  now,
		StartedAt:  now,
	}
	mirrorQueueRecord(store, target, task)

	divergent, err := mirrorQueuePush(ctx, o, target)
	finished := time.Now()
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
	mirrorQueueRecord(store, target, task)

	return *task
}

// mirrorQueueRecord writes the task of a run into the history of the queue,
// and the outcome of the run into its target.
func mirrorQueueRecord(store *mirrorqueue.FileStore, target *mirrorqueue.Target, task *mirrorqueue.Task) {
	_ = store.Update(func(queue *mirrorqueue.Queue) error {
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
			current.SetNextRun(task.FinishedAt)
		}
		return nil
	})
}

// mirrorQueuePush publishes the selected references of the target to its
// mirror. The credential is read from the environment when the task runs, so
// that the queue never holds a secret.
func mirrorQueuePush(ctx context.Context, o *options, tgt *mirrorqueue.Target) ([]string, error) {
	address := tgt.Address
	if address == "" {
		address = o.address
	}
	if address == "" {
		return nil, fmt.Errorf("the mirror target %q names no server: set GRAFT_ADDRESS or pass -address", tgt.Name)
	}
	credential := mirrorAuth{
		usage:    "mirror queue",
		url:      tgt.URL,
		tokenEnv: tgt.TokenEnv,
		username: tgt.Username,
		sshKey:   tgt.SSHKey,
	}
	// The check that refuses a token in cleartext is made again here, because
	// the environment of the worker is not the environment of the operator who
	// added the target.
	if name, token := mirrorToken(credential); token != "" {
		if strings.HasPrefix(tgt.URL, "http://") && !tgt.AllowInsecureHTTP {
			return nil, fmt.Errorf("the mirror target %q refuses to send %s over http://%s: use HTTPS, or allow it on the target", tgt.Name, name, strings.TrimPrefix(tgt.URL, "http://"))
		}
	}
	header, err := mirrorAuthorization(credential)
	if err != nil {
		return nil, err
	}

	backend, err := openSDKBackend(ctx, target{Address: address, Token: o.token, Storage: pick(tgt.Storage, o.storage), Repo: tgt.Repository})
	if err != nil {
		return nil, err
	}
	defer func() { _ = backend.Close() }()

	res, err := backend.repo.Mirror(ctx, sdk.MirrorOptions{
		URL:                     tgt.URL,
		Branches:                tgt.Branches,
		HTTPAuthorizationHeader: header,
		SSHKey:                  tgt.SSHKey,
		KnownHosts:              tgt.KnownHosts,
		// The default is the safety net: a mirror that has unrelated history
		// is a person's decision, not a retry's.
		KeepDivergentRefs: !tgt.Force,
	})
	if err != nil {
		return nil, err
	}
	return res.DivergentRefs, nil
}

// mirrorQueueReport prints the tasks of a run, and says whether the run
// failed. A run that kept a divergent branch or that could not publish is
// not a success, and the exit status says so.
func mirrorQueueReport(out io.Writer, o *options, tasks []mirrorqueue.Task) error {
	if o.jsonOut {
		list := make([]mirrorQueueTask, 0, len(tasks))
		for _, t := range tasks {
			list = append(list, mirrorQueueTaskJSON(&t))
		}
		if err := mirrorQueueWriteJSON(out, list); err != nil {
			return err
		}
	} else {
		failed := 0
		for _, t := range tasks {
			switch t.Status {
			case mirrorqueue.TaskOK:
				fmt.Fprintf(out, "published %s to %s (%s)\n", t.Repository, mirrorDisplayURL(t.URL), t.Target)
			case mirrorqueue.TaskDiverged:
				failed++
				fmt.Fprintf(out, "kept the divergent ref %s at %s (%s)\n", strings.Join(t.DivergentRefs, ", "), mirrorDisplayURL(t.URL), t.Target)
			default:
				failed++
				fmt.Fprintf(out, "failed to publish %s to %s (%s): %s\n", t.Repository, mirrorDisplayURL(t.URL), t.Target, t.Error)
			}
		}
	}
	for _, t := range tasks {
		if t.Status != mirrorqueue.TaskOK {
			return fmt.Errorf("%d of %d mirror tasks did not publish", countFailed(tasks), len(tasks))
		}
	}
	return nil
}

// countFailed returns the number of tasks that did not publish.
func countFailed(tasks []mirrorqueue.Task) int {
	failed := 0
	for _, t := range tasks {
		if t.Status != mirrorqueue.TaskOK {
			failed++
		}
	}
	return failed
}

// cmdMirrorTask dispatches a mirror task subcommand.
func cmdMirrorTask(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", mirrorTaskUsage)
	}
	switch args[0] {
	case "list":
		return mirrorTaskList(ctx, out, o, f, q, args[1:])
	case "show":
		return mirrorTaskShow(ctx, out, o, f, q, args[1:])
	default:
		return usagef("unknown mirror task subcommand %q: %s", args[0], mirrorTaskUsage)
	}
}

// mirrorTaskList prints the tasks of the history, newest first.
func mirrorTaskList(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 0 {
		return usagef("mirror task list takes no arguments")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}
	limit := q.limit
	if limit <= 0 {
		limit = defaultTaskLimit
	}
	tasks := queue.LastTasks(q.target, limit)
	if o.jsonOut {
		list := make([]mirrorQueueTask, 0, len(tasks))
		for _, t := range tasks {
			list = append(list, mirrorQueueTaskJSON(t))
		}
		return mirrorQueueWriteJSON(out, list)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(out, "the mirror queue has no task yet")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTARGET\tREPOSITORY\tSTATUS\tFINISHED\tDETAIL")
	for _, t := range tasks {
		detail := t.Error
		if len(t.DivergentRefs) > 0 {
			detail = strings.Join(t.DivergentRefs, ",")
		}
		if detail == "" {
			detail = "-"
		}
		finished := "-"
		if !t.FinishedAt.IsZero() {
			finished = t.FinishedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Target, t.Repository, t.Status, finished, detail)
	}
	return w.Flush()
}

// mirrorTaskShow prints one task of the history.
func mirrorTaskShow(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 1 {
		return usagef("mirror task show takes the identifier of the task")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}
	task, ok := queue.Task(args[0])
	if !ok {
		return fmt.Errorf("the mirror queue has no task %q", args[0])
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTaskJSON(task))
	}
	fmt.Fprintf(out, "task: %s\n", task.ID)
	fmt.Fprintf(out, "target: %s\n", task.Target)
	fmt.Fprintf(out, "repository: %s\n", task.Repository)
	fmt.Fprintf(out, "url: %s\n", mirrorDisplayURL(task.URL))
	fmt.Fprintf(out, "status: %s\n", task.Status)
	if !task.StartedAt.IsZero() {
		fmt.Fprintf(out, "started: %s\n", task.StartedAt.Format(time.RFC3339))
	}
	if !task.FinishedAt.IsZero() {
		fmt.Fprintf(out, "finished: %s\n", task.FinishedAt.Format(time.RFC3339))
	}
	for _, ref := range task.DivergentRefs {
		fmt.Fprintf(out, "kept the divergent ref %s\n", ref)
	}
	if task.Error != "" {
		fmt.Fprintf(out, "error: %s\n", task.Error)
	}
	return nil
}

// mirrorQueueWriteJSON writes one JSON object, or one JSON array.
func mirrorQueueWriteJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

// mirrorTaskID returns the identifier of a task.
func mirrorTaskID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// The identifier only has to be unique inside one queue, so the time
		// is enough when the random source fails.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("task-%d-%s", time.Now().Unix(), hex.EncodeToString(buf[:]))
}

// mirrorQueueFlags holds the flags of the mirror queue commands.
type mirrorQueueFlags struct {
	// interval is how often a target is published.
	interval time.Duration
	// tick is how often the worker looks for the targets that are due.
	tick time.Duration
	// concurrency is how many tasks run at the same time.
	concurrency int
	// queueFile is the file the queue is kept in.
	queueFile string
	// target is the name of the one target a command works on.
	target string
	// limit is how many tasks the task list prints.
	limit int
	// all makes a run publish every enabled target, not only the due ones.
	all bool
	// manual keeps a target out of the schedule: it is only published when a
	// person asks for it.
	manual bool
}
