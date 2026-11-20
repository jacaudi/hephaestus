package clusters

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/secrets"
)

var (
	// ErrNoRender is returned by Render when the ticket's pair is not in the tree cache or is no longer stored. It
	// sets neither drift nor render:approved.
	ErrNoRender = errors.New("clusters: no render for this pair")
	// ErrDrift is returned by Render when the host's render hashes differently from its stored one; the host's render
	// row then records the drift.
	ErrDrift = errors.New("clusters: render differs from the approved hash")
	// errRender marks an error of the render itself: the definition does not render, or renders no config for a host.
	errRender = errors.New("clusters: the pair does not render")
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
	errNoToken              = "HEPHAESTUS_OP_TOKEN is not set" //nolint:gosec // G101: names the token variable, holds no credential
	actionMovedAside        = "cluster.store_moved_aside"
	alertMovedAside         = "git_store_moved_aside"
	actionRefreshIncomplete = "cluster.refresh_incomplete"
	alertRefreshIncomplete  = "refresh_incomplete"
	actionDrift             = "cluster.drift"
	alertDrift              = "drift"
	outcomeOK               = "ok"
	outcomeError            = "error"
	kindMissingResult       = "missing"
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
	stageRefresh    = "refresh"
	stageStore      = "store"
	stageCache      = "cache"
	stageRelease    = "release"
	stageDrift      = "drift"
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
	// Now is the clock. Render and the checks read it from their callers' goroutines, so it must be safe for
	// concurrent use, as time.Now is.
	Now func() time.Time
	// OnError is told of every failure the service survives. Its errors name the stage, the cluster and, for a
	// refused tree, the file and rule; never a resolver, render or git error's text. Render's callers reach it from
	// their own goroutines, so it must be safe for concurrent use.
	OnError func(context.Context, error)
	// Meter creates the service's counters; nil is a no-op meter.
	Meter metric.Meter
}

// Service polls the imported clusters, fetches their branches, turns each new head tree into a pending pair and
// runs the refreshes their triggers ask for. It keeps the tree of every approved and pending pair, loaded, in a
// cache. Startup and then Run are called on one goroutine, which owns the poll state and every processing mark the
// service holds: head work, refreshes and drift detection alike run on it. Render and Checks set no mark and are safe
// to call from any goroutine.
type Service struct {
	cfg     Config
	git     *gitStore
	fetches metric.Int64Counter
	alerts  metric.Int64Counter

	// fetched is the last fetch of each cluster this process made.
	fetched map[string]fetchState
	// credentialFailed holds the clusters whose credential reference failed, so it is not resolved again.
	credentialFailed map[string]credentialMark
	// held holds the ids of the clusters whose processing mark the service holds: the one of the work in progress, and
	// any whose release failed, which later polls and Run's exit retry. ClearProcessing does not check whose mark it
	// clears, so only the holder may call it.
	held map[string]struct{}

	cacheMu sync.Mutex
	// cache maps a pair id to its loaded definition.
	cache map[string]*render.Cluster

	clientMu sync.Mutex
	// clientFailures counts the consecutive whole-call failures of the resolver; clientPanicked is whether the last
	// one was a panic of the SDK's core.
	clientFailures int
	clientPanicked bool

	renderMu sync.Mutex
	// renderFailed maps a pair id to the hosts whose last render of it failed, "" standing for a tree that could not be
	// loaded into the cache; render:approved reports those of each cluster's approved pair.
	// ponytail: an entry stays after its pair is replaced; the check skips it, and a failed pair is rare and small.
	renderFailed map[string][]string
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

// work is a piece of work on c's tree, started by trigger at at; it holds the tree's processing mark once marked.
// tree is "" for work that holds no mark (a credential resolution).
type work struct {
	c       *Cluster
	tree    string
	trigger Trigger
	at      time.Time
}

// cachePair is a pair the cache should hold.
type cachePair struct {
	pairID, clusterID, cluster, tree string
}

// driftDetail is the detail of a cluster.drift audit event.
type driftDetail struct {
	Cluster string `json:"cluster"`
	Host    string `json:"host"`
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
		fetched: map[string]fetchState{}, credentialFailed: map[string]credentialMark{}, held: map[string]struct{}{}, cache: map[string]*render.Cluster{},
		renderFailed: map[string][]string{},
	}, nil
}

// Startup turns a processing mark a dead process left into the failed tree "interrupted", then reads the tree of
// every approved and pending pair into the cache. A corrupt store is moved aside, every cluster fetched afresh and
// the pairs the cache lacks read again. A tree that cannot be read is reported and left out of the cache. Last, every
// host of every approved pair is checked for drift.
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
		if err := s.refill(ctx, at); err != nil {
			return err
		}
	}
	for i := range clusters {
		s.detectDrift(ctx, &clusters[i], at)
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

// poll reads the clusters, drops the cache entries no cluster uses, and fetches, processes and refreshes each cluster
// in turn. After a move-aside no refresh runs; every cluster is fetched afresh and the cache refilled before the poll
// ends.
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
		if !moved {
			moved = s.refreshIfDue(ctx, c, at)
		}
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
			s.recordWholeCall(ctx, c, wholeCallFailure(work{c: c, trigger: TriggerCredential, at: at}, err))
		}
		return "", false
	}
	s.resetAttempts(ctx, c)
	password, kind := lookup(results, c.CredentialRef)
	if kind != "" {
		s.credentialFailure(ctx, c, credentialText(c.CredentialRef, kind), at)
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
	// A tree fetched at another revision may be the old source's: it is not this revision's head.
	f := s.fetched[c.ID]
	tree := f.tree
	if f.revision != c.Revision {
		tree = ""
	}
	if tree == "" || tree == c.LastHeadTree || c.ProcessingTree != "" || waiting(c, at) {
		return false
	}
	w := work{c: c, tree: tree, trigger: TriggerHead, at: at}
	if !s.mark(ctx, w) {
		return false
	}
	fsys, moved, ok := s.readMarked(ctx, w)
	if !ok {
		if ctx.Err() == nil { // on shutdown Run clears the mark
			s.release(ctx)
		}
		return moved
	}
	s.buildPair(ctx, w, fsys)
	return false
}

// mark records w's tree as processing, consuming w's trigger, and reports whether it did. A cluster whose mark the
// service could not release is not marked again until the release succeeds.
func (s *Service) mark(ctx context.Context, w work) bool {
	if _, unreleased := s.held[w.c.ID]; unreleased {
		return false
	}
	ok, err := s.cfg.Repo.MarkProcessing(ctx, MarkRequest{ClusterID: w.c.ID, Tree: w.tree, Revision: w.c.Revision, Trigger: w.trigger, At: w.at})
	if err != nil {
		s.fail(ctx, stageStore, w.c.Name, err)
		return false
	}
	if ok {
		s.held[w.c.ID] = struct{}{}
	}
	return ok
}

// readMarked reads w's tree. When it cannot, ok is false and the mark is left for the caller: after a corrupt read
// the store is moved aside (moved reports whether it was), otherwise the failure is reported; on shutdown neither.
func (s *Service) readMarked(ctx context.Context, w work) (fsys fs.FS, moved, ok bool) {
	fsys, err := s.git.readTree(ctx, w.c.ID, w.tree)
	if err == nil {
		return fsys, false, true
	}
	if ctx.Err() != nil {
		return nil, false, false
	}
	if s.git.corrupt(w.tree, err) {
		return nil, s.moveAside(ctx, w.c.Name, err, w.at), false
	}
	s.report(ctx, stageRead, w.c.Name, "the tree cannot be read")
	return nil, false, false
}

// buildPair loads, resolves and renders the tree w holds the mark on, and stores the result: the pending pair, or the
// failed tree with its error sealed. A whole-call failure is recorded for the backoff instead. Work other than a head
// tree stores the pair as a refresh. It reports whether it stored the failed tree.
func (s *Service) buildPair(ctx context.Context, w work, fsys fs.FS) (failed bool) {
	c := w.c
	def, err := render.Load(fsys)
	if err != nil {
		failed = s.storeFailed(ctx, w, err.Error())
		if refusal, isRefusal := errors.AsType[*render.RefusalError](err); isRefusal {
			s.report(ctx, stageLoad, c.Name, "the tree is refused: "+refusal.File+": "+refusal.Rule)
		} else {
			s.report(ctx, stageLoad, c.Name, "the tree does not load")
		}
		return failed
	}
	if s.cfg.Resolver == nil {
		failed = s.storeFailed(ctx, w, errNoToken)
		s.report(ctx, stageResolve, c.Name, errNoToken)
		return failed
	}

	values, failures, ok := s.resolveTree(ctx, w, def.Refs)
	if !ok {
		return false
	}
	if len(failures) > 0 {
		failed = s.storeFailed(ctx, w, strings.Join(failures, "; "))
		s.report(ctx, stageResolve, c.Name, fmt.Sprintf("%d references did not resolve", len(failures)))
		return failed
	}

	renders, err := def.Render(ctx, values)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		failed = s.storeFailed(ctx, w, err.Error())
		s.report(ctx, stageRender, c.Name, "the tree does not render")
		return failed
	}
	s.storePending(ctx, c, def, StorePendingRequest{
		ClusterID: c.ID, Tree: w.tree, Revision: c.Revision, Values: values, TalosVersion: def.TalosVersion, Renders: renders,
		Refresh: w.trigger != TriggerHead, At: w.at,
	})
	return false
}

// storePending stores the pending pair and caches its definition. A store that found the mark stale writes nothing,
// and the mark is then not the service's to clear.
func (s *Service) storePending(ctx context.Context, c *Cluster, def *render.Cluster, req StorePendingRequest) {
	pairID, stored, err := s.cfg.Repo.StorePending(ctx, req)
	if err != nil {
		s.storeError(ctx, c, err)
		return
	}
	delete(s.held, c.ID)
	if stored {
		s.keep(pairID, def)
	}
}

// storeFailed records w's tree as c's failed tree with text sealed, and reports whether it did. A store that found
// the mark stale writes nothing, and the mark is then not the service's to clear.
func (s *Service) storeFailed(ctx context.Context, w work, text string) bool {
	stored, err := s.cfg.Repo.StoreFailed(ctx, StoreFailedRequest{ClusterID: w.c.ID, Tree: w.tree, Revision: w.c.Revision, Error: text, At: w.at})
	if err != nil {
		s.storeError(ctx, w.c, err)
		return false
	}
	delete(s.held, w.c.ID)
	return stored
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

// resolveTree resolves refs, the references of the tree w holds the mark on: the value of each, and
// "reference <ref>: <kind>" for each that failed. A tree without references needs no call. ok is false when the call
// failed as a whole, which is recorded for the backoff, or was cut short by ctx's end.
func (s *Service) resolveTree(ctx context.Context, w work, refs []string) (values map[string]string, failures []string, ok bool) {
	if len(refs) == 0 {
		return map[string]string{}, nil, true
	}
	results, err := s.resolve(ctx, refs)
	if err != nil {
		if ctx.Err() == nil {
			s.recordWholeCall(ctx, w.c, wholeCallFailure(w, err))
		}
		return nil, nil, false
	}
	s.resetAttempts(ctx, w.c)
	values, failures = resolved(results, refs)
	return values, failures, true
}

// refreshIfDue runs at most one refresh of c when a trigger is due, c carries no mark and no backoff or rate limit
// holds it back. Without a resolver nothing runs and the trigger stays pending; secrets:token reports the missing
// token. The cluster is read again first, since the poll's work on it so far may have changed it. After the refresh,
// the hosts of its approved pair are checked for drift. It reports whether a corrupt read moved the store aside.
func (s *Service) refreshIfDue(ctx context.Context, c *Cluster, at time.Time) (moved bool) {
	if s.cfg.Resolver == nil {
		return false
	}
	if _, due := refreshTrigger(c, at); !due {
		return false
	}
	fresh, err := s.cfg.Repo.Get(ctx, c.Name)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.fail(ctx, stageRefresh, c.Name, err)
		}
		return false
	}
	trigger, due := refreshTrigger(&fresh, at)
	if !due || fresh.ProcessingTree != "" || waiting(&fresh, at) {
		return false
	}
	moved = s.refresh(ctx, &fresh, trigger, at)
	s.detectDrift(ctx, &fresh, at)
	return moved
}

// refreshTrigger is the refresh trigger due for c at at: a refresh request, else the daily timer, a DailyInterval
// after the last daily refresh or, before the first, after the import.
func refreshTrigger(c *Cluster, at time.Time) (Trigger, bool) {
	if c.RefreshRequested {
		return TriggerRefresh, true
	}
	since := c.LastDailyAt
	if since.IsZero() {
		since = c.CreatedAt
	}
	if !at.Before(since.Add(DailyInterval)) {
		return TriggerDaily, true
	}
	return "", false
}

// refresh runs one refresh of c, started by trigger (design 5.4): it resolves the read credential again, retries the
// failed tree, and when there is none or it fails again, resolves the newest good tree. A cluster with no tree to
// mark runs nothing, so its trigger stays until one is stored. It reports whether a corrupt read moved the store
// aside.
func (s *Service) refresh(ctx context.Context, c *Cluster, trigger Trigger, at time.Time) (moved bool) {
	if c.FailedTree == "" && c.PendingPairID == "" && c.ApprovedPairID == "" {
		return false
	}
	if !s.refreshCredential(ctx, c, trigger, at) {
		return false
	}
	if c.FailedTree != "" {
		next, moved := s.retryFailed(ctx, work{c: c, tree: c.FailedTree, trigger: trigger, at: at})
		if !next {
			return moved
		}
		trigger = TriggerContinue
	}
	return s.refreshGood(ctx, c, trigger, at)
}

// refreshCredential resolves c's read credential again, in its own call, and stores a changed value. A failed
// reference is recorded as the fetch error and the refresh goes on with the stored value. It reports whether the
// refresh goes on: not after a whole-call failure, which is recorded for the backoff.
func (s *Service) refreshCredential(ctx context.Context, c *Cluster, trigger Trigger, at time.Time) bool {
	if c.CredentialRef == "" {
		return true
	}
	results, err := s.resolve(ctx, []string{c.CredentialRef})
	if err != nil {
		if ctx.Err() == nil {
			s.recordWholeCall(ctx, c, wholeCallFailure(work{c: c, trigger: trigger, at: at}, err))
		}
		return false
	}
	s.resetAttempts(ctx, c)
	value, kind := lookup(results, c.CredentialRef)
	if kind != "" {
		if err := s.cfg.Repo.RecordFetch(ctx, c.ID, errors.New(credentialText(c.CredentialRef, kind)), at); err != nil {
			s.fail(ctx, stageCredential, c.Name, err)
		}
		s.report(ctx, stageCredential, c.Name, "the credential did not resolve; the fetch error says why")
		return true
	}
	stored, ok, err := s.cfg.Repo.Credential(ctx, c.ID)
	if err != nil {
		s.fail(ctx, stageCredential, c.Name, err)
		return true
	}
	if ok && stored == value {
		return true
	}
	if err := s.cfg.Repo.SetCredential(ctx, c.ID, value, at); err != nil {
		s.fail(ctx, stageCredential, c.Name, err)
	}
	return true
}

// retryFailed is step 1 of a refresh: the failed tree is processed again, and stored as the pending pair when it now
// succeeds. next reports whether step 2 follows: when the tree failed again, or could not be read.
func (s *Service) retryFailed(ctx context.Context, w work) (next, moved bool) {
	if !s.mark(ctx, w) {
		return false, false
	}
	fsys, moved, ok := s.readMarked(ctx, w)
	if !ok {
		if ctx.Err() != nil {
			return false, false
		}
		s.release(ctx)
		return !moved, moved
	}
	return s.buildPair(ctx, w, fsys), false
}

// refreshGood is step 2 of a refresh: the newest good pair, the pending one else the approved one, is resolved again
// and its values compared with the pair's version. It reports whether a corrupt read moved the store aside.
func (s *Service) refreshGood(ctx context.Context, c *Cluster, trigger Trigger, at time.Time) (moved bool) {
	pairID := cmp.Or(c.PendingPairID, c.ApprovedPairID)
	if pairID == "" {
		return false
	}
	p, err := s.cfg.Repo.Pair(ctx, pairID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.fail(ctx, stageRefresh, c.Name, err)
		}
		return false
	}
	w := work{c: c, tree: p.TreeHash, trigger: trigger, at: at}
	if !s.mark(ctx, w) {
		return false
	}
	def, ok := s.cachedDef(pairID)
	if !ok {
		fsys, moved, read := s.readMarked(ctx, w)
		if !read {
			if ctx.Err() == nil {
				s.incomplete(ctx, w, "the tree cannot be read")
			}
			return moved
		}
		if def, err = render.Load(fsys); err != nil {
			s.incomplete(ctx, w, "the tree does not load")
			return false
		}
		s.keep(pairID, def)
	}
	s.refreshValues(ctx, w, def, p.SecretVersionID)
	return false
}

// refreshValues resolves def's references for w and, when a value differs from version's, renders def with the new
// values and stores the result as the pending pair (w's tree, a new version). Equal values record a complete refresh
// that stores nothing; a failed reference, a stored version that does not open or a render error record an
// incomplete one, which keeps the failed tree and the last head tree as they are.
func (s *Service) refreshValues(ctx context.Context, w work, def *render.Cluster, versionID string) {
	values, failures, ok := s.resolveTree(ctx, w, def.Refs)
	if !ok {
		return
	}
	if len(failures) > 0 {
		s.incomplete(ctx, w, fmt.Sprintf("%d references did not resolve", len(failures)))
		return
	}
	old, err := s.cfg.Repo.Values(ctx, versionID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fail(ctx, stageRefresh, w.c.Name, err)
		s.incomplete(ctx, w, "the pair's version cannot be read")
		return
	}
	if maps.Equal(values, old) {
		s.recordRefresh(ctx, w, false)
		return
	}
	renders, err := def.Render(ctx, values)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.incomplete(ctx, w, "the tree does not render") // the render error can quote a value: it is not kept
		return
	}
	s.storePending(ctx, w.c, def, StorePendingRequest{
		ClusterID: w.c.ID, Tree: w.tree, Revision: w.c.Revision, Values: values, TalosVersion: def.TalosVersion, Renders: renders,
		Refresh: true, At: w.at,
	})
}

// incomplete records w as a refresh that could not finish, audits it as cluster.refresh_incomplete and raises the
// alert refresh_incomplete. why is reported and must quote no third-party text.
func (s *Service) incomplete(ctx context.Context, w work, why string) {
	if !s.recordRefresh(ctx, w, true) {
		return
	}
	ev := fleet.AuditEvent{At: w.at, Actor: fleet.ActorSystem, Action: actionRefreshIncomplete, Detail: nameDetail{Name: w.c.Name}}
	if err := fleet.InsertAudit(ctx, s.cfg.Repo.bdb, ev); err != nil {
		s.fail(ctx, stageRefresh, w.c.Name, err)
	}
	s.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", alertRefreshIncomplete)))
	s.report(ctx, stageRefresh, w.c.Name, "the refresh is incomplete: "+why)
}

// recordRefresh records w as a refresh that stores no pair, which clears its mark, and reports whether it did. A
// record that found the mark stale writes nothing, and the mark is then not the service's to clear.
func (s *Service) recordRefresh(ctx context.Context, w work, incomplete bool) bool {
	recorded, err := s.cfg.Repo.RecordRefresh(ctx, RefreshRecord{ClusterID: w.c.ID, Tree: w.tree, Revision: w.c.Revision, Incomplete: incomplete, At: w.at})
	if err != nil {
		s.storeError(ctx, w.c, err)
		return false
	}
	delete(s.held, w.c.ID)
	return recorded
}

// wholeCallFailure is the record of callErr, a whole-call failure of w.
func wholeCallFailure(w work, callErr error) WholeCallFailure {
	return WholeCallFailure{
		ClusterID: w.c.ID, Tree: w.tree, Trigger: w.trigger, RateLimited: errors.Is(callErr, secrets.ErrRateLimited),
		NextDaily: nextDaily(w.c, w.at), Error: callErr.Error(), At: w.at,
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
		delete(s.held, c.ID)
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
			if id == "" {
				continue
			}
			if _, ok := s.cachedDef(id); ok {
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

// cachedDef returns the cached definition of the pair.
func (s *Service) cachedDef(pairID string) (*render.Cluster, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	def, ok := s.cache[pairID]
	return def, ok
}

// keep caches def as the definition of the pair.
func (s *Service) keep(pairID string, def *render.Cluster) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache[pairID] = def
}

// fillCache reads and loads each pair's tree into the cache. It stops, reporting true, when a corrupt read moved the
// store aside; a tree that cannot be read or loaded otherwise is reported, skipped and kept for render:approved,
// which reports it when the pair is a cluster's approved one.
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
				s.failRender(p.pairID, "")
				continue
			}
			s.report(ctx, stageCache, p.cluster, "a pair's tree cannot be read: a "+className(classifyRead(err))+" object")
			s.failRender(p.pairID, "")
			continue
		}
		def, err := render.Load(fsys)
		if err != nil {
			s.report(ctx, stageCache, p.cluster, "a pair's tree does not load")
			s.failRender(p.pairID, "")
			continue
		}
		s.keep(p.pairID, def)
		s.clearRender(p.pairID)
	}
	return false
}

// Approved returns the cluster's approved pair, ErrNotFound when it has none.
func (s *Service) Approved(ctx context.Context, clusterID string) (Pair, error) {
	return s.cfg.Repo.ApprovedPair(ctx, clusterID)
}

// Drift reports whether the approved render of the machine's host records drift, as Repository.Drift does.
func (s *Service) Drift(ctx context.Context, m fleet.Machine) (bool, error) {
	return s.cfg.Repo.Drift(ctx, m)
}

// EnsureAssets creates the talos_cache row the machine installs from when it is absent, as Repository.EnsureAssets
// does.
func (s *Service) EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error {
	return s.cfg.Repo.EnsureAssets(ctx, m, at)
}

// Render returns the machine config of the ticket's host, rendered from the ticket's pair and secret version only,
// never the cluster's current ones. It reads the tree cache and the database, never the git store, so a fetch does
// not hold it up. A pair the cache lacks, or one no longer stored, is ErrNoRender. A render that hashes differently
// from the host's stored one records the drift on the host's row and is ErrDrift. A stored row that does not open
// (datakey.ErrDecrypt) or a render that fails keeps render:approved failing for the pair until a render of it is
// produced; any other repository error, such as a dropped connection, leaves render:approved as it is. The error's
// text can quote a secret value, so it is never logged.
func (s *Service) Render(ctx context.Context, t fleet.Ticket) ([]byte, error) {
	config, err := s.renderTicket(ctx, t)
	s.noteRender(ctx, t.PairID, err, t.HostName)
	return config, err
}

// renderTicket renders the host of t from t's pair and version.
func (s *Service) renderTicket(ctx context.Context, t fleet.Ticket) ([]byte, error) {
	def, ok := s.cachedDef(t.PairID)
	if !ok {
		return nil, ErrNoRender
	}
	if _, err := s.cfg.Repo.Pair(ctx, t.PairID); err != nil {
		return nil, noRender(err)
	}
	fresh, rows, err := s.renderPair(ctx, def, t.PairID, t.SecretVersionID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(rows, func(row HostRow) bool { return row.Host == t.HostName })
	if i < 0 {
		return nil, ErrNoRender
	}
	return s.checkHost(ctx, t.ClusterID, rows[i], fresh, s.cfg.Now())
}

// detectDrift checks every host of c's approved pair for drift, as Render does, and reports a pair or host that does
// not render. A pair the cache lacks is left alone: the cache fill reported it.
func (s *Service) detectDrift(ctx context.Context, c *Cluster, at time.Time) {
	def, ok := s.cachedDef(c.ApprovedPairID)
	if !ok {
		return
	}
	p, err := s.cfg.Repo.Pair(ctx, c.ApprovedPairID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.fail(ctx, stageDrift, c.Name, err)
		}
		return
	}
	hosts := make([]string, len(def.Hosts))
	for i, h := range def.Hosts {
		hosts[i] = h.Name
	}
	fresh, rows, err := s.renderPair(ctx, def, p.ID, p.SecretVersionID)
	s.noteRender(ctx, p.ID, err, hosts...)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoRender), ctx.Err() != nil:
		case pairFailed(err):
			s.report(ctx, stageDrift, c.Name, "the approved pair does not render") // the error can quote a value
		default:
			s.fail(ctx, stageDrift, c.Name, err)
		}
		return
	}
	for i := range rows {
		if _, err := s.checkHost(ctx, c.ID, rows[i], fresh, at); err != nil && !errors.Is(err, ErrDrift) {
			s.failRender(p.ID, rows[i].Host)
			s.report(ctx, stageDrift, c.Name, "the approved pair renders no config for host "+rows[i].Host)
		}
	}
}

// renderPair renders def, the cached tree of the pair, with the values of version, and returns the render of each
// host by name beside the pair's stored rows. A version no longer stored is ErrNoRender.
func (s *Service) renderPair(ctx context.Context, def *render.Cluster, pairID, versionID string) (map[string]render.HostRender, []HostRow, error) {
	values, err := s.cfg.Repo.Values(ctx, versionID)
	if err != nil {
		return nil, nil, noRender(err)
	}
	rows, err := s.cfg.Repo.HostRows(ctx, pairID)
	if err != nil {
		return nil, nil, err
	}
	renders, err := def.Render(ctx, values)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: pair %s: %w", errRender, pairID, err)
	}
	fresh := make(map[string]render.HostRender, len(renders))
	for i := range renders {
		fresh[renders[i].Host] = renders[i]
	}
	return fresh, rows, nil
}

// checkHost returns the config of row's host from fresh when it hashes as row's; otherwise it records the drift and
// is ErrDrift. A host fresh lacks is a render error.
func (s *Service) checkHost(ctx context.Context, clusterID string, row HostRow, fresh map[string]render.HostRender, at time.Time) ([]byte, error) {
	got, ok := fresh[row.Host]
	if !ok {
		return nil, fmt.Errorf("%w: pair %s renders no config for host %s", errRender, row.PairID, row.Host)
	}
	if got.ConfigHash == row.ConfigHash {
		return got.Config, nil
	}
	s.setDrift(ctx, clusterID, row, got, at)
	return nil, ErrDrift
}

// setDrift records got as the drift of row's host, conditional on the hash row was read with, and when it wrote,
// raises the alert drift and audits cluster.drift.
func (s *Service) setDrift(ctx context.Context, clusterID string, row HostRow, got render.HostRender, at time.Time) {
	set, err := s.cfg.Repo.SetDrift(ctx, SetDriftRequest{
		PairID: row.PairID, Host: row.Host, ReadHash: row.ConfigHash,
		Render: DriftRender{ConfigHash: got.ConfigHash, DisplayHash: got.DisplayHash, Redacted: got.Redacted},
	})
	if err != nil {
		s.fail(ctx, stageDrift, "", err)
		return
	}
	if !set {
		return // another pair is approved, or the row was re-approved meanwhile
	}
	s.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", alertDrift)))
	c, err := findCluster(ctx, s.cfg.Repo.bdb, columnID, clusterID, false)
	if err != nil {
		s.fail(ctx, stageDrift, "", err)
		return
	}
	ev := fleet.AuditEvent{At: at, Actor: fleet.ActorSystem, Action: actionDrift, Detail: driftDetail{Cluster: c.Name, Host: row.Host}}
	if err := fleet.InsertAudit(ctx, s.cfg.Repo.bdb, ev); err != nil {
		s.fail(ctx, stageDrift, c.Name, err)
	}
}

// noteRender keeps render:approved's view of the pair after a render of it for hosts ended with err: a render that was
// produced, served or drifted, clears the pair; a pair that failed marks the hosts failing. No render, a render ctx cut
// short and any other repository error, such as a dropped database connection, say nothing of the pair and change
// nothing.
func (s *Service) noteRender(ctx context.Context, pairID string, err error, hosts ...string) {
	switch {
	case err == nil, errors.Is(err, ErrDrift):
		s.clearRender(pairID)
	case ctx.Err() == nil && pairFailed(err):
		s.failRender(pairID, hosts...)
	}
}

// pairFailed reports whether err says the pair itself cannot render: a stored row of it that does not open, or a
// render error.
func pairFailed(err error) bool {
	return errors.Is(err, datakey.ErrDecrypt) || errors.Is(err, errRender)
}

// failRender marks the hosts of the pair failing for render:approved.
func (s *Service) failRender(pairID string, hosts ...string) {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	for _, h := range hosts {
		if !slices.Contains(s.renderFailed[pairID], h) {
			s.renderFailed[pairID] = append(s.renderFailed[pairID], h)
		}
	}
}

// clearRender marks every host of the pair rendering for render:approved.
func (s *Service) clearRender(pairID string) {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	delete(s.renderFailed, pairID)
}

// noRender is err, or ErrNoRender when err says the pair or its version is gone: a newer approval deleted them.
func noRender(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrNoRender
	}
	return err
}

// release clears every processing mark the service holds; a mark it cannot clear is kept, to be cleared by the next
// poll or by Run's exit, and its cluster is not marked again until then.
func (s *Service) release(ctx context.Context) {
	for id := range s.held {
		if err := s.cfg.Repo.ClearProcessing(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			s.fail(ctx, stageRelease, "", err)
			continue
		}
		delete(s.held, id)
	}
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

// credentialText is the fetch error of a credential reference that failed with kind.
func credentialText(ref, kind string) string {
	return "credential " + ref + ": " + kind
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
