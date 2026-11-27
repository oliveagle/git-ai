package jobs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
)

// The tests of the workflows run the workflows themselves: the history of a
// run is what an operator reads, so the shape of the history is part of the
// contract and is worth a test.

func TestMirrorSweepPublishesEveryDueTarget(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(MirrorSweepWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorSweep})
	env.RegisterWorkflowWithOptions(MirrorPublishWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorPublish})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityMirrorDueTargets, mock.Anything, mock.Anything).Return(DueTargets{
		Targets: []mirrorsweep.DueTarget{
			{Name: "shop", Repository: "shop.git", URL: "git@github.com:oliveagle/shop.git"},
			{Name: "books", Repository: "books.git", URL: "git@github.com:oliveagle/books.git"},
		},
	}, nil)
	env.OnActivity(ActivityMirrorPublishTarget, mock.Anything, targetIs("shop")).Return(
		MirrorPublishResult{Target: "shop", Status: mirrorqueue.TaskOK}, nil)
	env.OnActivity(ActivityMirrorPublishTarget, mock.Anything, targetIs("books")).Return(
		MirrorPublishResult{Target: "books", Status: mirrorqueue.TaskDiverged}, nil)

	env.ExecuteWorkflow(MirrorSweepWorkflow, MirrorSweepInput{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MirrorSweepResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 2, result.Due)
	require.Equal(t, 1, result.Published)
	require.Equal(t, 1, result.Diverged)
	require.Equal(t, 0, result.Failed)
	require.Len(t, result.Tasks, 2)
}

func TestMirrorSweepKeepsTheFailureOfOneTargetAndGoesOn(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(MirrorSweepWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorSweep})
	env.RegisterWorkflowWithOptions(MirrorPublishWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorPublish})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityMirrorDueTargets, mock.Anything, mock.Anything).Return(DueTargets{
		Targets: []mirrorsweep.DueTarget{
			{Name: "shop", Repository: "shop.git"},
			{Name: "books", Repository: "books.git"},
		},
	}, nil)
	env.OnActivity(ActivityMirrorPublishTarget, mock.Anything, targetIs("shop")).Return(
		nil, context.DeadlineExceeded)
	env.OnActivity(ActivityMirrorPublishTarget, mock.Anything, targetIs("books")).Return(
		MirrorPublishResult{Target: "books", Status: mirrorqueue.TaskOK}, nil)

	env.ExecuteWorkflow(MirrorSweepWorkflow, MirrorSweepInput{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MirrorSweepResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 1, result.Published)
	require.Equal(t, 1, result.Failed)
}

func TestMirrorSweepWithNothingDueDoesNothing(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(MirrorSweepWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorSweep})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityMirrorDueTargets, mock.Anything, mock.Anything).Return(DueTargets{}, nil)

	env.ExecuteWorkflow(MirrorSweepWorkflow, MirrorSweepInput{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MirrorSweepResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 0, result.Due)
	require.Empty(t, result.Tasks)
}

func TestBackupGateWritesTheReportAndSucceeds(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(BackupGateWorkflow, workflow.RegisterOptions{Name: WorkflowBackupGate})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	var seen ScriptInput
	env.OnActivity(ActivityRunGate, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in ScriptInput) (ScriptResult, error) {
			seen = in
			return ScriptResult{ExitCode: 0, Output: "every repository has an origin"}, nil
		})

	env.ExecuteWorkflow(BackupGateWorkflow, GateInput{
		Script:       "/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh",
		Args:         []string{"--sync"},
		ReportDir:    "/opt/git-ai/reports",
		ReportPrefix: "backup-origins",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result GateResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 0, result.ExitCode)
	require.Contains(t, result.Report, "backup-origins-")
	require.True(t, strings.HasSuffix(result.Report, ".json"))
	require.Equal(t, "/opt/git-ai/reports", filepath.Dir(result.Report))

	// The gate is asked to write the report it was promised.
	require.Equal(t, "/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh", seen.Path)
	require.Equal(t, "--sync", seen.Args[0])
	require.Equal(t, "--report", seen.Args[1])
	require.Equal(t, result.Report, seen.Args[2])
}

func TestBackupGateFailsWhenTheOriginIsNotInSync(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(BackupGateWorkflow, workflow.RegisterOptions{Name: WorkflowBackupGate})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityRunGate, mock.Anything, mock.Anything).Return(ScriptResult{
		ExitCode: 3,
		Output:   "shop.git: the origin github does not carry refs/heads/main",
	}, nil)

	env.ExecuteWorkflow(BackupGateWorkflow, GateInput{
		Script:    "/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh",
		ReportDir: "/opt/git-ai/reports",
	})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "the backup gate reported a failure")
	require.Contains(t, err.Error(), "shop.git")
}

func TestDatabaseDumpFailsWhenTheScriptFails(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(DatabaseDumpWorkflow, workflow.RegisterOptions{Name: WorkflowDatabaseDump})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityDumpDatabase, mock.Anything, mock.Anything).Return(ScriptResult{
		ExitCode: 2,
		Output:   "pg_dump: error: connection to the database failed",
	}, nil)

	env.ExecuteWorkflow(DatabaseDumpWorkflow, DumpInput{
		Script: "/home/oliveagle/bin/pg_backup.sh",
		Env:    map[string]string{"DB": "gitai"},
	})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "the dump of the database failed")
}

func TestDatabaseDumpSucceedsAndCarriesTheEnvironment(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(DatabaseDumpWorkflow, workflow.RegisterOptions{Name: WorkflowDatabaseDump})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	var seen ScriptInput
	env.OnActivity(ActivityDumpDatabase, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in ScriptInput) (ScriptResult, error) {
			seen = in
			return ScriptResult{ExitCode: 0, Output: "the dump was restored"}, nil
		})

	env.ExecuteWorkflow(DatabaseDumpWorkflow, DumpInput{
		Script: "/home/oliveagle/bin/pg_backup.sh",
		Env:    map[string]string{"DB": "gitai", "KEEP": "7"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result ScriptResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, "gitai", seen.Env["DB"])
}

func TestLocalBackupFailsWhenTheCheckoutIsNotBackedUp(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(LocalBackupWorkflow, workflow.RegisterOptions{Name: WorkflowLocalBackup})
	env.RegisterActivity(Activities{}.MirrorDueTargets)
	env.RegisterActivity(Activities{}.MirrorPublishTarget)
	env.RegisterActivity(Activities{}.MirrorTierTargets)
	env.RegisterActivity(Activities{}.RunGate)
	env.RegisterActivity(Activities{}.DumpDatabase)
	env.RegisterActivity(Activities{}.RunScript)

	env.OnActivity(ActivityRunScript, mock.Anything, mock.Anything).Return(ScriptResult{
		ExitCode: 1,
		Output:   "the origin does not carry the commit",
	}, nil)

	env.ExecuteWorkflow(LocalBackupWorkflow, LocalBackupInput{
		Script: "/opt/git-ai/deploy/host-scripts/local-backup-gate.sh",
	})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "the backup of the checkouts failed")
}

func TestTheReportOfAGateIsNamedAfterTheRun(t *testing.T) {
	at := time.Date(2026, 11, 27, 3, 17, 5, 0, time.UTC)
	path := gateReportPath(GateInput{ReportDir: "/opt/git-ai/reports", ReportPrefix: "backup-origins"}, at)
	require.Equal(t, "/opt/git-ai/reports/backup-origins-20261127T031705Z.json", path)
}

// targetIs matches the input of a publication of one target.
func targetIs(name string) interface{} {
	return mock.MatchedBy(func(in MirrorPublishInput) bool { return in.Target == name })
}

func TestTheSchedulesKeepTheMinutesOfTheCronTheyReplace(t *testing.T) {
	schedules := Schedules(Options{
		TaskQueue: "graft-jobs",
		Mirror:    MirrorOptions{QueueFile: "/opt/git-ai/mirror/mirror-queue.json"},
		Gate:      GateOptions{Script: "/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh", Sync: true},
		Dump:      DumpOptions{Script: "/opt/git-ai/deploy/host-scripts/pg-dump.sh", Args: []string{"--lane", "prod"}},
		DumpCI:    DumpOptions{Script: "/home/oliveagle/bin/pg_backup.sh", Database: "woodpecker"},
	})
	byID := map[string]Schedule{}
	for _, s := range schedules {
		byID[s.ID] = s
	}

	// The cron of the node ran the sweep every minute, the gate at 17
	// minutes past the hour, the dump of the storage core at 40 and the dump
	// of the CI database at 35.
	require.Equal(t, time.Minute, byID[ScheduleMirrorSweep].Every)
	require.Equal(t, "17 * * * *", byID[ScheduleBackupGate].Cron)
	require.Equal(t, "40 3 * * *", byID[ScheduleDatabaseDump].Cron)
	require.Equal(t, "35 3 * * *", byID[ScheduleDatabaseDumpCI].Cron)
	require.Equal(t, "23 4 * * *", byID[ScheduleMirrorTier].Cron)

	// The gate publishes what is missing before it verifies.
	gate := byID[ScheduleBackupGate].Args[0].(GateInput)
	require.Equal(t, []string{"--sync"}, gate.Args)

	// The dump of the storage core names the lane it dumps.
	dump := byID[ScheduleDatabaseDump].Args[0].(DumpInput)
	require.Equal(t, []string{"--lane", "prod"}, dump.Args)

	// Every job of the node runs on the queue of the node.
	for _, s := range schedules {
		require.Equal(t, "graft-jobs", s.TaskQueue)
		require.NotEmpty(t, s.Workflow)
	}
}

// The jobs of a workstation are on the calendar too, with the minutes the
// cron of the node used: the gate of the local repositories at 23 minutes
// past the hour, the full backup on Sunday and the incremental one on the
// other days, both at two in the morning.
func TestTheSchedulesOfAWorkstationKeepTheMinutesOfTheCronTheyReplace(t *testing.T) {
	schedules := Schedules(Options{
		TaskQueue: "graft-jobs-workstation",
		Local: LocalOptions{
			Script:            "/home/oliveagle/bin/local-backup-gate.sh",
			Roots:             []string{"/mnt/data/repos/github.com/oliveagle"},
			Sync:              true,
			FullScript:        "/home/oliveagle/bin/backup-full.sh",
			IncrementalScript: "/home/oliveagle/bin/backup-incremental.sh",
		},
	})
	byID := map[string]Schedule{}
	for _, s := range schedules {
		byID[s.ID] = s
	}

	require.Equal(t, "23 * * * *", byID[ScheduleLocalBackupGate].Cron)
	require.Equal(t, "0 2 * * 0", byID[ScheduleLocalBackupFull].Cron)
	require.Equal(t, "0 2 * * 1-6", byID[ScheduleLocalBackupIncremental].Cron)

	// The gate of the checkouts publishes what is missing before it verifies,
	// and it names the roots it walks.
	gate := byID[ScheduleLocalBackupGate].Args[0].(GateInput)
	require.Contains(t, gate.Args, "--sync")
	require.Contains(t, gate.Args, "/mnt/data/repos/github.com/oliveagle")
	require.Equal(t, "local-origins", gate.ReportPrefix)

	// The two backups run the script of the node, and every job of the
	// workstation runs on the queue of the workstation.
	require.Equal(t, "/home/oliveagle/bin/backup-full.sh",
		byID[ScheduleLocalBackupFull].Args[0].(LocalBackupInput).Script)
	require.Equal(t, "/home/oliveagle/bin/backup-incremental.sh",
		byID[ScheduleLocalBackupIncremental].Args[0].(LocalBackupInput).Script)
	for _, s := range schedules {
		require.Equal(t, "graft-jobs-workstation", s.TaskQueue)
		require.NotEmpty(t, s.Workflow)
	}
}

// A node that holds no jobs holds no schedule: the calendar of the server
// names only the jobs a worker of that node can run.
func TestANodeWithoutJobsHasNoSchedule(t *testing.T) {
	require.Empty(t, Schedules(Options{TaskQueue: "graft-jobs"}))
}
