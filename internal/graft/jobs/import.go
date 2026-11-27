package jobs

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

// The names of the activities of the import job.
const (
	// ActivityImportRemote imports the state of a remote repository into the
	// storage core.
	ActivityImportRemote = "ImportRemote"
)

// The workflow names of the import job.
const (
	// WorkflowRemoteImport imports the state of a remote repository into the
	// storage core.
	WorkflowRemoteImport = "RemoteImport"
)

// RemoteImportInput asks for the state of a remote repository to be imported
// into the storage core.
//
// The import is the other half of the mirror: the queue publishes the storage
// core to a remote, and the import brings the commits of a remote that moved
// ahead back into the storage core. Without it the queue reports the target
// as diverged and the commits of the remote are only in the remote.
//
// The script of the node does the work: it reads the references the storage
// core holds, builds the bundle of what the remote has and the storage core
// does not, and hands that bundle to the import of the storage core.
type RemoteImportInput struct {
	// Script is the import script of the node.
	Script string `json:"script"`
	// Args are the arguments of the script: the repository and the remote it
	// is imported from.
	Args []string `json:"args,omitempty"`
	// Env holds the variables the script needs. They are not secrets: the
	// script reads its credentials from the files of the node, because a
	// value that rides on the history of a run is a value that everybody who
	// can read the history knows.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds the run.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// RemoteImportWorkflow imports the state of a remote repository into the
// storage core.
//
// The import fails when the script fails: a remote that holds commits the
// storage core does not have is a fact about the backups, and the run that
// says so is the run an operator reads.
func RemoteImportWorkflow(ctx workflow.Context, in RemoteImportInput) (ScriptResult, error) {
	var result ScriptResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: scriptTimeout(in.Timeout),
		RetryPolicy:         scriptRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityImportRemote, ScriptInput{
		Path:    in.Script,
		Args:    in.Args,
		Env:     in.Env,
		Timeout: in.Timeout,
	}).Get(ctx, &result)
	if err != nil {
		return ScriptResult{}, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("the import of the remote failed: %s", tail(result.Output))
	}
	return result, nil
}

// ImportRemote imports the state of a remote repository into the storage core.
func (a Activities) ImportRemote(ctx context.Context, in ScriptInput) (ScriptResult, error) {
	return runScript(ctx, in)
}
