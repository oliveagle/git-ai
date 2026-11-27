package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestRemoteImportSucceedsAndCarriesTheRepository(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(RemoteImportWorkflow, workflow.RegisterOptions{Name: WorkflowRemoteImport})
	env.RegisterActivity(Activities{}.ImportRemote)

	var seen ScriptInput
	env.OnActivity(ActivityImportRemote, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in ScriptInput) (ScriptResult, error) {
			seen = in
			return ScriptResult{ExitCode: 0, Output: "== DONE"}, nil
		})

	env.ExecuteWorkflow(RemoteImportWorkflow, RemoteImportInput{
		Script: "/opt/git-ai/deploy/host-scripts/import-remote.sh",
		Args:   []string{"quant_mt5.git", "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result ScriptResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, "/opt/git-ai/deploy/host-scripts/import-remote.sh", seen.Path)
	require.Equal(t, []string{"quant_mt5.git", "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git"}, seen.Args)
}

func TestRemoteImportFailsWhenTheRemoteIsNotADescendant(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(RemoteImportWorkflow, workflow.RegisterOptions{Name: WorkflowRemoteImport})
	env.RegisterActivity(Activities{}.ImportRemote)

	env.OnActivity(ActivityImportRemote, mock.Anything, mock.Anything).Return(ScriptResult{
		ExitCode: 3,
		Output:   "ERROR: the remote main is not a descendant of the storage head",
	}, nil)

	env.ExecuteWorkflow(RemoteImportWorkflow, RemoteImportInput{
		Script: "/opt/git-ai/deploy/host-scripts/import-remote.sh",
		Args:   []string{"quant_mt5.git"},
	})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Contains(t, err.Error(), "the import of the remote failed")
	require.Contains(t, err.Error(), "not a descendant")
}

func TestTheImportRunsEveryHalfHourWhenTheNodeDoesNotSayOtherwise(t *testing.T) {
	schedules := Schedules(Options{
		TaskQueue: "graft-jobs",
		Import: ImportOptions{
			Script:     "/opt/git-ai/deploy/host-scripts/import-remote.sh",
			Repository: "quant_mt5.git",
			Remote:     "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git",
		},
	})
	var found *Schedule
	for i := range schedules {
		if schedules[i].ID == ScheduleRemoteImport {
			found = &schedules[i]
		}
	}
	require.NotNil(t, found, "the import has to be on the schedule")
	require.Equal(t, 30*time.Minute, found.Every)
	require.Equal(t, WorkflowRemoteImport, found.Workflow)
	require.Equal(t, "graft-jobs", found.TaskQueue)

	in := found.Args[0].(RemoteImportInput)
	require.Equal(t, "/opt/git-ai/deploy/host-scripts/import-remote.sh", in.Script)
	require.Equal(t, []string{"quant_mt5.git", "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git"}, in.Args)
	require.Equal(t, 30*time.Minute, in.Timeout)
}

func TestTheImportIsNotOnTheScheduleOfANodeThatHasNoImport(t *testing.T) {
	schedules := Schedules(Options{TaskQueue: "graft-jobs"})
	for _, s := range schedules {
		require.NotEqual(t, ScheduleRemoteImport, s.ID)
	}
}
