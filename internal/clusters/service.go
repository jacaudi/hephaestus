package clusters

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/secrets"
)

// Resolver resolves references in one call, with a result per reference; any error is a failure of the call as a
// whole. *secrets.OnePassword implements it.
type Resolver interface {
	ResolveAll(ctx context.Context, refs []string) (map[string]secrets.Result, error)
}

const (
	// PollInterval is how often the clusters table is read for new clusters, changed fields and refresh requests.
	PollInterval = 10 * time.Second
	// FetchInterval is how often each cluster's branch is fetched when nothing else asks for it.
	FetchInterval = 5 * time.Minute
	// FetchDeadline bounds one fetch.
	FetchDeadline = 5 * time.Minute
	// ResolveDeadline bounds one call to the resolver.
	ResolveDeadline = 2 * time.Minute
	// DailyInterval is the period of the daily refresh trigger; a rate-limited call waits for its next run.
	DailyInterval = 24 * time.Hour
	// ClientWarnFailures is the count of consecutive whole-call failures at which secrets:client warns.
	ClientWarnFailures = 3
)

const (
	errNoToken        = "HEPHAESTUS_OP_TOKEN is not set" //nolint:gosec // G101: names the token variable, holds no credential
	actionMovedAside  = "cluster.store_moved_aside"
	alertMovedAside   = "git_store_moved_aside"
	outcomeOK         = "ok"
	outcomeError      = "error"
	kindMissingResult = "missing"
	// releaseDeadline bounds the clearing of a held processing mark once Run's context has ended.
	releaseDeadline = 10 * time.Second

	// The stages a report names.
	stageList       = "list"
	stageCredential = "credential" //nolint:gosec // G101: the name of a stage, not a credential
	stageFetch      = "fetch"
	stageRead       = "read"
	stageLoad       = "load"
	stageResolve    = "resolve"
	stageRender     = "render"
	stageStore      = "store"
	stageCache      = "cache"
	stageRelease    = "release"
)

// Config holds what a Service needs.
type Config struct {
	Repo *Repository
	// Resolver is nil when HEPHAESTUS_OP_TOKEN is unset; it must then be a nil interface, not a typed nil pointer.
	Resolver Resolver
	// Counter counts the requests to 1Password; nil is allowed.
	Counter *secrets.Counter
	// GitDir is the directory of the git object store.
	GitDir string
	Now    func() time.Time
	// OnError is told of every failure the service survives. Its errors name the stage, the cluster and, for a
	// refused tree, the file and rule; never a resolver, render or git error's text.
	OnError func(context.Context, error)
	// Meter creates the service's counters; nil is a no-op meter.
	Meter metric.Meter
}

// Service polls the imported clusters, fetches their branches and turns each new head tree into a pending pair. It
// keeps the tree of every approved and pending pair, loaded, in a cache. Startup and then Run are called on one
// goroutine, which owns the poll state and any processing mark the service holds.
type Service struct {
	cfg     Config
	git     *gitStore
	fetches metric.Int64Counter
	alerts  metric.Int64Counter

	// fetched is the last fetch of each cluster this process made.
	fetched map[string]fetchState
	// credentialFailed holds the clusters whose credential reference failed, so it is not resolved again.
	credentialFailed map[string]credentialMark
	// held is the id of the cluster whose processing mark the service holds, "" for none. ClearProcessing does not
	// check whose mark it clears, so only the holder may call it.
	held string

	cacheMu sync.Mutex
	// cache maps a pair id to its loaded definition.
	cache map[string]*render.Cluster

	clientMu sync.Mutex
	// clientFailures counts the consecutive whole-call failures of the resolver; clientPanicked is whether the last
	// one was a panic of the SDK's core.
	clientFailures int
	clientPanicked bool
}

// fetchState is a fetch of a cluster at a revision. tree is the head tree it found, or the one an earlier fetch at
// the same revision found when it failed; "" when there is none.
type fetchState struct {
	revision int64
	at       time.Time
	tree     string
}

// credentialMark is a credential reference that failed at a revision. refresh is whether a refresh request was
// pending then; a new request lets the reference be resolved again.
type credentialMark struct {
	revision int64
	refresh  bool
}

// holds reports whether the mark still stops c's credential from being resolved, and clears refresh once the request
// it saw is gone.
func (m *credentialMark) holds(c *Cluster) bool {
	if m.revision != c.Revision {
		return false
	}
	if !c.RefreshRequested {
		m.refresh = false
		return true
	}
	return m.refresh
}

// cachePair is a pair the cache should hold.
type cachePair struct {
	pairID, clusterID, cluster, tree string
}

// movedDetail is the detail of a cluster.store_moved_aside audit event.
type movedDetail struct {
	Moved string `json:"moved"`
}

// NewService returns a service over cfg. It reports every missing dependency together.
func NewService(cfg Config) (*Service, error) {
	var errs []error
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Repo", cfg.Repo == nil},
		{"GitDir", cfg.GitDir == ""},
		{"Now", cfg.Now == nil},
		{"OnError", cfg.OnError == nil},
	} {
		if d.missing {
			errs = append(errs, fmt.Errorf("clusters: Config.%s is not set", d.name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	meter := cfg.Meter
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	fetches, err := meter.Int64Counter("hephaestus.git.fetches", metric.WithDescription("Git fetches of cluster branches, by outcome."))
	if err != nil {
		return nil, fmt.Errorf("clusters: git fetches counter: %w", err)
	}
	alerts, err := meter.Int64Counter("hephaestus.alerts", metric.WithDescription("Operator alerts raised, by kind."))
	if err != nil {
		return nil, fmt.Errorf("clusters: alerts counter: %w", err)
	}
	return &Service{
		cfg: cfg, git: newGitStore(cfg.GitDir, cfg.Now), fetches: fetches, alerts: alerts,
		fetched: map[string]fetchState{}, credentialFailed: map[string]credentialMark{}, cache: map[string]*render.Cluster{},
	}, nil
}

// Startup turns a processing mark a dead process left into the failed tree "interrupted", then reads the tree of
// every approved and pending pair into the cache. A corrupt store is moved aside, every cluster fetched afresh and
// the pairs the cache lacks read again. A tree that cannot be read is reported and left out of the cache.
func (s *Service) Startup(ctx context.Context) error {
	at := s.cfg.Now()
	if _, err := s.cfg.Repo.ConvertInterrupted(ctx, at); err != nil {
		return err
	}
	clusters, err := s.cfg.Repo.List(ctx)
	if err != nil {
		return err
	}
	pairs, err := s.missingPairs(ctx, clusters)
	if err != nil {
		return err
	}
	trees := make([]string, len(pairs))
	for i, p := range pairs {
		trees[i] = p.tree
	}
	s.git.expect(trees)
	if s.fillCache(ctx, pairs, at) {
		return s.refill(ctx, at)
	}
	return nil
}

// Run polls at once and then every PollInterval until ctx ends. On exit it clears the processing mark of work that
// was cut short, so the next start processes that tree again.
func (s *Service) Run(ctx context.Context) error {
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseDeadline)
		defer cancel()
		s.release(rctx)
	}()
	tick := time.Tick(PollInterval)
	for {
		s.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// poll reads the clusters, drops the cache entries no cluster uses, and fetches and processes each cluster in turn.
// After a move-aside every cluster is fetched afresh and the cache refilled before the poll ends.
func (s *Service) poll(ctx context.Context) {
	at := s.cfg.Now()
	s.release(ctx) // a mark an earlier poll could not clear
	clusters, err := s.cfg.Repo.List(ctx)
	if err != nil {
		s.fail(ctx, stageList, "", err)
		return
	}
	s.dropUnused(clusters)

	moved := false
	for i := range clusters {
		if ctx.Err() != nil {
			return
		}
		c := &clusters[i]
		s.fetchIfDue(ctx, c, at)
		moved = s.processHead(ctx, c, at) || moved
	}
	if moved && ctx.Err() == nil {
		if err := s.refill(ctx, at); err != nil {
			s.fail(ctx, stageCache, "", err)
		}
	}
}

// dropUnused removes the cache entries whose pair is no longer any cluster's approved or pending pair.
func (s *Service) dropUnused(clusters []Cluster) {
	used := map[string]struct{}{}
	for i := range clusters {
		used[clusters[i].ApprovedPairID] = struct{}{}
		used[clusters[i].PendingPairID] = struct{}{}
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	maps.DeleteFunc(s.cache, func(id string, _ *render.Cluster) bool {
		_, ok := used[id]
		return !ok
	})
	// ponytail: fetched and credentialFailed keep an entry per removed cluster; a removal is rare and an entry small.
}

// fetchIfDue fetches c's branch when this process has not fetched it, its revision changed since, or FetchInterval
// has passed. A credential reference without a stored value is resolved first; no fetch runs without it. A fetch
// error is stored sealed and never moves the store aside: a dropped connection is not a corrupt store.
func (s *Service) fetchIfDue(ctx context.Context, c *Cluster, at time.Time) {
	prev, seen := s.fetched[c.ID]
	if seen && prev.revision == c.Revision && at.Sub(prev.at) < FetchInterval {
		return
	}
	password, ok := s.credential(ctx, c, at)
	if !ok {
		return
	}

	fctx, cancel := context.WithTimeout(ctx, FetchDeadline)
	tree, err := s.git.fetch(fctx, c.ID, gitSource{URL: c.RepoURL, Ref: c.Ref, Subpath: c.Subpath, Username: c.Username, Password: password})
	cancel()
	if err != nil && ctx.Err() != nil {
		return
	}
	outcome := outcomeOK
	if err != nil {
		outcome, tree = outcomeError, ""
		if seen && prev.revision == c.Revision {
			tree = prev.tree
		}
	}
	s.fetched[c.ID] = fetchState{revision: c.Revision, at: at, tree: tree}
	s.fetches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if rerr := s.cfg.Repo.RecordFetch(ctx, c.ID, err, at); rerr != nil {
		s.fail(ctx, stageFetch, c.Name, rerr)
	}
	if err != nil {
		// The error is stored sealed; its text can quote the repository URL.
		s.report(ctx, stageFetch, c.Name, "the fetch failed")
	}
}

// credential returns the password to fetch c with: none when c names no credential reference, else the stored one,
// resolved and stored first when there is none. ok is false when there is no password to fetch with yet.
func (s *Service) credential(ctx context.Context, c *Cluster, at time.Time) (password secret.Secret, ok bool) {
	if c.CredentialRef == "" {
		return "", true
	}
	password, ok, err := s.cfg.Repo.Credential(ctx, c.ID)
	if err != nil {
		s.fail(ctx, stageCredential, c.Name, err)
		return "", false
	}
	if ok {
		return password, true
	}
	if m, marked := s.credentialFailed[c.ID]; marked {
		stop := m.holds(c)
		s.credentialFailed[c.ID] = m
		if stop {
			return "", false
		}
	}
	if waiting(c, at) {
		return "", false
	}
	if s.cfg.Resolver == nil {
		s.credentialFailure(ctx, c, errNoToken, at)
		return "", false
	}

	results, err := s.resolve(ctx, []string{c.CredentialRef})
	if err != nil {
		if ctx.Err() == nil {
			s.recordWholeCall(ctx, c, wholeCallFailure(c, "", TriggerCredential, err, at))
		}
		return "", false
	}
	s.resetAttempts(ctx, c)
	password, kind := lookup(results, c.CredentialRef)
	if kind != "" {
		s.credentialFailure(ctx, c, "credential "+c.CredentialRef+": "+kind, at)
		return "", false
	}
	if err := s.cfg.Repo.SetCredential(ctx, c.ID, password, at); err != nil {
		s.fail(ctx, stageCredential, c.Name, err)
		return "", false
	}
	delete(s.credentialFailed, c.ID)
	return password, true
}

// credentialFailure records text as c's fetch error and holds the reference back until c's revision changes or a
// refresh is requested.
func (s *Service) credentialFailure(ctx context.Context, c *Cluster, text string, at time.Time) {
	s.credentialFailed[c.ID] = credentialMark{revision: c.Revision, refresh: c.RefreshRequested}
	if err := s.cfg.Repo.RecordFetch(ctx, c.ID, errors.New(text), at); err != nil {
		s.fail(ctx, stageCredential, c.Name, err)
	}
	s.report(ctx, stageCredential, c.Name, "no credential to fetch with; the fetch error says why")
}

// processHead turns c's fetched head tree into a pending pair or a failed tree, when it is new, no work holds c's
// mark and no backoff or rate limit holds c back. It reports whether a corrupt read moved the store aside.
func (s *Service) processHead(ctx context.Context, c *Cluster, at time.Time) (moved bool) {
	tree := s.fetched[c.ID].tree
	if tree == "" || tree == c.LastHeadTree || c.ProcessingTree != "" || waiting(c, at) {
		return false
	}
	ok, err := s.cfg.Repo.MarkProcessing(ctx, MarkRequest{ClusterID: c.ID, Tree: tree, Revision: c.Revision, Trigger: TriggerHead, At: at})
	if err != nil {
		s.fail(ctx, stageStore, c.Name, err)
		return false
	}
	if !ok {
		return false
	}
	s.held = c.ID

	fsys, err := s.git.readTree(ctx, c.ID, tree)
	if err != nil {
		if ctx.Err() != nil {
			return false // Run clears the mark
		}
		if s.git.corrupt(tree, err) {
			moved = s.moveAside(ctx, c.Name, err, at)
		} else {
			s.report(ctx, stageRead, c.Name, "the head tree cannot be read")
		}
		s.release(ctx)
		return moved
	}
	s.buildPair(ctx, c, tree, fsys, at)
	return false
}

// buildPair loads, resolves and renders the tree c holds the mark on, and stores the result: the pending pair, or the
// failed tree with its error sealed. A whole-call failure is recorded for the backoff instead.
func (s *Service) buildPair(ctx context.Context, c *Cluster, tree string, fsys fs.FS, at time.Time) {
	def, err := render.Load(fsys)
	if err != nil {
		s.storeFailed(ctx, c, tree, err.Error(), at)
		if refusal, isRefusal := errors.AsType[*render.RefusalError](err); isRefusal {
			s.report(ctx, stageLoad, c.Name, "the tree is refused: "+refusal.File+": "+refusal.Rule)
		} else {
			s.report(ctx, stageLoad, c.Name, "the tree does not load")
		}
		return
	}
	if s.cfg.Resolver == nil {
		s.storeFailed(ctx, c, tree, errNoToken, at)
		s.report(ctx, stageResolve, c.Name, errNoToken)
		return
	}

	results, err := s.resolve(ctx, def.Refs)
	if err != nil {
		if ctx.Err() == nil {
			s.recordWholeCall(ctx, c, wholeCallFailure(c, tree, TriggerHead, err, at))
		}
		return
	}
	s.resetAttempts(ctx, c)
	values, failures := resolved(results, def.Refs)
	if len(failures) > 0 {
		s.storeFailed(ctx, c, tree, strings.Join(failures, "; "), at)
		s.report(ctx, stageResolve, c.Name, fmt.Sprintf("%d references did not resolve", len(failures)))
		return
	}

	renders, err := def.Render(ctx, values)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.storeFailed(ctx, c, tree, err.Error(), at)
		s.report(ctx, stageRender, c.Name, "the tree does not render")
		return
	}
	s.storePending(ctx, c, def, StorePendingRequest{
		ClusterID: c.ID, Tree: tree, Revision: c.Revision, Values: values, TalosVersion: def.TalosVersion, Renders: renders, At: at,
	})
}

// storePending stores the pending pair and caches its definition. A store that found the mark stale writes nothing,
// and the mark is then not the service's to clear.
func (s *Service) storePending(ctx context.Context, c *Cluster, def *render.Cluster, req StorePendingRequest) {
	pairID, stored, err := s.cfg.Repo.StorePending(ctx, req)
	if err != nil {
		s.storeError(ctx, c, err)
		return
	}
	s.held = ""
	if stored {
		s.cacheMu.Lock()
		s.cache[pairID] = def
		s.cacheMu.Unlock()
	}
}

// storeFailed records tree as c's failed tree with text sealed. A store that found the mark stale writes nothing,
// and the mark is then not the service's to clear.
func (s *Service) storeFailed(ctx context.Context, c *Cluster, tree, text string, at time.Time) {
	_, err := s.cfg.Repo.StoreFailed(ctx, StoreFailedRequest{ClusterID: c.ID, Tree: tree, Revision: c.Revision, Error: text, At: at})
	if err != nil {
		s.storeError(ctx, c, err)
		return
	}
	s.held = ""
}

// storeError handles a store that failed: on shutdown Run clears the mark; otherwise the mark is cleared now, so the
// next poll processes the tree again.
func (s *Service) storeError(ctx context.Context, c *Cluster, err error) {
	if ctx.Err() != nil {
		return
	}
	s.fail(ctx, stageStore, c.Name, err)
	s.release(ctx)
}

// wholeCallFailure is the record of callErr, a whole-call failure of work on c that held the mark on tree ("" for
// none).
func wholeCallFailure(c *Cluster, tree string, trigger Trigger, callErr error, at time.Time) WholeCallFailure {
	return WholeCallFailure{
		ClusterID: c.ID, Tree: tree, Trigger: trigger, RateLimited: errors.Is(callErr, secrets.ErrRateLimited),
		NextDaily: nextDaily(c, at), Error: callErr.Error(), At: at,
	}
}

// recordWholeCall records f for c; the repository clears the mark on f.Tree.
func (s *Service) recordWholeCall(ctx context.Context, c *Cluster, f WholeCallFailure) {
	if err := s.cfg.Repo.RecordWholeCallFailure(ctx, f); err != nil {
		s.fail(ctx, stageResolve, c.Name, err)
		if f.Tree != "" {
			s.release(ctx)
		}
		return
	}
	if f.Tree != "" {
		s.held = ""
	}
	s.report(ctx, stageResolve, c.Name, "the 1Password call failed")
}

// resolve calls the resolver under ResolveDeadline and keeps the count of consecutive whole-call failures. A call
// cut short by ctx's end is not counted.
func (s *Service) resolve(ctx context.Context, refs []string) (map[string]secrets.Result, error) {
	rctx, cancel := context.WithTimeout(ctx, ResolveDeadline)
	defer cancel()
	results, err := s.cfg.Resolver.ResolveAll(rctx, refs)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if err != nil {
		whole, ok := errors.AsType[*secrets.WholeCallError](err)
		s.clientFailures++
		s.clientPanicked = ok && whole.Panicked
		return nil, err
	}
	s.clientFailures, s.clientPanicked = 0, false
	return results, nil
}

// resetAttempts clears c's backoff after a call that succeeded as a whole, in the store and on c, so the rest of the
// poll is not held back by it.
func (s *Service) resetAttempts(ctx context.Context, c *Cluster) {
	if err := s.cfg.Repo.ResetAttempts(ctx, c.ID); err != nil {
		s.fail(ctx, stageResolve, c.Name, err)
		return
	}
	c.Attempts, c.NextAttemptAt = 0, time.Time{}
}

// moveAside moves the store aside after a corrupt read and raises the alert; every cluster is then fetched afresh.
// It reports whether it moved the store: within an hour of the last move-aside it does not, and reports the read
// error's class only.
func (s *Service) moveAside(ctx context.Context, cluster string, readErr error, at time.Time) bool {
	moved, ok, err := s.git.moveAside(at)
	if err != nil {
		s.fail(ctx, stageRead, cluster, err)
	}
	if !ok {
		if err == nil {
			s.report(ctx, stageRead, cluster, "a "+className(classifyRead(readErr))+" object; the git store was moved aside less than an hour ago")
		}
		return false
	}
	clear(s.fetched)
	ev := fleet.AuditEvent{At: at, Actor: fleet.ActorSystem, Action: actionMovedAside, Detail: movedDetail{Moved: moved}}
	if err := fleet.InsertAudit(ctx, s.cfg.Repo.bdb, ev); err != nil {
		s.fail(ctx, stageRead, cluster, err)
	}
	s.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", alertMovedAside)))
	return true
}

// refill fetches every cluster this process has not fetched since the move-aside, then reads the pairs the cache
// lacks. A corrupt read there does not move the store again within the hour.
func (s *Service) refill(ctx context.Context, at time.Time) error {
	clusters, err := s.cfg.Repo.List(ctx)
	if err != nil {
		return err
	}
	for i := range clusters {
		s.fetchIfDue(ctx, &clusters[i], at)
	}
	pairs, err := s.missingPairs(ctx, clusters)
	if err != nil {
		return err
	}
	s.fillCache(ctx, pairs, at)
	return nil
}

// missingPairs returns the approved and pending pairs of clusters that the cache does not hold.
func (s *Service) missingPairs(ctx context.Context, clusters []Cluster) ([]cachePair, error) {
	var out []cachePair
	for i := range clusters {
		c := &clusters[i]
		for _, id := range []string{c.ApprovedPairID, c.PendingPairID} {
			if id == "" || s.cached(id) {
				continue
			}
			p, err := s.cfg.Repo.Pair(ctx, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, cachePair{pairID: id, clusterID: c.ID, cluster: c.Name, tree: p.TreeHash})
		}
	}
	return out, nil
}

func (s *Service) cached(pairID string) bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	_, ok := s.cache[pairID]
	return ok
}

// fillCache reads and loads each pair's tree into the cache. It stops, reporting true, when a corrupt read moved the
// store aside; a tree that cannot be read or loaded otherwise is reported and skipped.
func (s *Service) fillCache(ctx context.Context, pairs []cachePair, at time.Time) (moved bool) {
	for _, p := range pairs {
		if ctx.Err() != nil {
			return false
		}
		fsys, err := s.git.readTree(ctx, p.clusterID, p.tree)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			if s.git.corrupt(p.tree, err) {
				if s.moveAside(ctx, p.cluster, err, at) {
					return true
				}
				continue
			}
			s.report(ctx, stageCache, p.cluster, "a pair's tree cannot be read: a "+className(classifyRead(err))+" object")
			continue
		}
		def, err := render.Load(fsys)
		if err != nil {
			s.report(ctx, stageCache, p.cluster, "a pair's tree does not load")
			continue
		}
		s.cacheMu.Lock()
		s.cache[p.pairID] = def
		s.cacheMu.Unlock()
	}
	return false
}

// release clears the processing mark the service holds, if any; a mark it cannot clear is kept, to be cleared by the
// next poll or by Run's exit.
func (s *Service) release(ctx context.Context) {
	if s.held == "" {
		return
	}
	if err := s.cfg.Repo.ClearProcessing(ctx, s.held); err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(ctx, stageRelease, "", err)
		return
	}
	s.held = ""
}

// report tells OnError of a failure in stage of the work on cluster, described by detail, which must quote no
// third-party text.
func (s *Service) report(ctx context.Context, stage, cluster, detail string) {
	s.cfg.OnError(ctx, fmt.Errorf("clusters: %s: cluster %s: %s", stage, cluster, detail))
}

// fail tells OnError of err, a failure of the service's own storage, unless ctx has ended.
func (s *Service) fail(ctx context.Context, stage, cluster string, err error) {
	if ctx.Err() != nil {
		return
	}
	if cluster == "" {
		s.cfg.OnError(ctx, fmt.Errorf("clusters: %s: %w", stage, err))
		return
	}
	s.cfg.OnError(ctx, fmt.Errorf("clusters: %s: cluster %s: %w", stage, cluster, err))
}

// waiting reports whether a backoff or a rate limit holds c's calls back at at.
func waiting(c *Cluster, at time.Time) bool {
	return at.Before(c.NextAttemptAt) || at.Before(c.RateLimitedUntil)
}

// nextDaily is when c's next daily trigger runs: a day after the last one, or a day from at when that is past.
func nextDaily(c *Cluster, at time.Time) time.Time {
	next := c.LastDailyAt.Add(DailyInterval)
	if !next.After(at) {
		next = at.Add(DailyInterval)
	}
	return next
}

// lookup returns the value of ref in results, or the kind of its failure: a *secrets.ReferenceError's Kind, or
// "missing" when results lack ref, "unknown" for any other error.
func lookup(results map[string]secrets.Result, ref string) (value secret.Secret, kind string) {
	r, ok := results[ref]
	if !ok {
		return "", kindMissingResult
	}
	if r.Err == nil {
		return r.Value, ""
	}
	if re, ok := errors.AsType[*secrets.ReferenceError](r.Err); ok {
		return "", re.Kind
	}
	return "", "unknown"
}

// resolved returns the value of every reference, or "reference <ref>: <kind>" for each one that failed.
func resolved(results map[string]secrets.Result, refs []string) (values map[string]string, failures []string) {
	values = make(map[string]string, len(refs))
	for _, ref := range refs {
		value, kind := lookup(results, ref)
		if kind != "" {
			failures = append(failures, "reference "+ref+": "+kind)
			continue
		}
		values[ref] = value.Expose()
	}
	return values, failures
}

// className names a read class for a report.
func className(c readClass) string {
	switch c {
	case readCorrupt:
		return "corrupt"
	case readMissing:
		return "missing"
	case readOther:
		return "unreadable"
	default:
		return "unreadable"
	}
}
