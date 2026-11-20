package clusters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/secrets"
)

const (
	// svcBundle is the shared secrets-bundle fixture.
	svcBundle = "../third_party/topf/render/testdata/bundle/"
	// svcTopf is the definition of cluster lab, which loads and renders.
	svcTopf = `clusterName: lab
clusterEndpoint: https://10.10.20.11:6443
kubernetesVersion: v1.35.10
talosVersion: v1.12.11
nodes:
  - host: cp-01
    role: control-plane
    ip: 10.10.20.11
  - host: wrk-01
    role: worker
    ip: 10.10.20.21
`
	svcCredRef  = "ref+op://vault/git/token"
	svcUser     = "deploy"
	svcPassword = "git-read-token-5d1e"
	// svcFailRef is the reference the per-reference failure tests fail; the bundle and all/base.yaml both hold it.
	svcFailRef = "ref+op://lab/bundle/cluster.id"
	svcKind    = "notFound"
)

// svcFiles is a definition under lab/ that loads and renders, with a large file so that half a pack loses part of the
// tree, and an unrelated file under other/.
func svcFiles(t *testing.T) map[string]string {
	t.Helper()
	bundle, err := os.ReadFile(svcBundle + "secrets.yaml")
	if err != nil {
		t.Fatalf("read the secrets bundle: %v", err)
	}
	return map[string]string{
		"lab/topf.yaml":                 svcTopf,
		"lab/secrets.yaml":              string(bundle),
		"lab/all/base.yaml":             "machine:\n  token: " + svcFailRef + "\n",
		"lab/control-plane/cp.yaml.tpl": "machine:\n  network:\n    hostname: {{ .Node.Host }}\n",
		"lab/big.bin":                   noise(64 << 10),
		"other/note.txt":                "n",
	}
}

// svcValues are the resolved values of every reference of svcFiles.
func svcValues(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(svcBundle + "values.json")
	if err != nil {
		t.Fatalf("read the bundle values: %v", err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode the bundle values: %v", err)
	}
	return values
}

// fakeResolver resolves from a fixed value set and records every call.
type fakeResolver struct {
	mu     sync.Mutex
	values map[string]string
	// fail maps a reference to the kind of its per-reference failure.
	fail map[string]string
	// whole, when set, fails every call as a whole.
	whole error
	// block makes the next call wait for its context; entered is closed when it starts waiting.
	block   bool
	entered chan struct{}
	calls   [][]string
}

func newFakeResolver(values map[string]string) *fakeResolver {
	return &fakeResolver{values: values, fail: map[string]string{}, entered: make(chan struct{})}
}

func (f *fakeResolver) ResolveAll(ctx context.Context, refs []string) (map[string]secrets.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(refs))
	block, whole := f.block, f.whole
	f.block = false
	f.mu.Unlock()

	if block {
		close(f.entered)
		<-ctx.Done()
		return nil, &secrets.WholeCallError{Err: ctx.Err()}
	}
	if whole != nil {
		return nil, whole
	}
	out := make(map[string]secrets.Result, len(refs))
	for _, ref := range refs {
		if kind, ok := f.fail[ref]; ok {
			out[ref] = secrets.Result{Err: &secrets.ReferenceError{Ref: ref, Kind: kind}}
			continue
		}
		if v, ok := f.values[ref]; ok {
			out[ref] = secrets.Result{Value: secret.Secret(v)}
		}
	}
	return out, nil
}

// set changes the fake under its lock.
func (f *fakeResolver) set(change func(f *fakeResolver)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeResolver) seen() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// svcHarness is a git server holding the definition, a database with cluster lab imported from it, and what a
// service needs around them.
type svcHarness struct {
	t      *testing.T
	srv    *gittest.Server
	repo   *Repository
	d      *db.DB
	fake   *fakeResolver
	gitDir string
	now    time.Time
	reader *sdkmetric.ManualReader
	meter  metric.Meter
	id     string

	mu   sync.Mutex
	errs []error
}

// newSvcHarness imports cluster lab from srv's default repository, path lab, naming credentialRef when it is not empty.
func newSvcHarness(t *testing.T, srv *gittest.Server, credentialRef string) *svcHarness {
	t.Helper()
	repo, d := testRepo(t, dbtest.SQLite)
	reader := sdkmetric.NewManualReader()
	h := &svcHarness{
		t: t, srv: srv, repo: repo, d: d, fake: newFakeResolver(svcValues(t)),
		gitDir: filepath.Join(t.TempDir(), "git"), now: gitT0, reader: reader,
		meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("clusters"),
	}
	user := ""
	if credentialRef != "" {
		user = svcUser
	}
	c := mustImport(t, repo, ImportRequest{
		Name: labName, RepoURL: srv.URL(gittest.DefaultRepo), Ref: gitBranch, Subpath: gitLab,
		Username: user, CredentialRef: credentialRef, At: gitT0,
	})
	h.id = c.ID
	return h
}

func (h *svcHarness) onError(_ context.Context, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, err)
}

// reported is the text of every error the services told OnError of.
func (h *svcHarness) reported() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, err := range h.errs {
		out = append(out, err.Error())
	}
	return out
}

// service is a new service over the harness, trusting its git server, after Startup.
func (h *svcHarness) service(res Resolver) *Service {
	h.t.Helper()
	s, err := NewService(Config{
		Repo: h.repo, Resolver: res, GitDir: h.gitDir, Now: func() time.Time { return h.now }, OnError: h.onError, Meter: h.meter,
	})
	if err != nil {
		h.t.Fatalf("NewService: %v", err)
	}
	s.git.caBundle = h.srv.CAPEM()
	if err := s.Startup(h.t.Context()); err != nil {
		h.t.Fatalf("Startup: %v", err)
	}
	return s
}

func (h *svcHarness) poll(s *Service) { s.poll(h.t.Context()) }

func (h *svcHarness) cluster() Cluster { return mustGet(h.t, h.repo, labName) }

func (h *svcHarness) errors() ClusterErrors {
	h.t.Helper()
	e, err := h.repo.Errors(h.t.Context(), h.id)
	if err != nil {
		h.t.Fatalf("Errors: %v", err)
	}
	return e
}

func (h *svcHarness) pair(id string) Pair {
	h.t.Helper()
	p, err := h.repo.Pair(h.t.Context(), id)
	if err != nil {
		h.t.Fatalf("Pair(%s): %v", id, err)
	}
	return p
}

// counter is the sum of the data points of the int64 counter name whose attribute key is value.
func (h *svcHarness) counter(name, key, value string) int64 {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(h.t.Context(), &rm); err != nil {
		h.t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				h.t.Fatalf("%s is a %T, want Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, has := dp.Attributes.Value(attribute.Key(key)); has && v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// cached reports whether the service's tree cache holds the pair.
func cached(s *Service, pairID string) bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	_, ok := s.cache[pairID]
	return ok
}

// approveForTest approves the cluster's pending pair with plain SQL, as the approval Task 17 adds will.
func approveForTest(t *testing.T, d *db.DB, clusterID, pairID string) {
	t.Helper()
	execRaw(t, d, "UPDATE cluster_pairs SET approved_at = ?, approved_by = ? WHERE id = ?", stamp(gitT0), fleet.ActorCLI, pairID)
	execRaw(t, d, "UPDATE clusters SET approved_pair_id = ?, pending_pair_id = NULL WHERE id = ?", pairID, clusterID)
}

func TestNewService_RequiresDependencies(t *testing.T) {
	_, err := NewService(Config{})
	if err == nil {
		t.Fatal("NewService(Config{}) succeeded")
	}
	for _, name := range []string{"Repo", "GitDir", "Now", "OnError"} {
		if !strings.Contains(err.Error(), "Config."+name) {
			t.Errorf("NewService(Config{}) = %q; want it to name Config.%s", err, name)
		}
	}
}

func TestService_NewHeadTreeBecomesPending(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	commit := srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)

	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" {
		t.Fatalf("no pending pair after a poll; reported %q", h.reported())
	}
	tree := srv.Tree(t, commit, gitLab)
	if got := h.pair(c.PendingPairID).TreeHash; got != tree {
		t.Errorf("pending pair tree = %s, want %s", got, tree)
	}
	if c.LastHeadTree != tree {
		t.Errorf("last head tree = %q, want %s", c.LastHeadTree, tree)
	}
	wantNoMark(t, c)
	want := [][]string{slices.Sorted(maps.Keys(svcValues(t)))}
	if got := h.fake.seen(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("resolver calls = %q, want %q", got, want)
	}
	if !cached(s, c.PendingPairID) {
		t.Error("the pending pair is not in the tree cache")
	}
	if n := h.counter("hephaestus.git.fetches", "outcome", "ok"); n != 1 {
		t.Errorf("hephaestus.git.fetches{outcome=ok} = %d, want 1", n)
	}
	if got := h.reported(); len(got) != 0 {
		t.Errorf("reported %q, want nothing", got)
	}
}

func TestService_CommitOutsideSubpathNoPair(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	h.poll(s)
	first := h.cluster().PendingPairID

	srv.Commit(t, map[string]string{"other/note.txt": "changed"}, nil)
	h.now = h.now.Add(FetchInterval)
	h.poll(s)

	c := h.cluster()
	if !c.LastFetchAt.Equal(h.now) {
		t.Errorf("last fetch = %v, want the second poll's %v", c.LastFetchAt, h.now)
	}
	if c.PendingPairID != first {
		t.Errorf("pending pair = %s, want %s unchanged", c.PendingPairID, first)
	}
	if n := len(h.fake.seen()); n != 1 {
		t.Errorf("resolver called %d times, want 1", n)
	}
}

func TestService_LoadRefusalStoredFailed(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	files := svcFiles(t)
	files["lab/topf.yaml"] += "nodesProvider: /bin/x\n"
	commit := srv.Commit(t, files, nil)
	s := h.service(h.fake)

	h.poll(s)

	c := h.cluster()
	if tree := srv.Tree(t, commit, gitLab); c.FailedTree != tree || c.LastHeadTree != tree {
		t.Errorf("failed tree = %q, last head = %q; want both %s", c.FailedTree, c.LastHeadTree, tree)
	}
	if got := h.errors().Failed; !strings.Contains(got, "nodesProvider set") {
		t.Errorf("failed error = %q, want it to hold %q", got, "nodesProvider set")
	}
	if c.PendingPairID != "" {
		t.Errorf("pending pair = %s, want none", c.PendingPairID)
	}
	if n := len(h.fake.seen()); n != 0 {
		t.Errorf("resolver called %d times for a refused tree, want 0", n)
	}
	if got := h.reported(); len(got) != 1 || !strings.Contains(got[0], "topf.yaml: nodesProvider set") {
		t.Errorf("reported %q, want one report naming the file and rule", got)
	}
}

func TestService_FailedTreeErrorIsSealed(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.fail[svcFailRef] = svcKind })
	s := h.service(h.fake)

	h.poll(s)

	text := h.errors().Failed
	if text == "" {
		t.Fatal("no failed error stored")
	}
	var rows []map[string]any
	if err := h.d.Bun().NewSelect().TableExpr("clusters").Scan(t.Context(), &rows); err != nil {
		t.Fatalf("read clusters: %v", err)
	}
	for _, row := range rows {
		for column, v := range row {
			var plain string
			switch x := v.(type) {
			case []byte:
				plain = string(x)
			default:
				plain = fmt.Sprint(x)
			}
			if strings.Contains(plain, text) {
				t.Errorf("column %s holds the failed error %q in plaintext", column, text)
			}
		}
	}
}

func TestService_PerReferenceFailureFailsTree(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	commit := srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.fail[svcFailRef] = svcKind })
	s := h.service(h.fake)

	h.poll(s)

	c := h.cluster()
	if want := "reference " + svcFailRef + ": " + svcKind; h.errors().Failed != want {
		t.Errorf("failed error = %q, want %q", h.errors().Failed, want)
	}
	if tree := srv.Tree(t, commit, gitLab); c.FailedTree != tree {
		t.Errorf("failed tree = %q, want %s", c.FailedTree, tree)
	}
	if c.PendingPairID != "" || c.Attempts != 0 {
		t.Errorf("pending pair = %q, attempts = %d; want none and 0", c.PendingPairID, c.Attempts)
	}
	for _, got := range h.reported() {
		if strings.Contains(got, svcFailRef) || strings.Contains(got, svcKind) {
			t.Errorf("report %q quotes the reference or its failure", got)
		}
	}
}

func TestService_NoTokenFailsTree(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	commit := srv.Commit(t, svcFiles(t), nil)
	s := h.service(nil)

	h.poll(s)

	c := h.cluster()
	if got := h.errors().Failed; got != "HEPHAESTUS_OP_TOKEN is not set" {
		t.Errorf("failed error = %q, want %q", got, "HEPHAESTUS_OP_TOKEN is not set")
	}
	if tree := srv.Tree(t, commit, gitLab); c.FailedTree != tree || c.PendingPairID != "" {
		t.Errorf("failed tree = %q, pending = %q; want %s and none", c.FailedTree, c.PendingPairID, tree)
	}
}

func TestService_InterruptedTreeNotReprocessed(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	commit := srv.Commit(t, svcFiles(t), nil)
	tree := srv.Tree(t, commit, gitLab)
	mustMark(t, h.repo, h.cluster(), tree, TriggerHead, gitT0) // a process that died mid-work left its mark

	s := h.service(h.fake)
	c := h.cluster()
	if c.FailedTree != tree || c.LastHeadTree != tree || h.errors().Failed != "interrupted" {
		t.Fatalf("after Startup: failed = %q (%q), last head = %q; want %s interrupted", c.FailedTree, h.errors().Failed, c.LastHeadTree, tree)
	}
	wantNoMark(t, c)

	h.poll(s)

	if n := len(h.fake.seen()); n != 0 {
		t.Errorf("resolver called %d times for an interrupted tree, want 0", n)
	}
	if c := h.cluster(); c.PendingPairID != "" || c.FailedTree != tree {
		t.Errorf("after a poll: pending = %q, failed = %q; want none and %s", c.PendingPairID, c.FailedTree, tree)
	}
}

func TestService_GracefulShutdownReprocesses(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.block = true })
	s := h.service(h.fake)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-h.fake.entered:
	case <-time.After(time.Minute):
		t.Fatalf("the first poll did not reach the resolver; reported %q", h.reported())
	}
	mid := h.cluster()
	if mid.ProcessingTree == "" {
		t.Fatal("no processing mark while the resolver works")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	c := h.cluster()
	wantNoMark(t, c)
	if c.LastHeadTree != "" || c.FailedTree != "" || c.Attempts != 0 {
		t.Fatalf("after shutdown: last head = %q, failed = %q, attempts = %d; want none, none, 0", c.LastHeadTree, c.FailedTree, c.Attempts)
	}
	if got := h.reported(); len(got) != 0 {
		t.Errorf("reported %q on shutdown, want nothing", got)
	}

	next := h.service(h.fake)
	h.poll(next)

	if c := h.cluster(); c.PendingPairID == "" || c.LastHeadTree != mid.ProcessingTree {
		t.Errorf("after restart: pending = %q, last head = %q; want a pair of %s", c.PendingPairID, c.LastHeadTree, mid.ProcessingTree)
	}
}

func TestService_SetSameHeadTreeYieldsPending(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	files := svcFiles(t)
	srv.Commit(t, files, nil)
	srv.CommitTo(t, "mirror", files, nil)
	s := h.service(h.fake)
	h.poll(s)
	first := h.cluster()

	mirror := srv.URL("mirror")
	mustSet(t, h.repo, SetRequest{Name: labName, RepoURL: &mirror, At: gitT0})
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" || c.PendingPairID == first.PendingPairID {
		t.Fatalf("pending pair = %q after the set; want a new pair (was %s)", c.PendingPairID, first.PendingPairID)
	}
	if got := h.pair(c.PendingPairID).TreeHash; got != first.LastHeadTree {
		t.Errorf("new pair tree = %s, want the same tree %s", got, first.LastHeadTree)
	}
	if n := len(h.fake.seen()); n != 2 {
		t.Errorf("resolver called %d times, want 2", n)
	}
}

func TestService_HeadWholeCallBacksOff(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.whole = &secrets.WholeCallError{Err: errors.New("transport"), Panicked: true} })
	s := h.service(h.fake)

	h.poll(s)

	c := h.cluster()
	wantNoMark(t, c)
	if c.Attempts != 1 || !c.NextAttemptAt.Equal(gitT0.Add(fleet.Backoff(1))) || c.LastHeadTree != "" {
		t.Fatalf("after a whole-call failure: attempts = %d, next = %v, last head = %q; want 1, %v, none",
			c.Attempts, c.NextAttemptAt, c.LastHeadTree, gitT0.Add(fleet.Backoff(1)))
	}
	if h.errors().LastCall == "" {
		t.Error("no last call error stored")
	}
	if s.clientFailures != 1 || !s.clientPanicked {
		t.Errorf("client failures = %d, panicked = %v; want 1, true", s.clientFailures, s.clientPanicked)
	}

	h.fake.set(func(f *fakeResolver) { f.whole = nil })
	h.now = gitT0.Add(fleet.Backoff(1) - time.Second)
	h.poll(s)
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("resolver called %d times before next_attempt_at, want 1", n)
	}

	h.now = gitT0.Add(fleet.Backoff(1))
	h.poll(s)
	if n := len(h.fake.seen()); n != 2 {
		t.Fatalf("resolver called %d times after next_attempt_at, want 2", n)
	}
	c = h.cluster()
	if c.PendingPairID == "" || c.Attempts != 0 || h.errors().LastCall != "" {
		t.Errorf("after the retry: pending = %q, attempts = %d, last call = %q; want a pair, 0, none", c.PendingPairID, c.Attempts, h.errors().LastCall)
	}
	if s.clientFailures != 0 || s.clientPanicked {
		t.Errorf("client failures = %d, panicked = %v after a success; want 0, false", s.clientFailures, s.clientPanicked)
	}
}

func TestService_HeadRateLimitedWaitsForDaily(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	limited := fmt.Errorf("%w: %w", secrets.ErrRateLimited, &secrets.WholeCallError{Err: errors.New("429")})
	h.fake.set(func(f *fakeResolver) { f.whole = limited })
	s := h.service(h.fake)

	h.poll(s)

	if c := h.cluster(); !c.RateLimitedUntil.Equal(gitT0.Add(DailyInterval)) {
		t.Fatalf("rate limited until %v, want %v", c.RateLimitedUntil, gitT0.Add(DailyInterval))
	}
	h.fake.set(func(f *fakeResolver) { f.whole = nil })
	h.now = gitT0.Add(time.Hour)
	h.poll(s)
	if n := len(h.fake.seen()); n != 1 {
		t.Errorf("resolver called %d times while rate limited, want 1", n)
	}
}

func TestService_CredentialResolvedBeforeFetch(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{BasicAuth: &[2]string{svcUser, svcPassword}})
	h := newSvcHarness(t, srv, svcCredRef)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.values[svcCredRef] = svcPassword })
	s := h.service(h.fake)

	h.poll(s)

	calls := h.fake.seen()
	if len(calls) == 0 || !slices.Equal(calls[0], []string{svcCredRef}) {
		t.Fatalf("resolver calls = %q, want the credential first", calls)
	}
	if got, ok, err := h.repo.Credential(t.Context(), h.id); err != nil || !ok || got.Expose() != svcPassword {
		t.Errorf("stored credential ok = %v, err = %v; want the resolved value", ok, err)
	}
	c := h.cluster()
	if fetchErr := h.errors().Fetch; fetchErr != "" || c.LastFetchAt.IsZero() {
		t.Errorf("fetch error = %q, last fetch = %v; want a successful fetch", fetchErr, c.LastFetchAt)
	}
	if c.PendingPairID == "" {
		t.Errorf("no pending pair after the authenticated fetch; reported %q", h.reported())
	}
}

func TestService_MistypedCredentialNotRetried(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{BasicAuth: &[2]string{svcUser, svcPassword}})
	h := newSvcHarness(t, srv, svcCredRef)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.fail[svcCredRef] = svcKind })
	s := h.service(h.fake)

	h.poll(s)

	if want := "credential " + svcCredRef + ": " + svcKind; h.errors().Fetch != want {
		t.Fatalf("fetch error = %q, want %q", h.errors().Fetch, want)
	}
	for range 10 {
		h.now = h.now.Add(FetchInterval)
		h.poll(s)
	}
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("resolver called %d times over eleven polls, want 1", n)
	}
	if n := h.counter("hephaestus.git.fetches", "outcome", "error") + h.counter("hephaestus.git.fetches", "outcome", "ok"); n != 0 {
		t.Errorf("%d fetches ran without the credential, want 0", n)
	}

	if err := h.repo.RequestRefresh(t.Context(), labName, h.now); err != nil {
		t.Fatalf("RequestRefresh: %v", err)
	}
	h.poll(s)
	h.poll(s)
	if n := len(h.fake.seen()); n != 2 {
		t.Errorf("resolver called %d times after a refresh request, want 2", n)
	}
}

func TestService_FetchDropNoMoveAside(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{DropAfter: 2048})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)

	h.poll(s)

	if h.errors().Fetch == "" {
		t.Fatal("no fetch error recorded for a connection cut mid-fetch")
	}
	if matches, err := filepath.Glob(h.gitDir + ".corrupt-*"); err != nil || len(matches) != 0 {
		t.Errorf("moved-aside stores = %v, %v; want none", matches, err)
	}
	if _, err := os.Stat(h.gitDir); err != nil {
		t.Errorf("the git store: %v; want it in place", err)
	}
	if n := h.counter("hephaestus.git.fetches", "outcome", "error"); n != 1 {
		t.Errorf("hephaestus.git.fetches{outcome=error} = %d, want 1", n)
	}
	if n := h.counter("hephaestus.alerts", "kind", "git_store_moved_aside"); n != 0 {
		t.Errorf("hephaestus.alerts{kind=git_store_moved_aside} = %d, want 0", n)
	}
	host := strings.TrimPrefix(srv.URL(gittest.DefaultRepo), "https://")
	for _, got := range h.reported() {
		if strings.Contains(got, host) {
			t.Errorf("report %q quotes the repository URL", got)
		}
	}
}

func TestService_MoveAsideRefillsCache(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	first := h.service(h.fake)
	h.poll(first)
	pairID := h.cluster().PendingPairID
	if pairID == "" {
		t.Fatalf("no pending pair; reported %q", h.reported())
	}
	approveForTest(t, h.d, h.id, pairID)

	packs, err := filepath.Glob(filepath.Join(h.gitDir, h.id+".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("pack files = %v, %v; want exactly one", packs, err)
	}
	info, err := os.Stat(packs[0])
	if err != nil {
		t.Fatalf("stat pack: %v", err)
	}
	if err := os.Chmod(packs[0], 0o600); err != nil { // go-git writes packs read-only
		t.Fatalf("make the pack writable: %v", err)
	}
	if err := os.Truncate(packs[0], info.Size()/2); err != nil {
		t.Fatalf("truncate pack: %v", err)
	}

	restarted := h.service(h.fake)

	if !cached(restarted, pairID) {
		t.Fatalf("the approved pair is not cached after the move-aside; reported %q", h.reported())
	}
	if matches, err := filepath.Glob(h.gitDir + ".corrupt-*"); err != nil || len(matches) != 1 {
		t.Fatalf("moved-aside stores = %v, %v; want one", matches, err)
	}
	moved := h.gitDir + ".corrupt-20261119T120000Z"
	wantOneAudit(t, h.d, "cluster.store_moved_aside", fleet.ActorSystem, `{"moved":"`+moved+`"}`)
	if n := h.counter("hephaestus.alerts", "kind", "git_store_moved_aside"); n != 1 {
		t.Errorf("hephaestus.alerts{kind=git_store_moved_aside} = %d, want 1", n)
	}
}

func TestService_CacheDropsReplacedPairs(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	h.poll(s)
	first := h.cluster().PendingPairID

	srv.Commit(t, map[string]string{"lab/notes.txt": "a second head tree"}, nil)
	h.now = h.now.Add(FetchInterval)
	h.poll(s)
	second := h.cluster().PendingPairID
	if second == "" || second == first {
		t.Fatalf("pending pair = %q after a second head tree; want a new pair (was %s)", second, first)
	}
	h.poll(s)

	if cached(s, first) {
		t.Error("the replaced pair is still cached")
	}
	if !cached(s, second) {
		t.Error("the pending pair is not cached")
	}
}

// literalFiles is svcFiles with every reference replaced by its value, so the tree holds no reference at all.
func literalFiles(t *testing.T) map[string]string {
	t.Helper()
	files := svcFiles(t)
	values := svcValues(t)
	refs := slices.SortedFunc(maps.Keys(values), func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(refs))
	for _, ref := range refs {
		pairs = append(pairs, ref, strconv.Quote(values[ref]))
	}
	files["lab/secrets.yaml"] = strings.NewReplacer(pairs...).Replace(files["lab/secrets.yaml"])
	delete(files, "lab/all/base.yaml")
	if strings.Contains(files["lab/secrets.yaml"], "ref+") {
		t.Fatal("the literal bundle still holds a reference")
	}
	return files
}

func TestService_NoReferencesSkipsResolver(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	commit := srv.Commit(t, literalFiles(t), nil)
	s := h.service(h.fake)

	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" {
		t.Fatalf("no pending pair for a tree without references; reported %q", h.reported())
	}
	if got := h.pair(c.PendingPairID).TreeHash; got != srv.Tree(t, commit, gitLab) {
		t.Errorf("pending pair tree = %s, want %s", got, srv.Tree(t, commit, gitLab))
	}
	if calls := h.fake.seen(); len(calls) != 0 {
		t.Errorf("resolver calls = %q for a tree without references, want none", calls)
	}
}

func TestService_SetWithFailingCredentialStoresNothing(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	files := svcFiles(t)
	files["lab/notes.txt"] = "the new source's tree"
	srv.CommitTo(t, "mirror", files, nil)
	h.fake.set(func(f *fakeResolver) { f.fail[svcCredRef] = svcKind })
	s := h.service(h.fake)
	h.poll(s)
	if h.cluster().PendingPairID == "" {
		t.Fatalf("no pending pair from the first source; reported %q", h.reported())
	}

	mirror, user, ref := srv.URL("mirror"), svcUser, svcCredRef
	set := mustSet(t, h.repo, SetRequest{Name: labName, RepoURL: &mirror, Username: &user, CredentialRef: &ref, At: gitT0})
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID != "" || c.LastHeadTree != "" || c.FailedTree != "" {
		t.Errorf("at revision %d: pending = %q, last head = %q, failed = %q; want nothing stored before the new source is fetched",
			set.Revision, c.PendingPairID, c.LastHeadTree, c.FailedTree)
	}
	wantNoMark(t, c)
	if calls := h.fake.seen(); len(calls) != 2 || !slices.Equal(calls[1], []string{svcCredRef}) {
		t.Errorf("resolver calls = %q; want the first head tree, then the credential only", calls)
	}
}

func TestService_UnreleasedMarkRetried(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	other := mustImport(t, h.repo, ImportRequest{Name: labName + "2", RepoURL: srv.URL(gittest.DefaultRepo), Ref: gitBranch, Subpath: gitLab, At: gitT0})
	// Cluster lab's mark cannot be cleared: its store fails, and so does the release after it.
	execRaw(t, h.d, "CREATE TRIGGER keep_mark BEFORE UPDATE ON clusters WHEN OLD.id = '"+h.id+
		"' AND OLD.processing_tree IS NOT NULL AND NEW.processing_tree IS NULL BEGIN SELECT RAISE(ABORT, 'injected'); END")
	s := h.service(h.fake)

	h.poll(s)

	if c := h.cluster(); c.ProcessingTree == "" || c.PendingPairID != "" {
		t.Fatalf("lab: mark = %q, pending = %q; want the mark kept and no pair", c.ProcessingTree, c.PendingPairID)
	}
	if c := mustGet(t, h.repo, other.Name); c.PendingPairID == "" {
		t.Fatalf("%s: no pending pair; reported %q", other.Name, h.reported())
	}

	execRaw(t, h.d, "DROP TRIGGER keep_mark")
	h.poll(s)

	c := h.cluster()
	wantNoMark(t, c)
	if c.PendingPairID == "" {
		t.Errorf("lab: no pending pair once its mark could be cleared; reported %q", h.reported())
	}
}
