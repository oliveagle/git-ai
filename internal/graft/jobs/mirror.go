package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The names of the activities of the mirror jobs. A name is what the history
// of a run shows, so it says what the activity did.
const (
	// ActivityMirrorDueTargets asks the queue which targets are due.
	ActivityMirrorDueTargets = "MirrorDueTargets"
	// ActivityMirrorPublishTarget publishes one target of the queue.
	ActivityMirrorPublishTarget = "MirrorPublishTarget"
	// ActivityMirrorTierTargets sets the fallback of the targets by the age
	// of the last change of their repository.
	ActivityMirrorTierTargets = "MirrorTierTargets"
)

// The workflow names of the mirror jobs.
const (
	// WorkflowMirrorSweep publishes the targets that are due.
	WorkflowMirrorSweep = "MirrorSweep"
	// WorkflowMirrorPublish publishes one target.
	WorkflowMirrorPublish = "MirrorPublish"
	// WorkflowMirrorTier sets the fallback of the targets.
	WorkflowMirrorTier = "MirrorTier"
)

// MirrorSweepInput asks for the targets that are due to be published.
type MirrorSweepInput struct{}

// MirrorSweepResult is what a sweep did.
type MirrorSweepResult struct {
	// Due is how many targets were due.
	Due int `json:"due"`
	// Published is how many targets published their mirror.
	Published int `json:"published"`
	// Diverged is how many targets kept a remote branch with other history.
	Diverged int `json:"diverged"`
	// Failed is how many targets could not publish.
	Failed int `json:"failed"`
	// Tasks holds what every publication did.
	Tasks []MirrorPublishResult `json:"tasks,omitempty"`
}

// MirrorPublishInput asks for one target to be published.
type MirrorPublishInput struct {
	// Target is the name of the target of the queue.
	Target string `json:"target"`
	// Changed tells whether the changeset log of the repository moved past
	// the cursor of the queue.
	Changed bool `json:"changed,omitempty"`
	// Observed tells whether the changeset log could be read.
	Observed bool `json:"observed,omitempty"`
}

// MirrorPublishResult is what one publication did.
type MirrorPublishResult struct {
	// Target is the name of the target.
	Target string `json:"target"`
	// Repository is the graft repository that was published.
	Repository string `json:"repository"`
	// URL is the Git URL of the mirror.
	URL string `json:"url"`
	// Status is the outcome: ok, diverged or failed.
	Status string `json:"status"`
	// DivergentRefs are the remote branches the mirror kept because they
	// hold history the graft branch does not have.
	DivergentRefs []string `json:"divergentRefs,omitempty"`
	// Error is the reason of a failure.
	Error string `json:"error,omitempty"`
	// Changed tells whether the publication was asked for by a change of
	// the repository rather than by the interval.
	Changed bool `json:"changed,omitempty"`
}

// MirrorTierInput asks for the fallbacks of the targets to be set.
type MirrorTierInput struct {
	// Target sets the fallback of one target. Empty sets it of every
	// target.
	Target string `json:"target,omitempty"`
	// Active, Recent and Dormant are the fallbacks the three tiers ask for.
	Active  time.Duration `json:"active"`
	Recent  time.Duration `json:"recent"`
	Dormant time.Duration `json:"dormant"`
	// ActiveWithin and RecentWithin are the ages that divide the tiers.
	ActiveWithin time.Duration `json:"activeWithin"`
	RecentWithin time.Duration `json:"recentWithin"`
}

// MirrorTierResult is what a tiering did.
type MirrorTierResult struct {
	// Changed is how many targets the tiering moved.
	Changed int `json:"changed"`
	// Counts holds how many targets each tier holds, and how many kept the
	// fallback they had.
	Counts map[string]int `json:"counts,omitempty"`
	// Targets holds the targets the tiering moved.
	Targets []MirrorTierChange `json:"targets,omitempty"`
}

// MirrorTierChange is one target whose fallback the tiering moved.
type MirrorTierChange struct {
	// Target is the name of the target.
	Target string `json:"target"`
	// IdleInterval is the fallback the target now has.
	IdleInterval time.Duration `json:"idleInterval"`
	// Age is how long ago the repository of the target last changed.
	Age time.Duration `json:"age"`
	// Tier is the name of the tier the target falls into.
	Tier string `json:"tier"`
}

// MirrorSweepWorkflow publishes the targets of the mirror queue that are due.
//
// The sweep is the job of the minute. It asks the queue which targets are
// due, and it publishes each of them in a child workflow of its own: a
// mirror that cannot be reached holds its own history and nothing else, and
// the targets that can be reached are published while it tries.
func MirrorSweepWorkflow(ctx workflow.Context, in MirrorSweepInput) (MirrorSweepResult, error) {
	log := workflow.GetLogger(ctx)
	var due DueTargets
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: mirrorQueueTimeout,
		RetryPolicy:         mirrorPublishRetryPolicy(),
	})
	if err := workflow.ExecuteActivity(activityCtx, ActivityMirrorDueTargets, in).Get(ctx, &due); err != nil {
		return MirrorSweepResult{}, err
	}
	result := MirrorSweepResult{Due: len(due.Targets)}
	if len(due.Targets) == 0 {
		return result, nil
	}

	// Every publication is a child workflow, so that the history of a sweep
	// holds the history of every mirror it published.
	runID := workflow.GetInfo(ctx).WorkflowExecution.RunID
	futures := make([]workflow.Future, 0, len(due.Targets))
	for _, target := range due.Targets {
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:        fmt.Sprintf("mirror-publish-%s-%s", target.Name, runID),
			ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_TERMINATE,
		})
		futures = append(futures, workflow.ExecuteChildWorkflow(childCtx, MirrorPublishWorkflow,
			MirrorPublishInput{
				Target:   target.Name,
				Changed:  target.Changed,
				Observed: target.Observed,
			},
		))
	}

	for i, future := range futures {
		var published MirrorPublishResult
		if err := future.Get(ctx, &published); err != nil {
			// The publication itself failed: the target keeps the failure in
			// the history of the queue, and the sweep goes on with the other
			// targets.
			log.Error("a mirror target did not publish", "target", due.Targets[i].Name, "error", err)
			result.Failed++
			result.Tasks = append(result.Tasks, MirrorPublishResult{
				Target:     due.Targets[i].Name,
				Repository: due.Targets[i].Repository,
				URL:        due.Targets[i].URL,
				Status:     mirrorqueue.TaskFailed,
				Error:      err.Error(),
			})
			continue
		}
		switch published.Status {
		case mirrorqueue.TaskOK:
			result.Published++
		case mirrorqueue.TaskDiverged:
			result.Diverged++
		default:
			result.Failed++
		}
		result.Tasks = append(result.Tasks, published)
	}
	return result, nil
}

// MirrorPublishWorkflow publishes one target of the mirror queue.
//
// The publication is a reconciliation: it compares the references of the
// mirror with the ones of the storage core again, and it succeeds when the
// selected tips are on the mirror. That makes a retry safe, and the activity
// is retried when the host it reached is busy.
func MirrorPublishWorkflow(ctx workflow.Context, in MirrorPublishInput) (MirrorPublishResult, error) {
	var result MirrorPublishResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: mirrorPublishTimeout,
		RetryPolicy:         mirrorPublishRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityMirrorPublishTarget, in).Get(ctx, &result)
	return result, err
}

// MirrorTierWorkflow sets the fallback of the targets of the mirror queue by
// the age of the last change of their repository.
//
// It is the job of the day: the queue stretches the interval of a quiet
// target on its own, and the tiering sets the interval the stretch starts
// from.
func MirrorTierWorkflow(ctx workflow.Context, in MirrorTierInput) (MirrorTierResult, error) {
	var result MirrorTierResult
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: mirrorPublishTimeout,
		RetryPolicy:         mirrorPublishRetryPolicy(),
	})
	err := workflow.ExecuteActivity(activityCtx, ActivityMirrorTierTargets, in).Get(ctx, &result)
	return result, err
}

// mirrorPublishTimeout bounds one publication. The queue bounds it as well,
// and the bound of the server is the one that holds when the worker of the
// queue is gone.
const mirrorPublishTimeout = 5 * time.Minute

// mirrorQueueTimeout bounds the reading of the queue: the queue file and the
// changeset logs of the repositories that have one.
const mirrorQueueTimeout = 2 * time.Minute

// mirrorPublishRetryPolicy is how a publication is retried: a mirror that
// cannot be reached is a moment, not a verdict, and three tries with a
// growing gap are enough before the queue takes the failure over.
func mirrorPublishRetryPolicy() *temporal.RetryPolicy {
	return &temporal.RetryPolicy{
		InitialInterval:    15 * time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    2 * time.Minute,
		MaximumAttempts:    3,
	}
}

// DueTargets is what the queue says about its targets. It is the answer of
// the activity that asks the queue, and the input of every publication.
type DueTargets struct {
	// Targets are the targets that are due to be published.
	Targets []mirrorsweep.DueTarget `json:"targets"`
}

// MirrorDueTargets asks the mirror queue which targets are due.
func (a Activities) MirrorDueTargets(ctx context.Context, in MirrorSweepInput) (DueTargets, error) {
	runner, err := a.mirrorRunner()
	if err != nil {
		return DueTargets{}, err
	}
	due, err := runner.Due(ctx)
	if err != nil {
		return DueTargets{}, err
	}
	return DueTargets{Targets: due.Targets}, nil
}

// MirrorPublishTarget publishes one target of the mirror queue and records
// the task of the run in the history of the queue.
func (a Activities) MirrorPublishTarget(ctx context.Context, in MirrorPublishInput) (MirrorPublishResult, error) {
	runner, err := a.mirrorRunner()
	if err != nil {
		return MirrorPublishResult{}, err
	}
	task, err := runner.PublishTarget(ctx, in.Target, in.Changed, in.Observed)
	if err != nil {
		return MirrorPublishResult{}, err
	}
	return MirrorPublishResult{
		Target:        task.Target,
		Repository:    task.Repository,
		URL:           task.URL,
		Status:        task.Status,
		DivergentRefs: task.DivergentRefs,
		Error:         task.Error,
		Changed:       task.Changed,
	}, nil
}

// MirrorTierTargets sets the fallback of the targets of the mirror queue.
func (a Activities) MirrorTierTargets(ctx context.Context, in MirrorTierInput) (MirrorTierResult, error) {
	runner, err := a.mirrorRunner()
	if err != nil {
		return MirrorTierResult{}, err
	}
	result, err := runner.Tier(ctx, mirrorsweep.TierOptions{
		Target:       in.Target,
		Active:       in.Active,
		Recent:       in.Recent,
		Dormant:      in.Dormant,
		ActiveWithin: in.ActiveWithin,
		RecentWithin: in.RecentWithin,
	})
	if err != nil {
		return MirrorTierResult{}, err
	}
	out := MirrorTierResult{Changed: len(result.Changed), Counts: result.Counts}
	for _, change := range result.Changed {
		out.Targets = append(out.Targets, MirrorTierChange{
			Target:       change.Target.Name,
			IdleInterval: change.Target.IdleInterval,
			Age:          change.Age,
			Tier:         change.Tier,
		})
	}
	return out, nil
}

// mirrorRunner returns the worker of the mirror queue of the node.
func (a Activities) mirrorRunner() (*mirrorsweep.Runner, error) {
	if a.Options.Mirror.QueueFile == "" {
		return nil, fmt.Errorf("the worker names no mirror queue: set GRAFT_WORKER_QUEUE_FILE")
	}
	return &mirrorsweep.Runner{
		Store: mirrorqueue.NewFileStore(a.Options.Mirror.QueueFile),
		Config: mirrorsweep.Config{
			Address: a.Options.Mirror.Address,
			Token:   a.Options.Mirror.Token,
			Storage: a.Options.Mirror.Storage,
			User:    a.Options.Mirror.User,
		},
	}, nil
}
