package branchgate

import (
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// Store holds the branch management policy in force, so that a reload puts new
// rules in force without a restart and without a reader of the old policy
// deciding with two of them at once.
//
// One store serves the reference-transaction hook, which enforces the policy,
// and the BranchPolicyService, which answers the CI, exactly as one Policy does
// when the store is never reloaded: both read Current() for the decision they
// make, and the answer a CI reads is made with the rules the hook would decide
// with at that moment.
type Store struct {
	current atomic.Pointer[Policy]
	// number is the generation of the policy in force, 1 at startup.
	number atomic.Uint64
	// reload serializes reloads, so that generations follow each other.
	reload sync.Mutex

	reloads    *prometheus.CounterVec
	generation prometheus.Gauge
}

// Outcome is the result of a Reload.
type Outcome struct {
	// Applied tells whether the rules of the policy changed.
	Applied bool
	// Generation counts the policies put in force: 1 at startup, one more for
	// every reload that changed the rules.
	Generation uint64
}

// NewStore returns a Store whose policy is the one of graft, as generation 1.
func NewStore(graft config.Graft) *Store {
	s := &Store{
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_branch_policy_reloads_total",
			Help: "Reloads of the branch management policy, by result (applied, unchanged, rejected).",
		}, []string{"result"}),
		generation: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_branch_policy_generation",
			Help: "Generation of the branch management policy in force: 1 at startup, one more for every reload that applied new rules.",
		}),
	}
	s.store(NewPolicy(graft), 1)
	return s
}

// Current returns the policy in force. It is never nil.
func (s *Store) Current() *Policy {
	return s.current.Load()
}

// Generation returns the generation of the policy in force: 1 at startup, one
// more for every reload that applied new rules.
func (s *Store) Generation() uint64 {
	return s.number.Load()
}

// Reload puts the branch rules of graft in force, when they are valid. An
// invalid rule is an error and the rules in force stay, so that a typo in the
// configuration file never opens a managed branch.
//
// The approval rules of [graft.review] are not reloaded: they are read by
// other parts of the server at startup, so a change of them still needs a
// restart, and the policy keeps the ones in force.
func (s *Store) Reload(graft config.Graft) (Outcome, error) {
	s.reload.Lock()
	defer s.reload.Unlock()

	previous := s.Current()
	if err := graft.ValidateBranches(); err != nil {
		s.reloads.WithLabelValues("rejected").Inc()
		return Outcome{Generation: s.number.Load()}, err
	}

	rules := append([]config.GraftBranchRule(nil), graft.Branches...)
	if sameRules(previous.Rules, rules) {
		s.reloads.WithLabelValues("unchanged").Inc()
		return Outcome{Generation: s.number.Load()}, nil
	}

	generation := s.number.Load() + 1
	s.store(&Policy{Rules: rules, ReviewRules: previous.ReviewRules}, generation)
	s.reloads.WithLabelValues("applied").Inc()
	return Outcome{Applied: true, Generation: generation}, nil
}

// store puts policy in force as generation.
func (s *Store) store(policy *Policy, generation uint64) {
	s.current.Store(policy)
	s.number.Store(generation)
	s.generation.Set(float64(generation))
}

// sameRules tells whether two rules govern the branches the same way: the same
// branches in the same order, each with the same policy.
func sameRules(a, b []config.GraftBranchRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		// The rule holds a slice of the suites it requires, so the rules are
		// compared field by field, not with the == operator.
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Describe implements prometheus.Collector.
func (s *Store) Describe(descs chan<- *prometheus.Desc) {
	s.reloads.Describe(descs)
	s.generation.Describe(descs)
}

// Collect implements prometheus.Collector.
func (s *Store) Collect(metrics chan<- prometheus.Metric) {
	s.reloads.Collect(metrics)
	s.generation.Collect(metrics)
}
