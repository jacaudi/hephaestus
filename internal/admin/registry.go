package admin

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

const (
	defaultInterval = 15 * time.Second
	checkTimeout    = 5 * time.Second
)

// Status is the outcome of one health check run.
type Status string

// The check outcomes, named as the health+json draft names them.
const (
	// StatusPass means the check succeeded.
	StatusPass Status = "pass"
	// StatusWarn means the check succeeded in a degraded way.
	StatusWarn Status = "warn"
	// StatusFail means the check failed.
	StatusFail Status = "fail"
)

// Result is what one run of a Check observed. ObservedValue, ObservedUnit
// and Output are optional.
type Result struct {
	Status        Status
	ObservedValue any
	ObservedUnit  string
	Output        string
}

// Check is one named health check. A Critical check that is not passing
// takes the service out of rotation; an informational one only degrades
// /health.
type Check struct {
	Name     string
	Critical bool
	// Interval is the time between runs; zero means 15 seconds.
	Interval time.Duration
	// Run performs the check. Each run gets a context that expires after
	// 5 seconds.
	Run func(context.Context) Result
}

// observation is the latest Result of a Check and when it was recorded.
type observation struct {
	check  Check
	result Result
	at     time.Time // zero until the check has run
}

// Registry runs Checks and remembers each one's latest Result.
type Registry struct {
	checks []Check

	mu     sync.Mutex
	latest map[string]*observation
}

// NewRegistry returns a Registry over checks. No check has run yet.
func NewRegistry(checks ...Check) *Registry {
	return &Registry{checks: checks, latest: make(map[string]*observation, len(checks))}
}

// RunOnce runs every check once, one after another, and returns when all
// have finished.
func (r *Registry) RunOnce(ctx context.Context) {
	for _, c := range r.checks {
		r.runCheck(ctx, c)
	}
}

// Run runs each check on its own ticker until ctx is done, then returns once
// every check goroutine has exited. A check's first scheduled run happens one
// Interval after Run starts; call RunOnce first to have results sooner.
func (r *Registry) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range r.checks {
		wg.Go(func() {
			t := time.NewTicker(cmp.Or(max(c.Interval, 0), defaultInterval))
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					r.runCheck(ctx, c)
				}
			}
		})
	}
	wg.Wait()
}

func (r *Registry) runCheck(ctx context.Context, c Check) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	res := c.Run(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[c.Name] = &observation{check: c, result: res, at: time.Now().UTC()}
}

// snapshot returns the latest observation of every check, sorted by name.
// Recorded observations are never mutated. A check that has not run yet reads
// as a failing "pending" result with a zero time, so every consumer treats it
// as not passing.
func (r *Registry) snapshot() []*observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*observation, 0, len(r.checks))
	for _, c := range r.checks {
		o, ran := r.latest[c.Name]
		if !ran {
			o = &observation{check: c, result: Result{Status: StatusFail, Output: "pending"}}
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b *observation) int { return cmp.Compare(a.check.Name, b.check.Name) })
	return out
}
