package clusters

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.jacaudi.dev/hephaestus/internal/admin"
)

const (
	// refreshStaleAfter is how long an approved pair may go without a successful refresh before secrets:cache warns.
	refreshStaleAfter = 48 * time.Hour
	// fetchStaleAfter is how old a cluster's last fetch may be before git:fetch warns.
	fetchStaleAfter = 30 * time.Minute
	// budgetWarn is the count of 1Password requests in 24 hours at which secrets:budget warns: half the daily budget.
	budgetWarn = 500
)

// Checks returns the service's health checks. render:approved is critical: some cluster's approved pair failed its
// last render. The others are informational: secrets:cache, secrets:token, secrets:budget, secrets:client and
// git:fetch. No output quotes a stored error text, a value or a repository URL.
func (s *Service) Checks() []admin.Check {
	return []admin.Check{
		{Name: "secrets:cache", Run: s.overClusters(cacheJudge)},
		{Name: "secrets:token", Run: s.overClusters(s.tokenJudge)},
		{Name: "secrets:budget", Run: s.budgetCheck},
		{Name: "secrets:client", Run: s.clientCheck},
		{Name: "git:fetch", Run: s.overClusters(fetchJudge)},
		{Name: "render:approved", Critical: true, Run: s.overClusters(s.renderJudge)},
	}
}

// overClusters is a check that judges the clusters as stored now; a list that fails fails the check.
func (s *Service) overClusters(judge func(clusters []Cluster, at time.Time) admin.Result) func(context.Context) admin.Result {
	return func(ctx context.Context) admin.Result {
		clusters, err := s.cfg.Repo.List(ctx)
		if err != nil {
			return admin.Result{Status: admin.StatusFail, Output: err.Error()}
		}
		return judge(clusters, s.cfg.Now())
	}
}

// cacheJudge warns for a cluster whose last refresh was incomplete, or whose approved pair has had no successful
// refresh, counted from the import before the first, for refreshStaleAfter.
func cacheJudge(clusters []Cluster, at time.Time) admin.Result {
	var issues []string
	for i := range clusters {
		c := &clusters[i]
		since := c.LastRefreshAt
		if since.IsZero() {
			since = c.CreatedAt
		}
		switch {
		case c.RefreshIncomplete:
			issues = append(issues, c.Name+": the last refresh is incomplete")
		case c.ApprovedPairID != "" && at.Sub(since) >= refreshStaleAfter:
			issues = append(issues, c.Name+": no successful refresh for 2 days")
		}
	}
	return judged(issues, admin.StatusWarn)
}

// tokenJudge warns when clusters exist and no 1Password token is set.
func (s *Service) tokenJudge(clusters []Cluster, _ time.Time) admin.Result {
	if len(clusters) == 0 || s.cfg.Resolver != nil {
		return admin.Result{Status: admin.StatusPass}
	}
	return admin.Result{Status: admin.StatusWarn, Output: errNoToken + ": no cluster's references can be resolved"}
}

// budgetCheck warns when the requests to 1Password in the last 24 hours reach budgetWarn.
func (s *Service) budgetCheck(context.Context) admin.Result {
	if s.cfg.Counter == nil {
		return admin.Result{Status: admin.StatusPass}
	}
	n := s.cfg.Counter.Last24h()
	res := admin.Result{Status: admin.StatusPass, ObservedValue: n, ObservedUnit: "requests"}
	if n >= budgetWarn {
		res.Status, res.Output = admin.StatusWarn, fmt.Sprintf("%d 1Password requests in 24 hours, half the daily budget or more", n)
	}
	return res
}

// clientCheck warns after ClientWarnFailures consecutive whole-call failures of the resolver, naming a restart as the
// recovery when the last one was a panic of the SDK's core. A call that succeeds clears it.
func (s *Service) clientCheck(context.Context) admin.Result {
	s.clientMu.Lock()
	failures, panicked := s.clientFailures, s.clientPanicked
	s.clientMu.Unlock()
	if failures < ClientWarnFailures {
		return admin.Result{Status: admin.StatusPass}
	}
	out := fmt.Sprintf("%d consecutive 1Password calls failed as a whole", failures)
	if panicked {
		out += "; the last one panicked in the SDK's core, which only a restart of hephaestus recovers"
	}
	return admin.Result{Status: admin.StatusWarn, Output: out}
}

// fetchJudge warns for a cluster whose last fetch failed or is fetchStaleAfter old, counted from the import before the
// first, or that has a failed tree. It names the cluster, never the stored error.
func fetchJudge(clusters []Cluster, at time.Time) admin.Result {
	var issues []string
	for i := range clusters {
		c := &clusters[i]
		since := c.LastFetchAt
		if since.IsZero() {
			since = c.CreatedAt
		}
		switch {
		case c.FetchErrorSealed != nil:
			issues = append(issues, c.Name+": the last fetch failed")
		case at.Sub(since) >= fetchStaleAfter:
			issues = append(issues, c.Name+": no fetch for 30 minutes")
		}
		if c.FailedTree != "" {
			issues = append(issues, c.Name+": a tree failed")
		}
	}
	return judged(issues, admin.StatusWarn)
}

// renderJudge fails when the last render of some cluster's current approved pair failed, naming the cluster and host.
// It is keyed on the approved pair, so approving another pair clears it.
func (s *Service) renderJudge(clusters []Cluster, _ time.Time) admin.Result {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	var issues []string
	for i := range clusters {
		c := &clusters[i]
		for _, host := range s.renderFailed[c.ApprovedPairID] {
			if host == "" {
				issues = append(issues, c.Name+": the approved pair's tree is not loaded")
				continue
			}
			issues = append(issues, c.Name+"/"+host+": the approved pair does not render")
		}
	}
	return judged(issues, admin.StatusFail)
}

// judged passes without issues, and otherwise has status with the issues as its output.
func judged(issues []string, status admin.Status) admin.Result {
	if len(issues) == 0 {
		return admin.Result{Status: admin.StatusPass}
	}
	return admin.Result{Status: status, Output: strings.Join(issues, "; ")}
}
