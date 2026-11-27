package mirrorsweep

import (
	"context"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/sdk"
)

// TierOptions are the choices of one tiering.
type TierOptions struct {
	// Target tiers one named target. An empty name tiers every target.
	Target string
	// Active, Recent and Dormant are the idle intervals the three tiers
	// ask for.
	Active  time.Duration
	Recent  time.Duration
	Dormant time.Duration
	// ActiveWithin and RecentWithin are the ages that divide the three
	// tiers.
	ActiveWithin time.Duration
	RecentWithin time.Duration
}

// TierChange is one target whose fallback the tiering changed.
type TierChange struct {
	// Target is the target whose fallback moved.
	Target *mirrorqueue.Target
	// IdleInterval is the fallback the target now has.
	IdleInterval time.Duration
	// Age is how long ago the repository of the target last changed.
	Age time.Duration
	// Tier is the name of the tier the target falls into.
	Tier string
}

// TierResult is what a tiering did.
type TierResult struct {
	// Changed are the targets whose fallback the tiering moved.
	Changed []TierChange
	// Counts holds how many targets each tier holds, and how many kept the
	// fallback they had.
	Counts map[string]int
}

// Validate refuses a tiering that cannot be told apart from another one.
func (o TierOptions) Validate() error {
	if o.Active <= 0 || o.Recent <= 0 || o.Dormant <= 0 {
		return fmt.Errorf("the tiering requires --active, --recent and --dormant, each a positive duration")
	}
	if o.RecentWithin <= o.ActiveWithin {
		return fmt.Errorf("the tiering requires --recent-within to be longer than --active-within")
	}
	return nil
}

// Tier sets the fallback of the targets of the queue by the age of the last
// change of their repository.
//
// A tier never tightens a target the operator declared tighter: a tier whose
// interval is shorter than the declared one leaves the target alone. A
// repository whose log cannot be read keeps the fallback it has.
func (r *Runner) Tier(ctx context.Context, o TierOptions) (TierResult, error) {
	if err := o.Validate(); err != nil {
		return TierResult{}, err
	}
	queue, err := r.Store.Load()
	if err != nil {
		return TierResult{}, err
	}

	// The age of the last change of every repository, read once per repository
	// and shared by its targets.
	clients := map[string]*sdk.Client{}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	ages := map[string]time.Duration{}
	now := r.now()
	for _, t := range queue.Targets {
		if o.Target != "" && t.Name != o.Target {
			continue
		}
		key := r.repoKey(t)
		if _, done := ages[key]; done {
			continue
		}
		address := pick(t.Address, r.Config.Address)
		if address == "" {
			ages[key] = -1
			continue
		}
		client, ok := clients[address]
		if !ok {
			dialed, err := r.dial(ctx, address, t.Storage)
			if err != nil {
				ages[key] = -1
				continue
			}
			client = dialed
			clients[address] = client
		}
		last, err := LastChange(ctx, client.Repo(t.Repository))
		if err != nil || last.IsZero() {
			// A log that cannot be read, or one that holds no entry, says
			// nothing about activity: the target keeps the tier it has.
			ages[key] = -1
			continue
		}
		ages[key] = now.Sub(last)
	}

	result := TierResult{Counts: map[string]int{}}
	var changed []*mirrorqueue.Target
	tiers := map[string]string{}
	if err := r.Store.Update(func(queue *mirrorqueue.Queue) error {
		for _, target := range queue.Targets {
			if o.Target != "" && target.Name != o.Target {
				continue
			}
			key := r.repoKey(target)
			age, known := ages[key]
			if !known || age < 0 {
				result.Counts["kept"]++
				continue
			}
			idle, tier := TierFor(age, o)
			if idle <= target.Interval {
				result.Counts["kept"]++
				continue
			}
			if target.IdleInterval == idle {
				result.Counts[tier]++
				continue
			}
			target.IdleInterval = idle
			target.UpdatedAt = now
			changed = append(changed, target)
			tiers[target.Name] = tier
			result.Counts[tier]++
		}
		return nil
	}); err != nil {
		return TierResult{}, err
	}
	for _, t := range changed {
		result.Changed = append(result.Changed, TierChange{
			Target:       t,
			IdleInterval: t.IdleInterval,
			Age:          ages[r.repoKey(t)],
			Tier:         tiers[t.Name],
		})
	}
	return result, nil
}

// TierFor names the tier of a repository by the age of its last change, and
// the idle interval that tier asks for: the shortest interval that is not
// shorter than the declared one, so that a tier never tightens a target the
// operator set tighter.
func TierFor(age time.Duration, o TierOptions) (time.Duration, string) {
	switch {
	case age <= o.ActiveWithin:
		return o.Active, "active"
	case age <= o.RecentWithin:
		return o.Recent, "recent"
	default:
		return o.Dormant, "dormant"
	}
}

// RepoAge prints an age for a report: the next whole unit down, so that the
// operator reads how old the last change is without a nanosecond.
func RepoAge(age time.Duration) string {
	switch {
	case age < 0:
		return "unknown age"
	case age < time.Minute:
		return "less than a minute"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}
