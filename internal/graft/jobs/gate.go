package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The names of the activities of the jobs that run a script of the node.
const (
	// ActivityRunGate runs the backup gate of a node.
	ActivityRunGate = "RunGate"
	// ActivityDumpDatabase dumps a database of a node.
	ActivityDumpDatabase = "DumpDatabase"
	// ActivityRunScript runs a script of a node.
	ActivityRunScript = "RunScript"
)

// The workflow names of the jobs that run a script of the node.
const (
	// WorkflowBackupGate asks the backup gate of the storage core whether
	// every repository has an origin that holds its commits.
	WorkflowBackupGate = "BackupGate"
	// WorkflowDatabaseDump dumps a database of the node and proves that the
	// dump can be restored.
	WorkflowDatabaseDump = "DatabaseDump"
	// WorkflowLocalBackup backs the checkouts of a workstation up.
	WorkflowLocalBackup = "LocalBackup"
)

// GateInput asks for the backup gate of a node to run.
//
// The gate is the script of the node: it reads the authoritative references of
// the storage core, asks every origin what it holds, and writes a report that
// says what it found. The workflow adds the report to the arguments and the
// directory to write it in.
type GateInput struct {
	// Script is the gate of the node.
	Script string `json:"script"`
	// Args are the arguments of the gate, without the report.
	Args []string `json:"args,omitempty"`
	// ReportDir is the directory the report is written to.
	ReportDir string `json:"reportDir"`
	// ReportPrefix is the name the report file starts with. The observer
	// finds the reports by that name.
	ReportPrefix string `json:"reportPrefix"`
	// Env holds the variables the gate needs. They are not secrets: the
	// gate reads its credentials from the files of the node.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds the run.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// GateResult is what the gate did.
type GateResult struct {
	// Report is the file the gate wrote.
	Report string `json:"report"`
	// ExitCode is the status the gate exited with.
	ExitCode int `json:"exitCode"`
	// Output is what the gate printed.
	Output string `json:"output,omitempty"`
	// Duration is how long the gate ran.
	Duration time.Duration `json:"duration"`
}

// DumpInput asks for a database of a node to be dumped.
type DumpInput struct {
	// Script is the backup script of the node.
	Script string `json:"script"`
	// Args are the arguments of the script.
	Args []string `json:"args,omitempty"`
	// Env holds the variables the script needs: the database, the port, the
	// container, where the dump goes and how many dumps are kept. They are
	// not secrets: the script reads its password from a file of the node.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds the run.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// LocalBackupInput asks for the checkouts of a workstation to be backed up.
type LocalBackupInput struct {
	// Script is the backup script of the workstation.
	Script string `json:"script"`
	// Args are the arguments of the script.
	Args []string `json:"args,omitempty"`
	// Env holds the variables the script needs.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds the run.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ScriptResult is what a script did.
type ScriptResult struct {
	// ExitCode is the status the script exited with.
	ExitCode int `json:"exitCode"`
	// Output is what the script printed.
	Output string `json:"output,omitempty"`
	// Duration is how long the script ran.
	Duration time.Duration `json:"duration"`
	// TimedOut tells whether the script was stopped by the timeout.
	TimedOut bool `json:"timedOut,omitempty"`
}

// BackupGateWorkflow asks the backup gate of a node whether every repository
// has an origin that holds its commits.
//
// The gate fails when a repository has no origin, when an origin cannot be
// reached, or when an origin does not carry the commits of the storage core.
// A failure is a fact about the backups, and the workflow reports it as a
// failed run: the report holds the detail, and the operator reads the report
// of the last run of the schedule.
func BackupGateWorkflow(ctx workflow.Context, in GateInput) (GateResult, error) {
	report := gateReportPath(in, workflow.Now(ctx))
	args := append(append([]string{}, in.Args...), "--report", report)
	var result GateResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: scriptTimeout(in.Timeout),
		RetryPolicy:         scriptRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityRunGate, ScriptInput{
		Path:    in.Script,
		Args:    args,
		Env:     in.Env,
		Timeout: in.Timeout,
	}).Get(ctx, &result)
	if err != nil {
		return GateResult{}, err
	}
	result.Report = report
	if result.ExitCode != 0 {
		return result, fmt.Errorf("the backup gate reported a failure: %s", tail(result.Output))
	}
	return result, nil
}

// DatabaseDumpWorkflow dumps a database of a node and proves that the dump
// can be restored.
//
// The script of the node dumps, checks that the archive can be read, restores
// it into a throwaway database and compares the row counts. A run that fails
// any of those steps is a failed run, and the dumps that were good stay.
func DatabaseDumpWorkflow(ctx workflow.Context, in DumpInput) (ScriptResult, error) {
	var result ScriptResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: scriptTimeout(in.Timeout),
		RetryPolicy:         scriptRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityDumpDatabase, ScriptInput{
		Path:    in.Script,
		Args:    in.Args,
		Env:     in.Env,
		Timeout: in.Timeout,
	}).Get(ctx, &result)
	if err != nil {
		return ScriptResult{}, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("the dump of the database failed: %s", tail(result.Output))
	}
	return result, nil
}

// LocalBackupWorkflow backs the checkouts of a workstation up.
//
// The checkouts of a workstation are the operator's own work, and their
// backup is a job of the workstation: the files are there, and the job runs
// where they are.
func LocalBackupWorkflow(ctx workflow.Context, in LocalBackupInput) (ScriptResult, error) {
	var result ScriptResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: scriptTimeout(in.Timeout),
		RetryPolicy:         scriptRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityRunScript, ScriptInput{
		Path:    in.Script,
		Args:    in.Args,
		Env:     in.Env,
		Timeout: in.Timeout,
	}).Get(ctx, &result)
	if err != nil {
		return ScriptResult{}, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("the backup of the checkouts failed: %s", tail(result.Output))
	}
	return result, nil
}

// ScriptInput asks for a script of the node to run.
type ScriptInput struct {
	// Path is the script.
	Path string `json:"path"`
	// Args are the arguments of the script.
	Args []string `json:"args,omitempty"`
	// Env holds the variables the script needs. They are not secrets: the
	// scripts of the node read their credentials from the files of the node,
	// because a value that travels in the history of a run is a value that
	// everybody who can read the history knows.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds the run.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// RunGate runs the backup gate of the node.
func (a Activities) RunGate(ctx context.Context, in ScriptInput) (ScriptResult, error) {
	return runScript(ctx, in)
}

// DumpDatabase dumps a database of the node.
func (a Activities) DumpDatabase(ctx context.Context, in ScriptInput) (ScriptResult, error) {
	return runScript(ctx, in)
}

// RunScript runs a script of the node.
func (a Activities) RunScript(ctx context.Context, in ScriptInput) (ScriptResult, error) {
	return runScript(ctx, in)
}

// runScript runs a script of the node and reports what it did.
//
// A script that fails is a fact, not a crash: the exit code and the output
// are the answer, and the workflow decides what a failure means. A script
// that cannot be started at all is an error, and the server retries it.
func runScript(ctx context.Context, in ScriptInput) (ScriptResult, error) {
	if in.Path == "" {
		return ScriptResult{}, fmt.Errorf("the job names no script to run")
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if in.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, in.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, in.Path, in.Args...)
	cmd.Env = os.Environ()
	for name, value := range in.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	started := time.Now()
	out, err := cmd.CombinedOutput()
	result := ScriptResult{
		Output:   string(out),
		Duration: time.Since(started),
	}
	if runCtx.Err() != nil && ctx.Err() == nil {
		// The timeout of the job stopped the script: the run is over, and the
		// history says so.
		result.TimedOut = true
		return result, fmt.Errorf("the script %s did not finish in %s", in.Path, in.Timeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("the script %s could not be run: %w", in.Path, err)
	}
	return result, nil
}

// gateReportPath is the file the report of a gate run is written to. The name
// holds the time of the run, so that the reports of the node are ordered by
// the time they were taken.
func gateReportPath(in GateInput, at time.Time) string {
	prefix := in.ReportPrefix
	if prefix == "" {
		prefix = "backup-origins"
	}
	name := fmt.Sprintf("%s-%s.json", prefix, at.UTC().Format("20060102T150405Z"))
	if in.ReportDir == "" {
		return name
	}
	return filepath.Join(in.ReportDir, name)
}

// scriptTimeout bounds the activity of a script: the timeout of the script
// itself, and a minute for the machine to notice that it is over.
func scriptTimeout(inner time.Duration) time.Duration {
	if inner <= 0 {
		return time.Hour
	}
	return inner + time.Minute
}

// scriptRetryPolicy is how a script of the node is retried: three tries with
// a growing gap, because a script that could not be started is usually a
// machine that was busy.
func scriptRetryPolicy() *temporal.RetryPolicy {
	return &temporal.RetryPolicy{
		InitialInterval:    30 * time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    5 * time.Minute,
		MaximumAttempts:    3,
	}
}

// tail returns the last lines of an output, so that a failure says what
// failed without holding the whole output of a long run.
func tail(output string) string {
	const lines = 10
	parts := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(parts) <= lines {
		return strings.Join(parts, "\n")
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}
