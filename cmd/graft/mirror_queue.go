package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
	"github.com/feder-cr/git-ai/sdk"
)

// mirrorQueueSynopsis is the command line of graft mirror queue.
const mirrorQueueSynopsis = "usage: graft mirror queue <add|update|remove|register|list|show|enable|disable|idle|tier|run|worker> [flags]"

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
	// defaultTaskTimeout is how long one publication may take. A mirror that
	// hangs is a mirror that starves the schedule of every other target, and
	// with events on the schedule also carries the latency of a change, so a
	// task is given a deadline by default.
	defaultTaskTimeout = 2 * time.Minute
)

// mirrorQueueTarget is one target of the queue, as the command prints it.
type mirrorQueueTarget struct {
	Name            string   `json:"name"`
	Repository      string   `json:"repository"`
	URL             string   `json:"url"`
	Branches        []string `json:"branches"`
	TokenEnv        string   `json:"token_env,omitempty"`
	Username        string   `json:"username,omitempty"`
	SSHKey          string   `json:"ssh_key,omitempty"`
	KnownHosts      string   `json:"known_hosts,omitempty"`
	Force           bool     `json:"force,omitempty"`
	Interval        string   `json:"interval"`
	IdleInterval    string   `json:"idle_interval,omitempty"`
	QuietRuns       int      `json:"quiet_runs,omitempty"`
	FailureInterval string   `json:"failure_interval,omitempty"`
	Failures        int      `json:"failures,omitempty"`
	Enabled         bool     `json:"enabled"`
	NextRun         string   `json:"next_run,omitempty"`
	LastTask        string   `json:"last_task,omitempty"`
	LastStatus      string   `json:"last_status,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
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
		Name:            t.Name,
		Repository:      t.Repository,
		URL:             t.URL,
		Branches:        append([]string(nil), t.Branches...),
		TokenEnv:        t.TokenEnv,
		Username:        t.Username,
		SSHKey:          t.SSHKey,
		KnownHosts:      t.KnownHosts,
		Force:           t.Force,
		Interval:        mirrorQueueInterval(t.Interval),
		IdleInterval:    mirrorQueueIdleInterval(t),
		QuietRuns:       t.QuietRuns,
		FailureInterval: mirrorQueueFailureInterval(t),
		Failures:        t.Failures,
		Enabled:         t.Enabled,
		LastTask:        t.LastTask,
		LastStatus:      t.LastStatus,
		LastError:       t.LastError,
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

// mirrorQueueIdleInterval is the idle interval of a target as the command
// prints it, empty when the target has none.
func mirrorQueueIdleInterval(t *mirrorqueue.Target) string {
	if t.IdleInterval <= 0 {
		return ""
	}
	return t.IdleInterval.String()
}

// mirrorQueueSchedule is the schedule of a target as the command prints it: the
// interval, and the idle interval the target stretches towards while its
// repository does not change.
func mirrorQueueFailureInterval(t *mirrorqueue.Target) string {
	if t.FailureInterval <= 0 {
		return ""
	}
	return t.FailureInterval.String()
}

func mirrorQueueSchedule(t *mirrorqueue.Target) string {
	if t.Interval <= 0 {
		return "manual"
	}
	schedule := t.Interval.String()
	if t.IdleInterval > 0 {
		schedule = fmt.Sprintf("%s, idle %s", schedule, t.IdleInterval)
	}
	if cap := mirrorQueueFailureInterval(t); cap != "" {
		schedule = fmt.Sprintf("%s, failing %s", schedule, cap)
	} else if t.IdleInterval > 0 {
		schedule = fmt.Sprintf("%s, failing %s", schedule, t.IdleInterval)
	}
	return schedule
}

// mirrorQueueStore returns the store of the queue. The file is named by the
// flag, else by the environment, else by the default of the user.
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

// mirrorQueueRunner returns the worker of the queue: the code that publishes
// the targets and records what every publication did. The same worker runs
// the queue of a person and the queue of a deployment, so that the two cannot
// drift apart.
func mirrorQueueRunner(store *mirrorqueue.FileStore, o *options) *mirrorsweep.Runner {
	return &mirrorsweep.Runner{
		Store: store,
		Config: mirrorsweep.Config{
			Address: o.address,
			Token:   o.token,
			Storage: o.storage,
			User:    o.user,
		},
	}
}

// firstArg returns the first argument of a command, empty when it has none.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func cmdMirrorQueue(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", mirrorQueueUsage)
	}
	switch args[0] {
	case "add":
		return mirrorQueueAdd(ctx, out, o, f, q, args[1:])
	case "update":
		return mirrorQueueUpdate(ctx, out, o, f, q, args[1:])
	case "remove":
		return mirrorQueueRemove(ctx, out, o, f, q, args[1:])
	case "register":
		return mirrorQueueRegister(ctx, out, o, f, q, args[1:])
	case "enable":
		return mirrorQueueToggle(ctx, out, o, f, q, args[1:], true)
	case "disable":
		return mirrorQueueToggle(ctx, out, o, f, q, args[1:], false)
	case "list":
		return mirrorQueueList(ctx, out, o, f, q, args[1:])
	case "show":
		return mirrorQueueShow(ctx, out, o, f, q, args[1:])
	case "idle":
		return mirrorQueueIdle(ctx, out, o, f, q, args[1:])
	case "tier":
		return mirrorQueueTier(ctx, out, o, f, q, args[1:])
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
	target, err := mirrorQueueDeclare(o, f, q, "mirror queue add", args[0])
	if err != nil {
		return err
	}
	now := time.Now()
	target.Enabled = true
	target.CreatedAt = now
	target.UpdatedAt = now

	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		return queue.AddTarget(target)
	}); err != nil {
		return err
	}
	// The server is what holds a push to the mirror policy: a target that the
	// server does not know would count for the queue and for nothing else.
	// The local queue is the source of the targets, so a server that refuses
	// the target leaves the queue as it was.
	if err := mirrorQueueRegisterTargets(ctx, o, target); err != nil {
		_ = store.Update(func(queue *mirrorqueue.Queue) error {
			return queue.RemoveTarget(target.Name)
		})
		return err
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(target))
	}
	fmt.Fprintf(out, "added the mirror target %s: %s -> %s (branches: %s, every %s)\n",
		target.Name, target.Repository, mirrorDisplayURL(target.URL),
		strings.Join(target.Branches, ", "), mirrorQueueSchedule(target))
	return nil
}

// mirrorQueueUpdate restates the declaration of a target the queue holds: a
// schedule, a URL or a set of branches that changed is a declaration that
// changed, and the history of the target — when it was added, what its last
// task did — is what makes the queue a record rather than a guess. It takes
// the same flags as add, so the flags that are left out are the defaults of a
// new target, not the values the target had.
func mirrorQueueUpdate(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 1 {
		return usagef("mirror queue update takes the name of the target")
	}
	target, err := mirrorQueueDeclare(o, f, q, "mirror queue update", args[0])
	if err != nil {
		return err
	}

	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	var kept *mirrorqueue.Target
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		var err error
		kept, err = queue.UpdateTarget(target)
		return err
	}); err != nil {
		return err
	}
	// The server decides on the branches and the URL it holds for the target,
	// so a target that is published under new ones is registered again.
	if err := mirrorQueueRegisterTargets(ctx, o, kept); err != nil {
		return err
	}
	if o.jsonOut {
		return mirrorQueueWriteJSON(out, mirrorQueueTargetJSON(kept))
	}
	fmt.Fprintf(out, "updated the mirror target %s: %s -> %s (branches: %s, every %s, %s)\n",
		kept.Name, kept.Repository, mirrorDisplayURL(kept.URL),
		strings.Join(kept.Branches, ", "), mirrorQueueSchedule(kept),
		mirrorQueueEnabled(kept.Enabled))
	return nil
}

// mirrorQueueEnabled says whether the target is published on its schedule.
func mirrorQueueEnabled(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

// mirrorQueueDeclare builds the declaration of a target from the flags of add
// and update. usage names the command in a refusal, so that the operator
// reads which one refused.
func mirrorQueueDeclare(o *options, f *mirrorFlags, q *mirrorQueueFlags, usage, name string) (*mirrorqueue.Target, error) {
	if f.repo == "" {
		return nil, usagef("%s requires --repo REPO", usage)
	}
	if f.url == "" {
		return nil, usagef("%s requires --url URL", usage)
	}
	if len(f.branches) == 0 {
		return nil, usagef("%s requires --branch PATTERN: name the branches the mirror may publish", usage)
	}
	if f.sshKey != "" && f.tokenEnv != "" {
		return nil, usagef("%s takes either --token-env or --ssh-key, not both", usage)
	}
	interval := q.interval
	switch {
	case q.manual && interval >= 0:
		return nil, usagef("%s takes either --interval or --manual, not both", usage)
	case q.manual:
		interval = 0
	case interval < 0:
		interval = defaultQueueInterval
	}
	idleInterval := q.idleInterval
	if idleInterval < 0 {
		idleInterval = 0
	}
	if idleInterval > 0 && interval > 0 && idleInterval < interval {
		return nil, usagef("%s: the idle interval %s is shorter than the interval %s", usage, idleInterval, interval)
	}
	failureInterval := q.failureInterval
	if failureInterval < 0 {
		failureInterval = 0
	}
	if failureInterval > 0 && interval > 0 && failureInterval < interval {
		return nil, usagef("%s: the failure interval %s is shorter than the interval %s", usage, failureInterval, interval)
	}
	credential := mirrorAuth{
		usage:    usage,
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
		return nil, usagef("%s refuses to send %s over http://%s: use HTTPS, or pass --allow-insecure-http", usage, name, strings.TrimPrefix(f.url, "http://"))
	}

	target := &mirrorqueue.Target{
		Name:              name,
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
		IdleInterval:      idleInterval,
		FailureInterval:   failureInterval,
	}
	// A target with a schedule is due right away, so that the queue publishes
	// it without waiting for the next tick of the worker.
	if target.Interval > 0 {
		target.NextRun = time.Now()
	}
	return target, nil
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
	// The server stops counting the target, and tolerates a target it never
	// knew: the queue of a client that was never registered is removed the
	// same way.
	if err := mirrorQueueUnregisterTarget(ctx, o, removed); err != nil {
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
			mirrorQueueSchedule(t), next, last)
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
	fmt.Fprintf(out, "every: %s\n", mirrorQueueSchedule(target))
	fmt.Fprintf(out, "enabled: %s\n", map[bool]string{true: "yes", false: "no"}[target.Enabled])
	if target.QuietRuns > 0 {
		fmt.Fprintf(out, "quiet runs: %d\n", target.QuietRuns)
	}
	if target.Failures > 0 {
		fmt.Fprintf(out, "failed runs: %d\n", target.Failures)
	}
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

// mirrorQueueIdle sets the idle interval of the targets of the queue: the
// longest a target waits between two publications while its repository does
// not change. It is the operator's way of moving a queue that was written
// before the idle interval existed, and it leaves everything else of a target
// alone — its declaration, its history and its cursor.
func mirrorQueueIdle(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) > 1 {
		return usagef("mirror queue idle takes at most the name of one target")
	}
	if q.idleInterval < 0 && q.failureInterval < 0 {
		return usagef("mirror queue idle requires --idle-interval DURATION, --failure-interval DURATION, or both")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	now := time.Now()
	var changed []*mirrorqueue.Target
	if err := store.Update(func(queue *mirrorqueue.Queue) error {
		for _, target := range queue.Targets {
			if len(args) == 1 && target.Name != args[0] {
				continue
			}
			if q.idleInterval > 0 && target.Interval > 0 && q.idleInterval < target.Interval {
				return fmt.Errorf("the idle interval %s is shorter than the interval %s of the mirror target %q",
					q.idleInterval, target.Interval, target.Name)
			}
			if q.failureInterval > 0 && target.Interval > 0 && q.failureInterval < target.Interval {
				return fmt.Errorf("the failure interval %s is shorter than the interval %s of the mirror target %q",
					q.failureInterval, target.Interval, target.Name)
			}
			if q.idleInterval >= 0 {
				target.IdleInterval = q.idleInterval
			}
			if q.failureInterval >= 0 {
				target.FailureInterval = q.failureInterval
			}
			target.UpdatedAt = now
			changed = append(changed, target)
		}
		return nil
	}); err != nil {
		return err
	}
	if len(changed) == 0 {
		if len(args) == 1 {
			return fmt.Errorf("the mirror queue has no target named %q", args[0])
		}
		fmt.Fprintln(out, "the mirror queue holds no target")
		return nil
	}
	if o.jsonOut {
		list := make([]mirrorQueueTarget, 0, len(changed))
		for _, t := range changed {
			list = append(list, mirrorQueueTargetJSON(t))
		}
		return mirrorQueueWriteJSON(out, list)
	}
	for _, t := range changed {
		fmt.Fprintf(out, "the mirror target %s now waits %s at most between two publications, and %s between two attempts while it fails\n",
			t.Name, pick(mirrorQueueIdleInterval(t), t.Interval.String()), pick(mirrorQueueFailureInterval(t), pick(mirrorQueueIdleInterval(t), t.Interval.String())))
	}
	return nil
}

// mirrorQueueTier sets the idle interval of every target by how long ago the
// repository of the target last changed: a repository that changed today is
// worth looking at often, and a repository nobody has touched in a week is not
// worth a round trip an hour. It is the operator's answer to a queue where one
// interval was asked of every repository, and it reads the same changeset log
// the events come from, so the tiers follow what the server actually recorded.
func mirrorQueueTier(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) > 1 {
		return usagef("mirror queue tier takes at most the name of one target")
	}
	if q.tierActive <= 0 || q.tierRecent <= 0 || q.tierDormant <= 0 {
		return usagef("mirror queue tier requires --active, --recent and --dormant, each a positive duration")
	}
	if q.tierRecentWithin <= q.tierActiveWithin {
		return usagef("mirror queue tier requires --recent-within to be longer than --active-within")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	runner := mirrorQueueRunner(store, o)
	result, err := runner.Tier(ctx, mirrorsweep.TierOptions{
		Target:       firstArg(args),
		Active:       q.tierActive,
		Recent:       q.tierRecent,
		Dormant:      q.tierDormant,
		ActiveWithin: q.tierActiveWithin,
		RecentWithin: q.tierRecentWithin,
	})
	if err != nil {
		return err
	}
	if o.jsonOut {
		list := make([]mirrorQueueTarget, 0, len(result.Changed))
		for _, change := range result.Changed {
			list = append(list, mirrorQueueTargetJSON(change.Target))
		}
		return mirrorQueueWriteJSON(out, list)
	}
	tiers := []string{"active", "recent", "dormant", "kept"}
	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		if result.Counts[tier] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", result.Counts[tier], tier))
		}
	}
	fmt.Fprintf(out, "the mirror queue tiers %d target(s): %s\n", len(result.Changed), strings.Join(parts, ", "))
	for _, change := range result.Changed {
		fmt.Fprintf(out, "  %s (%s): %s at most between two publications\n", change.Target.Name, mirrorsweep.RepoAge(change.Age), mirrorQueueIdleInterval(change.Target))
	}
	return nil
}

func mirrorQueueRun(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) != 0 {
		return usagef("mirror queue run takes no arguments")
	}
	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	runner := mirrorQueueRunner(store, o)
	result, err := runner.Sweep(ctx, mirrorsweep.Options{
		Target:      q.target,
		All:         q.all,
		Concurrency: q.concurrency,
		TaskTimeout: q.taskTimeout,
		NoEvents:    q.noEvents,
	})
	if err != nil {
		return err
	}
	if len(result.Tasks) == 0 {
		if o.jsonOut {
			return mirrorQueueWriteJSON(out, []mirrorQueueTask{})
		}
		if result.Targets == 0 {
			// The file is named by --queue-file, then GRAFT_MIRROR_QUEUE, then
			// the default of the user: a person who repairs a mirror by hand
			// reads which file the run used, so an empty queue is not mistaken
			// for an up-to-date one.
			fmt.Fprintf(out, "the mirror queue %s holds no target, so nothing was published: name the queue of the worker with --queue-file or GRAFT_MIRROR_QUEUE\n", store.Path)
			return nil
		}
		fmt.Fprintf(out, "no mirror target is due in %s\n", store.Path)
		return nil
	}
	return mirrorQueueReport(out, o, result.Tasks)
}

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
				if t.Changed {
					fmt.Fprintf(out, "published %s to %s (%s, on a change)\n", t.Repository, mirrorDisplayURL(t.URL), t.Target)
				} else {
					fmt.Fprintf(out, "published %s to %s (%s, on the schedule)\n", t.Repository, mirrorDisplayURL(t.URL), t.Target)
				}
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
// mirrorQueueFlags holds the flags of the mirror queue commands.
type mirrorQueueFlags struct {
	// interval is how often a target is published.
	interval time.Duration
	// failureInterval is the longest a failing target waits between two
	// attempts. Zero falls back to the idle interval.
	failureInterval time.Duration
	// taskTimeout bounds one publication, so that a mirror that hangs cannot
	// hold the run — and with it the schedule and the events of every other
	// target — for as long as its own client keeps waiting.
	taskTimeout time.Duration
	// tierActive, tierRecent and tierDormant are the idle intervals that
	// mirror queue tier gives a repository by how long ago it last changed,
	// and tierActiveWithin and tierRecentWithin are the ages that divide the
	// three tiers.
	tierActive       time.Duration
	tierRecent       time.Duration
	tierDormant      time.Duration
	tierActiveWithin time.Duration
	tierRecentWithin time.Duration
	// idleInterval is the longest a quiet target waits between two
	// publications. Zero leaves the interval fixed.
	idleInterval time.Duration
	// noEvents keeps a run from reading the changeset logs of the
	// repositories, so that the interval is the only trigger.
	noEvents bool
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

// mirrorQueueRegister records targets of the queue with the server, so that

func mirrorQueueRegister(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) > 1 {
		return usagef("mirror queue register takes at most the name of one target")
	}

	store, err := mirrorQueueStore(q)
	if err != nil {
		return err
	}
	queue, err := store.Load()
	if err != nil {
		return err
	}

	var targets []*mirrorqueue.Target
	if len(args) == 1 {
		target, ok := queue.Target(args[0])
		if !ok {
			return fmt.Errorf("the mirror queue has no target named %q", args[0])
		}
		targets = append(targets, target)
	} else {
		targets = append(targets, queue.Targets...)
	}

	if len(targets) == 0 {
		fmt.Fprintln(out, "the mirror queue has no target to register")
		return nil
	}

	if err := mirrorQueueRegisterTargets(ctx, o, targets...); err != nil {
		return err
	}

	if o.jsonOut {
		list := make([]mirrorQueueTarget, 0, len(targets))
		for _, target := range targets {
			list = append(list, mirrorQueueTargetJSON(target))
		}
		return mirrorQueueWriteJSON(out, list)
	}
	for _, target := range targets {
		fmt.Fprintf(out, "registered the mirror target %s of %s with the server (%s, branches: %s)\n",
			target.Name, target.Repository, mirrorDisplayURL(target.URL), strings.Join(target.Branches, ", "))
	}
	return nil
}

// mirrorQueueRegisterTargets records targets with the server. The targets of
// different servers are recorded with their own server.
func mirrorQueueRegisterTargets(ctx context.Context, o *options, targets ...*mirrorqueue.Target) error {
	for _, target := range targets {
		backend, err := mirrorQueueBackend(ctx, o, target)
		if err != nil {
			return err
		}
		_, err = backend.repo.RegisterMirrorTarget(ctx, sdk.MirrorTargetOptions{
			Name:     target.Name,
			URL:      target.URL,
			Branches: target.Branches,
			Enabled:  target.Enabled,
		})
		closeErr := backend.Close()
		if err != nil {
			return fmt.Errorf("registering the mirror target %q with the server: %w", target.Name, err)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// mirrorQueueUnregisterTarget stops the mirror policy of the server counting
// a target. A server that never knew the target is not an error: the queue of
// a client and the state of a server are repaired separately.
func mirrorQueueUnregisterTarget(ctx context.Context, o *options, tgt *mirrorqueue.Target) error {
	backend, err := mirrorQueueBackend(ctx, o, tgt)
	if err != nil {
		return err
	}
	err = backend.repo.UnregisterMirrorTarget(ctx, tgt.Name)
	closeErr := backend.Close()
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("unregistering the mirror target %q with the server: %w", tgt.Name, err)
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

// mirrorQueueBackend returns the SDK backend of a target: the server it names,
// else the one of the run, and the storage it names, else the one of the run.
func mirrorQueueBackend(ctx context.Context, o *options, tgt *mirrorqueue.Target) (*sdkBackend, error) {
	address := tgt.Address
	if address == "" {
		address = o.address
	}
	if address == "" {
		return nil, fmt.Errorf("the mirror target %q names no server: set GRAFT_ADDRESS or pass -address", tgt.Name)
	}
	return openSDKBackend(ctx, target{
		Address: address,
		Token:   o.token,
		Storage: pick(tgt.Storage, o.storage),
		Repo:    tgt.Repository,
	})
}
