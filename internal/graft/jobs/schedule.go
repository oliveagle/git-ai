package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// The identifiers of the schedules of the jobs.
const (
	// ScheduleMirrorSweep publishes the mirror targets that are due.
	ScheduleMirrorSweep = "graft-mirror-sweep"
	// ScheduleMirrorTier sets the fallback of the mirror targets.
	ScheduleMirrorTier = "graft-mirror-tier"
	// ScheduleBackupGate asks the backup gate of the storage core whether
	// every repository has an origin that holds its commits.
	ScheduleBackupGate = "graft-backup-gate"
	// ScheduleDatabaseDump dumps the database of the node.
	ScheduleDatabaseDump = "graft-database-dump"
	// ScheduleDatabaseDumpCI dumps the database of the CI server of the node.
	ScheduleDatabaseDumpCI = "graft-database-dump-ci"
	// ScheduleRemoteImport imports the state of a remote repository into the
	// storage core.
	ScheduleRemoteImport = "graft-remote-import"
	// ScheduleLocalBackupGate asks the gate of the local repositories whether
	// every checkout has an origin that holds its commits.
	ScheduleLocalBackupGate = "graft-local-backup-gate"
	// ScheduleLocalBackupFull backs the checkouts up completely.
	ScheduleLocalBackupFull = "graft-local-backup-full"
	// ScheduleLocalBackupIncremental backs the checkouts up since the last
	// one.
	ScheduleLocalBackupIncremental = "graft-local-backup-incremental"
)

// NodeTimeZone is the time zone the cron of a node runs in. The schedules
// keep the minutes of the cron they replace, and a schedule that names a time
// zone runs at the same minute of the wall clock as the cron did.
const NodeTimeZone = "Asia/Shanghai"

// Schedule is one job of a deployment and when it runs.
type Schedule struct {
	// ID is the identifier of the schedule.
	ID string
	// Title is what the operator reads in the list of the schedules.
	Title string
	// Workflow is the name of the workflow the schedule starts.
	Workflow string
	// TaskQueue is the queue the workflow runs on.
	TaskQueue string
	// Cron is the calendar of the schedule, in the cron syntax of the
	// machine. A schedule with an interval has no calendar.
	Cron string
	// Every is how often the schedule starts the workflow.
	Every time.Duration
	// Args are the arguments of the workflow.
	Args []any
	// Paused starts the schedule in a paused state, so that a job that was
	// registered by hand does not run before the operator looked at it.
	Paused bool
}

// Schedules returns the schedules of the jobs a worker of a node runs.
//
// The schedules hold the same minutes as the cron of the node held, in the
// same time zone: a job that ran at 17 minutes past the hour still runs at
// 17 minutes past the hour. The jobs of a node are the jobs of that node, so
// the schedules of a worker are the jobs its configuration names.
func Schedules(o Options) []Schedule {
	var schedules []Schedule
	if o.Mirror.QueueFile != "" {
		schedules = append(schedules,
			Schedule{
				ID:        ScheduleMirrorSweep,
				Title:     "publish the mirror targets that are due",
				Workflow:  WorkflowMirrorSweep,
				TaskQueue: o.TaskQueue,
				Every:     time.Minute,
				Args:      []any{MirrorSweepInput{}},
			},
			Schedule{
				ID:        ScheduleMirrorTier,
				Title:     "set the fallback of the mirror targets by the age of their last change",
				Workflow:  WorkflowMirrorTier,
				TaskQueue: o.TaskQueue,
				Cron:      "23 4 * * *",
				Args: []any{MirrorTierInput{
					Active:       time.Hour,
					Recent:       3 * time.Hour,
					Dormant:      12 * time.Hour,
					ActiveWithin: 24 * time.Hour,
					RecentWithin: 7 * 24 * time.Hour,
				}},
			},
		)
	}
	if o.Gate.Script != "" {
		args := []string{}
		if o.Gate.Sync {
			args = append(args, "--sync")
		}
		if o.Gate.Registry != "" {
			args = append(args, "--registry", o.Gate.Registry)
		}
		schedules = append(schedules, Schedule{
			ID:        ScheduleBackupGate,
			Title:     "ask the backup gate whether every repository has an origin that holds its commits",
			Workflow:  WorkflowBackupGate,
			TaskQueue: o.TaskQueue,
			Cron:      "17 * * * *",
			Args: []any{GateInput{
				Script:       o.Gate.Script,
				Args:         args,
				ReportDir:    o.Gate.ReportDir,
				ReportPrefix: "backup-origins",
				Timeout:      time.Hour,
			}},
		})
	}
	if o.Dump.Script != "" {
		schedules = append(schedules, Schedule{
			ID:        ScheduleDatabaseDump,
			Title:     "dump the database of the node and prove that the dump can be restored",
			Workflow:  WorkflowDatabaseDump,
			TaskQueue: o.TaskQueue,
			Cron:      dumpCron(o.Dump.Cron, "40 3 * * *"),
			Args: []any{DumpInput{
				Script:  o.Dump.Script,
				Args:    o.Dump.Args,
				Timeout: 30 * time.Minute,
				Env:     o.Dump.env(),
			}},
		})
	}
	if o.DumpCI.Script != "" {
		schedules = append(schedules, Schedule{
			ID:        ScheduleDatabaseDumpCI,
			Title:     "dump the database of the CI server and prove that the dump can be restored",
			Workflow:  WorkflowDatabaseDump,
			TaskQueue: o.TaskQueue,
			Cron:      dumpCron(o.DumpCI.Cron, "35 3 * * *"),
			Args: []any{DumpInput{
				Script:  o.DumpCI.Script,
				Args:    o.DumpCI.Args,
				Timeout: 30 * time.Minute,
				Env:     o.DumpCI.env(),
			}},
		})
	}
	if o.Import.Script != "" {
		args := []string{}
		if o.Import.Repository != "" {
			args = append(args, o.Import.Repository)
		}
		if o.Import.Remote != "" {
			args = append(args, o.Import.Remote)
		}
		schedules = append(schedules, Schedule{
			ID:        ScheduleRemoteImport,
			Title:     "import the state of the remote repository into the storage core",
			Workflow:  WorkflowRemoteImport,
			TaskQueue: o.TaskQueue,
			Every:     importEvery(o.Import.Every),
			Args: []any{RemoteImportInput{
				Script:  o.Import.Script,
				Args:    args,
				Timeout: importTimeout(o.Import.Timeout),
			}},
		})
	}
	if o.Local.Script != "" {
		args := []string{}
		for _, root := range o.Local.Roots {
			args = append(args, "--root", root)
		}
		if o.Local.Sync {
			args = append(args, "--sync")
		}
		if o.Local.Registry != "" {
			args = append(args, "--registry", o.Local.Registry)
		}
		schedules = append(schedules, Schedule{
			ID:        ScheduleLocalBackupGate,
			Title:     "ask the gate of the local repositories whether every checkout has an origin that holds its commits",
			Workflow:  WorkflowBackupGate,
			TaskQueue: o.TaskQueue,
			Cron:      "23 * * * *",
			Args: []any{GateInput{
				Script:       o.Local.Script,
				Args:         args,
				ReportDir:    o.Local.ReportDir,
				ReportPrefix: "local-origins",
				Timeout:      time.Hour,
			}},
		})
	}
	if o.Local.FullScript != "" {
		schedules = append(schedules, Schedule{
			ID:        ScheduleLocalBackupFull,
			Title:     "back the checkouts up completely",
			Workflow:  WorkflowLocalBackup,
			TaskQueue: o.TaskQueue,
			Cron:      localCron(o.Local.FullCron, "0 2 * * 0"),
			Args: []any{LocalBackupInput{
				Script:  o.Local.FullScript,
				Timeout: 6 * time.Hour,
			}},
		})
	}
	if o.Local.IncrementalScript != "" {
		schedules = append(schedules, Schedule{
			ID:        ScheduleLocalBackupIncremental,
			Title:     "back the checkouts up since the last backup",
			Workflow:  WorkflowLocalBackup,
			TaskQueue: o.TaskQueue,
			Cron:      localCron(o.Local.IncrementalCron, "0 2 * * 1-6"),
			Args: []any{LocalBackupInput{
				Script:  o.Local.IncrementalScript,
				Timeout: 6 * time.Hour,
			}},
		})
	}
	return schedules
}

// dumpCron is the calendar of a dump, or the one the node used when the
// operator did not name another.
func dumpCron(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// importEvery is how often the import runs, or the half hour the node used
// when the operator did not name another.
func importEvery(every time.Duration) time.Duration {
	if every <= 0 {
		return 30 * time.Minute
	}
	return every
}

// importTimeout bounds one import, or the half hour the node used when the
// operator did not name another.
func importTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 30 * time.Minute
	}
	return timeout
}

// localCron is the calendar of a schedule of the workstation, or the one the
// workstation used when the operator did not name another.
func localCron(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// env returns the variables the dump script of the node needs. They are not
// secrets: the script reads its password from a file of the node.
func (d DumpOptions) env() map[string]string {
	env := map[string]string{}
	if d.Database != "" {
		env["DB"] = d.Database
	}
	if d.Port != "" {
		env["PORT"] = d.Port
	}
	if d.Container != "" {
		env["CONTAINER"] = d.Container
	}
	if d.Directory != "" {
		env["DEST"] = d.Directory
	}
	if d.Keep > 0 {
		env["KEEP"] = fmt.Sprintf("%d", d.Keep)
	}
	return env
}

// Register creates the schedules of the jobs of a node on the server. A
// schedule that is there already is replaced, so that the schedules of the
// server are the schedules the configuration of the node names.
func Register(ctx context.Context, c client.Client, o Options) ([]string, error) {
	var registered []string
	for _, s := range Schedules(o) {
		options := client.ScheduleOptions{
			ID:      s.ID,
			Spec:    scheduleSpec(s),
			Action:  scheduleAction(s),
			Paused:  s.Paused,
			Note:    s.Title,
			Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		}
		handle, err := c.ScheduleClient().Create(ctx, options)
		if err != nil {
			var exists *serviceerror.AlreadyExists
			if !errors.Is(err, temporal.ErrScheduleAlreadyRunning) && !errors.As(err, &exists) {
				return registered, fmt.Errorf("the schedule %s could not be created: %w", s.ID, err)
			}
			// The schedule is on the server already: it is updated instead of
			// created, so that the operator may run this again after a change
			// of the configuration, and the runs the schedule had keep their
			// history.
			handle = c.ScheduleClient().GetHandle(ctx, s.ID)
			if err := handle.Update(ctx, client.ScheduleUpdateOptions{
				DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
					schedule := in.Description.Schedule
					schedule.Spec = &options.Spec
					schedule.Action = options.Action
					if schedule.Policy == nil {
						schedule.Policy = &client.SchedulePolicies{}
					}
					schedule.Policy.Overlap = options.Overlap
					if schedule.State == nil {
						schedule.State = &client.ScheduleState{}
					}
					schedule.State.Paused = options.Paused
					schedule.State.Note = options.Note
					return &client.ScheduleUpdate{Schedule: &schedule}, nil
				},
			}); err != nil {
				return registered, fmt.Errorf("the schedule %s could not be updated: %w", s.ID, err)
			}
		}
		registered = append(registered, s.ID)
		_ = handle
	}
	return registered, nil
}

// Unregister removes the schedules of the jobs of a node from the server. The
// history of the runs stays: a job that is removed is a job that does not
// run, not a job whose history is erased.
func Unregister(ctx context.Context, c client.Client, o Options) ([]string, error) {
	var removed []string
	for _, s := range Schedules(o) {
		handle := c.ScheduleClient().GetHandle(ctx, s.ID)
		if err := handle.Delete(ctx); err != nil {
			continue
		}
		removed = append(removed, s.ID)
	}
	return removed, nil
}

// List returns the schedules of the jobs of a node and when they run next.
func List(ctx context.Context, c client.Client, o Options) ([]ScheduleStatus, error) {
	var statuses []ScheduleStatus
	for _, s := range Schedules(o) {
		status := ScheduleStatus{ID: s.ID, Title: s.Title, Workflow: s.Workflow}
		description, err := c.ScheduleClient().GetHandle(ctx, s.ID).Describe(ctx)
		if err != nil {
			status.State = "absent"
			statuses = append(statuses, status)
			continue
		}
		status.State = "registered"
		status.Paused = description.Schedule.State.Paused
		for _, action := range description.Info.RunningWorkflows {
			status.Running = append(status.Running, action.WorkflowID)
		}
		if len(description.Info.NextActionTimes) > 0 {
			status.Next = description.Info.NextActionTimes[0]
		}
		status.Actions = description.Info.NumActions
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	return statuses, nil
}

// ScheduleStatus is what the operator reads about one schedule.
type ScheduleStatus struct {
	// ID is the identifier of the schedule.
	ID string
	// Title is what the schedule does.
	Title string
	// Workflow is the name of the workflow the schedule starts.
	Workflow string
	// State tells whether the schedule is on the server.
	State string
	// Paused tells whether the schedule is paused.
	Paused bool
	// Next is when the schedule runs next.
	Next time.Time
	// Running holds the runs that are in flight.
	Running []string
	// Actions is how many times the schedule has run.
	Actions int
}

// scheduleSpec is the calendar of a schedule.
func scheduleSpec(s Schedule) client.ScheduleSpec {
	spec := client.ScheduleSpec{TimeZoneName: NodeTimeZone}
	if s.Cron != "" {
		spec.CronExpressions = []string{s.Cron}
	}
	if s.Every > 0 {
		spec.Intervals = []client.ScheduleIntervalSpec{{Every: s.Every}}
	}
	return spec
}

// scheduleAction is what a schedule does when its time comes.
func scheduleAction(s Schedule) client.ScheduleAction {
	return &client.ScheduleWorkflowAction{
		Workflow:  s.Workflow,
		TaskQueue: s.TaskQueue,
		Args:      s.Args,
	}
}

// Run starts one job of a node once, without waiting for its schedule.
//
// It is the job an operator runs by hand: the same workflow, the same
// arguments, and the same history as the runs of the schedule.
func Run(ctx context.Context, c client.Client, o Options, id string) (string, error) {
	for _, s := range Schedules(o) {
		if s.ID != id {
			continue
		}
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			TaskQueue: s.TaskQueue,
			ID:        fmt.Sprintf("%s-%d", s.ID, time.Now().Unix()),
		}, s.Workflow, s.Args...)
		if err != nil {
			return "", fmt.Errorf("the job %s could not be started: %w", id, err)
		}
		return run.GetID(), nil
	}
	return "", fmt.Errorf("no job of this node is named %q", id)
}
