// Package jobs runs the jobs of a graft deployment on Temporal.
//
// Every periodic job of a deployment is a workflow of a Temporal server: the
// sweep of the mirror queue, the tiering of its fallbacks, the backup gate of
// the storage core, the dumps of its database, the gate of the local
// repositories and the backups of the checkouts.
//
// The schedule belongs to the server, the work belongs to a worker, and the
// history of a run is something a person can read: what ran, when, how long
// it took and what it did. A job that fails keeps its failure in the history
// instead of in a log file that nobody reads, and a job that is retried is
// retried by the server rather than by a line of cron.
//
// The package holds the workflows and the activities. The worker that runs
// them is cmd/graft-worker.
package jobs

import (
	"context"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// DefaultNamespace is the namespace the jobs of a graft deployment live
	// in. It is not the default namespace of the server: the jobs of graft
	// are graft's, and the server may serve others.
	DefaultNamespace = "graft"
	// DefaultTaskQueue is the queue a worker of a node polls.
	DefaultTaskQueue = "graft-jobs"
	// DefaultWorkstationTaskQueue is the queue of the worker that runs the
	// jobs of the workstation: the gate of the local repositories and the
	// backups of the checkouts. The jobs of a workstation run on the
	// workstation, because the files they work on are there.
	DefaultWorkstationTaskQueue = "graft-jobs-workstation"
	// DefaultAddress is the Temporal server of a node.
	DefaultAddress = "127.0.0.1:7233"
)

// Options is the configuration of a worker: the server it reaches and the
// files of the node it works on.
type Options struct {
	// Address is the Temporal server.
	Address string
	// Namespace is the namespace the jobs live in.
	Namespace string
	// TaskQueue is the queue the worker polls.
	TaskQueue string

	// Mirror is the mirror queue of the node.
	Mirror MirrorOptions
	// Gate is the backup gate of the storage core.
	Gate GateOptions
	// Local is the gate and the backups of the checkouts of a workstation.
	Local LocalOptions
	// Dump is the database of the node: the storage core itself.
	Dump DumpOptions
	// DumpCI is the database of the CI server of the node.
	DumpCI DumpOptions
}

// MirrorOptions are the queue and the server of the mirror queue of a node.
type MirrorOptions struct {
	// QueueFile is the file the queue is kept in.
	QueueFile string
	// Address is the storage core the queue publishes from.
	Address string
	// Token is the shared token of the operator.
	Token string
	// Storage is the storage the repositories live in.
	Storage string
	// User is the acting user of a write.
	User string
}

// GateOptions are the files of the backup gate of the storage core.
type GateOptions struct {
	// Script is the gate of the node.
	Script string
	// Registry is the file that names the origins of the repositories.
	Registry string
	// ReportDir is the directory the reports of the gate are written to.
	ReportDir string
	// Sync publishes what is missing before the gate verifies.
	Sync bool
}

// LocalOptions are the files of the jobs of a workstation: the gate of the
// local repositories and the backups of the checkouts.
type LocalOptions struct {
	// Script is the gate of the local repositories.
	Script string
	// Roots are the directories the gate walks.
	Roots []string
	// Registry is the file that names the origins of the repositories.
	Registry string
	// ReportDir is the directory the reports of the gate are written to.
	ReportDir string
	// Sync publishes what is missing before the gate verifies.
	Sync bool
	// FullScript backs the checkouts up completely.
	FullScript string
	// IncrementalScript backs the checkouts up since the last one.
	IncrementalScript string
	// FullCron is when the full backup runs.
	FullCron string
	// IncrementalCron is when the incremental backup runs.
	IncrementalCron string
}

// DumpOptions are the database of a node.
type DumpOptions struct {
	// Script is the backup script of the node. It dumps the database, checks
	// that the archive can be read, restores it into a throwaway database
	// and compares the row counts.
	Script string
	// Args are the arguments of the script, without the environment.
	Args []string
	// Cron is when the dump runs, in the cron syntax of the node.
	Cron string
	// Container is the PostgreSQL container the dump is taken from.
	Container string
	// Port is the port PostgreSQL listens on inside the container.
	Port string
	// Database is the database that is dumped.
	Database string
	// Directory is where the dumps are written.
	Directory string
	// Keep is how many dumps of this database are kept.
	Keep int
}

// NewClient returns a client of the Temporal server the options name.
func NewClient(o Options) (client.Client, error) {
	return client.Dial(client.Options{
		HostPort:  o.Address,
		Namespace: o.Namespace,
	})
}

// NewWorker returns a worker that runs the jobs of a node on the queue the
// options name. The workflows and the activities are registered on it, so
// that a worker is the whole job system of a node.
func NewWorker(c client.Client, o Options) worker.Worker {
	w := worker.New(c, o.TaskQueue, worker.Options{})
	RegisterWorkflows(w)
	RegisterActivities(w, Activities{Options: o})
	return w
}

// RegisterWorkflows registers every workflow of the package on a worker.
func RegisterWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(MirrorSweepWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorSweep})
	w.RegisterWorkflowWithOptions(MirrorPublishWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorPublish})
	w.RegisterWorkflowWithOptions(MirrorTierWorkflow, workflow.RegisterOptions{Name: WorkflowMirrorTier})
	w.RegisterWorkflowWithOptions(BackupGateWorkflow, workflow.RegisterOptions{Name: WorkflowBackupGate})
	w.RegisterWorkflowWithOptions(DatabaseDumpWorkflow, workflow.RegisterOptions{Name: WorkflowDatabaseDump})
	w.RegisterWorkflowWithOptions(LocalBackupWorkflow, workflow.RegisterOptions{Name: WorkflowLocalBackup})
}

// RegisterActivities registers every activity of the package on a worker.
func RegisterActivities(w worker.Worker, a Activities) {
	w.RegisterActivity(a.MirrorDueTargets)
	w.RegisterActivity(a.MirrorPublishTarget)
	w.RegisterActivity(a.MirrorTierTargets)
	w.RegisterActivity(a.RunGate)
	w.RegisterActivity(a.DumpDatabase)
	w.RegisterActivity(a.RunScript)
}

// EnsureNamespace registers the namespace of the jobs when the server does not
// have it yet. A server that has it is left alone, so that the operator who
// configured it keeps its settings.
func EnsureNamespace(ctx context.Context, c client.Client, name, description string) error {
	service := c.WorkflowService()
	_, err := service.DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{Namespace: name})
	if err == nil {
		return nil
	}
	_, err = service.RegisterNamespace(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace: name,
		// The retention of a job history is a week: a job that runs every
		// minute leaves a lot of history, and a month of it says nothing that
		// a week does not.
		WorkflowExecutionRetentionPeriod: durationpb.New(7 * 24 * time.Hour),
		Description:                      description,
	})
	return err
}
