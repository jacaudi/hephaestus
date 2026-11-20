package clusters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/datakey"
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
	want := [][]string{svcRefs(t)}
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

const (
	// svcSecretRef is a reference of the bundle only, whose value the refresh tests change.
	svcSecretRef = "ref+op://lab/bundle/cluster.secret"
	// svcChanged is a new value for svcSecretRef: base64 of 32 bytes, as the value it replaces.
	svcChanged = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	// svcNewPassword is the read credential after its rotation in 1Password.
	svcNewPassword = "git-read-token-9c2b"
)

// svcRefs is every reference of svcFiles, in the order Load gives them.
func svcRefs(t *testing.T) []string {
	t.Helper()
	return slices.Sorted(maps.Keys(svcValues(t)))
}

// requestRefresh asks for a refresh of cluster lab, then moves the clock to the next poll.
func (h *svcHarness) requestRefresh() {
	h.t.Helper()
	if err := h.repo.RequestRefresh(h.t.Context(), labName, h.now); err != nil {
		h.t.Fatalf("RequestRefresh: %v", err)
	}
	h.now = h.now.Add(PollInterval)
}

// values opens a secret version.
func (h *svcHarness) values(versionID string) map[string]string {
	h.t.Helper()
	v, err := h.repo.Values(h.t.Context(), versionID)
	if err != nil {
		h.t.Fatalf("Values(%s): %v", versionID, err)
	}
	return v
}

// pendingAfterPoll polls s once and returns the pending pair it stored.
func (h *svcHarness) pendingAfterPoll(s *Service) Pair {
	h.t.Helper()
	h.poll(s)
	id := h.cluster().PendingPairID
	if id == "" {
		h.t.Fatalf("no pending pair; reported %q", h.reported())
	}
	return h.pair(id)
}

func TestService_RefreshOrder(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	t3 := srv.Tree(t, srv.Commit(t, svcFiles(t), nil), gitLab)
	h.fake.set(func(f *fakeResolver) { f.fail[svcFailRef] = svcKind })
	s := h.service(h.fake)
	h.poll(s)
	if c := h.cluster(); c.FailedTree != t3 {
		t.Fatalf("failed tree = %q, want %s; reported %q", c.FailedTree, t3, h.reported())
	}

	h.fake.set(func(f *fakeResolver) { delete(f.fail, svcFailRef) })
	t4 := srv.Tree(t, srv.Commit(t, map[string]string{"lab/notes.txt": "a newer good tree"}, nil), gitLab)
	h.now = h.now.Add(FetchInterval)
	old := h.pendingAfterPoll(s)
	if c := h.cluster(); old.TreeHash != t4 || c.FailedTree != "" {
		t.Fatalf("pending tree = %s, failed = %q; want %s and none", old.TreeHash, c.FailedTree, t4)
	}

	h.fake.set(func(f *fakeResolver) { f.values[svcSecretRef] = svcChanged })
	h.requestRefresh()
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" || c.PendingPairID == old.ID {
		t.Fatalf("pending pair = %q after a refresh with a changed value; want a new pair (was %s); reported %q", c.PendingPairID, old.ID, h.reported())
	}
	p := h.pair(c.PendingPairID)
	if p.TreeHash != t4 || p.SecretVersionID == old.SecretVersionID {
		t.Errorf("new pair = (%s, %s); want (%s, a new version)", p.TreeHash, p.SecretVersionID, t4)
	}
	if got := h.values(p.SecretVersionID)[svcSecretRef]; got != svcChanged {
		t.Errorf("new version holds %s = %q, want the changed value", svcSecretRef, got)
	}
	if n := count(t, h.d, "secret_versions", "id = ?", old.SecretVersionID); n != 0 {
		t.Errorf("the old version row is still stored (%d rows)", n)
	}
	if c.RefreshRequested || !c.LastRefreshAt.Equal(h.now) || c.LastHeadTree != t4 {
		t.Errorf("refresh requested = %v, last refresh = %v, last head = %q; want false, %v, %s", c.RefreshRequested, c.LastRefreshAt, c.LastHeadTree, h.now, t4)
	}
	wantNoMark(t, c)
	if !cached(s, p.ID) {
		t.Error("the refreshed pair is not in the tree cache")
	}
}

func TestService_RefreshRetriesFailedTreeFirst(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	// The approved tree's only reference is svcFailRef; the failed tree adds the bundle's.
	approvedFiles := literalFiles(t)
	approvedFiles["lab/all/base.yaml"] = svcFiles(t)["lab/all/base.yaml"]
	srv.Commit(t, approvedFiles, nil)
	s := h.service(h.fake)
	approved := h.pendingAfterPoll(s)
	approveForTest(t, h.d, h.id, approved.ID)

	h.fake.set(func(f *fakeResolver) { f.fail[svcSecretRef] = svcKind })
	t3 := srv.Tree(t, srv.Commit(t, svcFiles(t), nil), gitLab)
	h.now = h.now.Add(FetchInterval)
	h.poll(s)
	if c := h.cluster(); c.FailedTree != t3 {
		t.Fatalf("failed tree = %q, want %s; reported %q", c.FailedTree, t3, h.reported())
	}

	h.fake.set(func(f *fakeResolver) { delete(f.fail, svcSecretRef) })
	before := len(h.fake.seen())
	h.requestRefresh()
	h.poll(s)

	if got, want := h.fake.seen()[before:], [][]string{svcRefs(t)}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("resolver calls of the refresh = %q, want the failed tree's references only, %q", got, want)
	}
	c := h.cluster()
	if c.PendingPairID == "" || h.pair(c.PendingPairID).TreeHash != t3 {
		t.Fatalf("pending pair = %q, want a pair of the failed tree %s; reported %q", c.PendingPairID, t3, h.reported())
	}
	if c.FailedTree != "" || h.errors().Failed != "" || c.ApprovedPairID != approved.ID {
		t.Errorf("failed tree = %q (%q), approved = %q; want none and %s", c.FailedTree, h.errors().Failed, c.ApprovedPairID, approved.ID)
	}
	if !c.LastRefreshAt.Equal(h.now) || c.RefreshRequested {
		t.Errorf("last refresh = %v, refresh requested = %v; want %v, false", c.LastRefreshAt, c.RefreshRequested, h.now)
	}
	wantNoMark(t, c)
}

func TestService_RefreshFailedThenGoodTree(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	t1 := srv.Tree(t, srv.Commit(t, svcFiles(t), nil), gitLab)
	s := h.service(h.fake)
	approved := h.pendingAfterPoll(s)
	approveForTest(t, h.d, h.id, approved.ID)

	t2 := srv.Tree(t, srv.Commit(t, map[string]string{"lab/topf.yaml": svcTopf + "nodesProvider: /bin/x\n"}, nil), gitLab)
	h.now = h.now.Add(FetchInterval)
	h.poll(s)
	if c := h.cluster(); c.FailedTree != t2 {
		t.Fatalf("failed tree = %q, want %s; reported %q", c.FailedTree, t2, h.reported())
	}

	h.fake.set(func(f *fakeResolver) { f.values[svcSecretRef] = svcChanged })
	before := len(h.fake.seen())
	h.requestRefresh()
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" {
		t.Fatalf("no pending pair after the refresh; reported %q", h.reported())
	}
	p := h.pair(c.PendingPairID)
	if p.TreeHash != t1 || p.SecretVersionID == approved.SecretVersionID || h.values(p.SecretVersionID)[svcSecretRef] != svcChanged {
		t.Errorf("pending pair = (%s, %s); want (%s, a new version with the changed value)", p.TreeHash, p.SecretVersionID, t1)
	}
	if c.FailedTree != t2 || !strings.Contains(h.errors().Failed, "nodesProvider set") {
		t.Errorf("failed tree = %q (%q); want %s kept with its refusal", c.FailedTree, h.errors().Failed, t2)
	}
	if c.ApprovedPairID != approved.ID || c.LastHeadTree != t2 {
		t.Errorf("approved = %q, last head = %q; want %s and %s", c.ApprovedPairID, c.LastHeadTree, approved.ID, t2)
	}
	if got, want := h.fake.seen()[before:], [][]string{svcRefs(t)}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("resolver calls of the refresh = %q, want the approved tree's only, %q", got, want)
	}
	wantNoMark(t, c)
}

func TestService_RefreshUnchangedValuesNoPair(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	first := h.pendingAfterPoll(s)
	execRaw(t, h.d, "UPDATE clusters SET refresh_incomplete = ? WHERE id = ?", true, h.id)

	h.requestRefresh()
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID != first.ID {
		t.Errorf("pending pair = %q, want %s unchanged", c.PendingPairID, first.ID)
	}
	if n := count(t, h.d, "secret_versions", "cluster_id = ?", h.id); n != 1 {
		t.Errorf("%d secret versions stored, want 1", n)
	}
	if !c.LastRefreshAt.Equal(h.now) || c.RefreshIncomplete || c.RefreshRequested {
		t.Errorf("last refresh = %v, incomplete = %v, requested = %v; want %v, false, false", c.LastRefreshAt, c.RefreshIncomplete, c.RefreshRequested, h.now)
	}
	if n := len(h.fake.seen()); n != 2 {
		t.Errorf("resolver called %d times, want 2", n)
	}
	wantNoMark(t, c)
}

func TestService_PerReferenceFailureNoBackoff(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	first := h.pendingAfterPoll(s)

	h.fake.set(func(f *fakeResolver) { f.fail[svcFailRef] = svcKind })
	h.requestRefresh()
	h.poll(s)

	c := h.cluster()
	if !c.RefreshIncomplete {
		t.Errorf("refresh incomplete = false after a failed reference; reported %q", h.reported())
	}
	if c.Attempts != 0 || !c.NextAttemptAt.IsZero() || c.RefreshRequested {
		t.Errorf("attempts = %d, next = %v, requested = %v; want 0, none, false", c.Attempts, c.NextAttemptAt, c.RefreshRequested)
	}
	if c.PendingPairID != first.ID || c.FailedTree != "" {
		t.Errorf("pending = %q, failed = %q; want %s unchanged and none", c.PendingPairID, c.FailedTree, first.ID)
	}
	wantNoMark(t, c)
	wantOneAudit(t, h.d, "cluster.refresh_incomplete", fleet.ActorSystem, `{"name":"`+labName+`"}`)
	if n := h.counter("hephaestus.alerts", "kind", "refresh_incomplete"); n != 1 {
		t.Errorf("hephaestus.alerts{kind=refresh_incomplete} = %d, want 1", n)
	}
	for _, got := range h.reported() {
		if strings.Contains(got, svcFailRef) || strings.Contains(got, svcKind) {
			t.Errorf("report %q quotes the reference or its failure", got)
		}
	}

	h.now = h.now.Add(FetchInterval)
	h.poll(s)
	if n := len(h.fake.seen()); n != 2 {
		t.Errorf("resolver called %d times, want 2: a failed reference waits for the next trigger", n)
	}
}

func TestService_WholeCallBackoffPersists(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	h.pendingAfterPoll(s)

	h.fake.set(func(f *fakeResolver) { f.whole = &secrets.WholeCallError{Err: errors.New("transport")} })
	h.requestRefresh()
	h.poll(s)

	at := h.now
	c := h.cluster()
	if c.Attempts != 1 || !c.NextAttemptAt.Equal(at.Add(fleet.Backoff(1))) || !c.RefreshRequested {
		t.Fatalf("attempts = %d, next = %v, requested = %v; want 1, %v, true; reported %q",
			c.Attempts, c.NextAttemptAt, c.RefreshRequested, at.Add(fleet.Backoff(1)), h.reported())
	}
	wantNoMark(t, c)
	if h.errors().LastCall == "" {
		t.Error("no last call error stored")
	}

	h.fake.set(func(f *fakeResolver) { f.whole = nil })
	restarted := h.service(h.fake)
	h.now = at.Add(fleet.Backoff(1) - time.Second)
	h.poll(restarted)
	if n := len(h.fake.seen()); n != 2 {
		t.Fatalf("resolver called %d times before next_attempt_at after a restart, want 2", n)
	}

	h.now = at.Add(fleet.Backoff(1))
	h.poll(restarted)
	if n := len(h.fake.seen()); n != 3 {
		t.Fatalf("resolver called %d times at next_attempt_at, want 3", n)
	}
	c = h.cluster()
	if c.Attempts != 0 || c.RefreshRequested || !c.LastRefreshAt.Equal(h.now) || h.errors().LastCall != "" {
		t.Errorf("after the retry: attempts = %d, requested = %v, last refresh = %v, last call = %q; want 0, false, %v, none",
			c.Attempts, c.RefreshRequested, c.LastRefreshAt, h.errors().LastCall, h.now)
	}
}

func TestService_RateLimitPausesUntilDaily(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	h.pendingAfterPoll(s)

	limited := fmt.Errorf("%w: %w", secrets.ErrRateLimited, &secrets.WholeCallError{Err: errors.New("429")})
	h.fake.set(func(f *fakeResolver) { f.whole = limited })
	h.requestRefresh()
	h.poll(s)

	at := h.now
	if c := h.cluster(); !c.RateLimitedUntil.Equal(at.Add(DailyInterval)) || !c.RefreshRequested {
		t.Fatalf("rate limited until %v, requested = %v; want %v, true; reported %q", c.RateLimitedUntil, c.RefreshRequested, at.Add(DailyInterval), h.reported())
	}
	h.fake.set(func(f *fakeResolver) { f.whole = nil })
	for _, d := range []time.Duration{fleet.Backoff(1), time.Hour, DailyInterval - time.Minute} {
		h.now = at.Add(d)
		h.poll(s)
	}
	if n := len(h.fake.seen()); n != 2 {
		t.Fatalf("resolver called %d times while rate limited, want 2", n)
	}

	h.requestRefresh()
	if c := h.cluster(); !c.RateLimitedUntil.IsZero() || !c.NextAttemptAt.IsZero() {
		t.Fatalf("after cluster refresh: rate limited until %v, next attempt %v; want both cleared", c.RateLimitedUntil, c.NextAttemptAt)
	}
	h.poll(s)
	if n := len(h.fake.seen()); n != 3 {
		t.Fatalf("resolver called %d times after cluster refresh, want 3", n)
	}
	if c := h.cluster(); !c.LastRefreshAt.Equal(h.now) || c.Attempts != 0 {
		t.Errorf("last refresh = %v, attempts = %d; want %v, 0", c.LastRefreshAt, c.Attempts, h.now)
	}
}

func TestService_DailyTimer(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	h.pendingAfterPoll(s)

	h.now = gitT0.Add(23 * time.Hour)
	h.poll(s)
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("resolver called %d times 23 hours after import, want 1", n)
	}

	h.now = gitT0.Add(DailyInterval)
	h.poll(s)
	if n := len(h.fake.seen()); n != 2 {
		t.Fatalf("resolver called %d times a day after import, want 2; reported %q", n, h.reported())
	}
	c := h.cluster()
	if !c.LastDailyAt.Equal(h.now) || !c.LastRefreshAt.Equal(h.now) {
		t.Errorf("last daily = %v, last refresh = %v; want both %v", c.LastDailyAt, c.LastRefreshAt, h.now)
	}

	h.now = h.now.Add(PollInterval)
	h.poll(s)
	if n := len(h.fake.seen()); n != 2 {
		t.Errorf("resolver called %d times on the poll after the daily refresh, want 2", n)
	}
}

func TestService_CredentialReResolvedAtRefresh(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{BasicAuth: &[2]string{svcUser, svcPassword}})
	h := newSvcHarness(t, srv, svcCredRef)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.values[svcCredRef] = svcPassword })
	s := h.service(h.fake)
	h.pendingAfterPoll(s)

	h.fake.set(func(f *fakeResolver) { f.values[svcCredRef] = svcNewPassword })
	before := len(h.fake.seen())
	h.requestRefresh()
	h.poll(s)

	got, ok, err := h.repo.Credential(t.Context(), h.id)
	if err != nil || !ok || got.Expose() != svcNewPassword {
		t.Errorf("stored credential ok = %v, err = %v, rotated = %v; want the new value", ok, err, got.Expose() == svcNewPassword)
	}
	if calls := h.fake.seen()[before:]; len(calls) == 0 || !slices.Equal(calls[0], []string{svcCredRef}) {
		t.Errorf("resolver calls of the refresh = %q, want the credential first, in its own call", calls)
	}
	if c := h.cluster(); !c.LastRefreshAt.Equal(h.now) {
		t.Errorf("last refresh = %v, want %v", c.LastRefreshAt, h.now)
	}
}

func TestService_RefreshWithoutTokenSkipped(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.pendingAfterPoll(h.service(h.fake))

	s := h.service(nil)
	h.requestRefresh()
	h.poll(s)
	h.now = gitT0.Add(DailyInterval)
	h.poll(s)

	c := h.cluster()
	if !c.RefreshRequested || !c.LastDailyAt.IsZero() || !c.LastRefreshAt.IsZero() {
		t.Errorf("requested = %v, last daily = %v, last refresh = %v; want the triggers left pending", c.RefreshRequested, c.LastDailyAt, c.LastRefreshAt)
	}
	wantNoMark(t, c)
	if got := h.reported(); len(got) != 0 {
		t.Errorf("reported %q, want nothing: secrets:token reports a missing token", got)
	}
}

func TestService_RefreshOnHeadPollKeepsNewTree(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.fake.set(func(f *fakeResolver) { f.fail[svcFailRef] = svcKind })
	s := h.service(h.fake)
	h.poll(s)
	if h.cluster().FailedTree == "" {
		t.Fatalf("no failed tree; reported %q", h.reported())
	}

	// One poll fetches a newer good tree while a refresh is due: the refresh must see the pair the head work stored.
	h.fake.set(func(f *fakeResolver) { delete(f.fail, svcFailRef) })
	t4 := srv.Tree(t, srv.Commit(t, map[string]string{"lab/notes.txt": "a newer good tree"}, nil), gitLab)
	h.requestRefresh()
	h.now = h.now.Add(FetchInterval)
	h.poll(s)

	c := h.cluster()
	if c.PendingPairID == "" || h.pair(c.PendingPairID).TreeHash != t4 {
		t.Fatalf("pending pair = %q; want a pair of the new head tree %s; reported %q", c.PendingPairID, t4, h.reported())
	}
	if c.FailedTree != "" || c.RefreshRequested || !c.LastRefreshAt.Equal(h.now) {
		t.Errorf("failed = %q, requested = %v, last refresh = %v; want none, false, %v", c.FailedTree, c.RefreshRequested, c.LastRefreshAt, h.now)
	}
	wantNoMark(t, c)
}

func TestService_RefreshWithoutTreeWaits(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{BasicAuth: &[2]string{svcUser, svcPassword}})
	h := newSvcHarness(t, srv, svcCredRef)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, map[string]string{"other/note.txt": "no cluster definition yet"}, nil)
	h.fake.set(func(f *fakeResolver) { f.values[svcCredRef] = svcPassword })
	s := h.service(h.fake)
	h.poll(s)

	h.requestRefresh()
	for range 3 {
		h.poll(s)
		h.now = h.now.Add(PollInterval)
	}

	if calls := h.fake.seen(); len(calls) != 1 {
		t.Errorf("resolver calls = %q; want the first credential resolution only, while there is no tree to refresh", calls)
	}
	if c := h.cluster(); !c.RefreshRequested {
		t.Error("the refresh request was consumed with no tree to refresh")
	}
}

func TestService_RefreshRenderErrorIncomplete(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	t1 := srv.Tree(t, srv.Commit(t, svcFiles(t), nil), gitLab)
	s := h.service(h.fake)
	first := h.pendingAfterPoll(s)

	// A value the bundle cannot hold: the tree resolves, but does not render.
	h.fake.set(func(f *fakeResolver) { f.values["ref+op://lab/bundle/certs.os.crt"] = "not a certificate" })
	h.requestRefresh()
	h.poll(s)

	c := h.cluster()
	if !c.RefreshIncomplete || c.RefreshRequested {
		t.Errorf("incomplete = %v, requested = %v; want true, false; reported %q", c.RefreshIncomplete, c.RefreshRequested, h.reported())
	}
	if c.FailedTree != "" || h.errors().Failed != "" || c.LastHeadTree != t1 || c.PendingPairID != first.ID {
		t.Errorf("failed = %q (%q), last head = %q, pending = %q; want none, %s, %s unchanged",
			c.FailedTree, h.errors().Failed, c.LastHeadTree, c.PendingPairID, t1, first.ID)
	}
	wantNoMark(t, c)
	wantOneAudit(t, h.d, "cluster.refresh_incomplete", fleet.ActorSystem, `{"name":"`+labName+`"}`)
	if n := h.counter("hephaestus.alerts", "kind", "refresh_incomplete"); n != 1 {
		t.Errorf("hephaestus.alerts{kind=refresh_incomplete} = %d, want 1", n)
	}
	if got := h.reported(); len(got) != 1 || !strings.HasSuffix(got[0], "the refresh is incomplete: the tree does not render") {
		t.Errorf("reported %q, want one report that the refreshed tree does not render", got)
	}
}

// svcCP is the control-plane host of svcFiles.
const svcCP = "cp-01"

// approvedAfterPoll polls s once and approves the pending pair it stored.
func (h *svcHarness) approvedAfterPoll(s *Service) Pair {
	h.t.Helper()
	p := h.pendingAfterPoll(s)
	approveForTest(h.t, h.d, h.id, p.ID)
	return p
}

// ticket is a ticket of host on pair p.
func (h *svcHarness) ticket(p Pair, host string) fleet.Ticket {
	return fleet.Ticket{ClusterID: h.id, HostName: host, PairID: p.ID, SecretVersionID: p.SecretVersionID}
}

// hostRow is the stored render of host in the pair.
func (h *svcHarness) hostRow(pairID, host string) HostRow {
	h.t.Helper()
	rows, err := h.repo.HostRows(h.t.Context(), pairID)
	if err != nil {
		h.t.Fatalf("HostRows(%s): %v", pairID, err)
	}
	for i := range rows {
		if rows[i].Host == host {
			return rows[i]
		}
	}
	h.t.Fatalf("pair %s has no render of %s", pairID, host)
	return HostRow{}
}

// alterHash stores another config hash for host in the pair, so the next render of it differs from the stored one.
func (h *svcHarness) alterHash(pairID, host string) {
	h.t.Helper()
	execRaw(h.t, h.d, "UPDATE host_renders SET config_hash = ? WHERE pair_id = ? AND host_name = ?", "a-hash-no-render-has", pairID, host)
}

// wantDrift fails unless host's row in the pair records want, the render of the host, as its drift.
func (h *svcHarness) wantDrift(pairID, host string, want HostRow) {
	h.t.Helper()
	got := h.hostRow(pairID, host)
	if !got.Drift || got.DriftConfigHash != want.ConfigHash || got.DriftDisplayHash != want.DisplayHash || !bytes.Equal(got.DriftRedacted, want.Redacted) {
		h.t.Errorf("drift of %s = %v (%s, %s); want true with the render's hashes (%s, %s) and redacted text",
			host, got.Drift, got.DriftConfigHash, got.DriftDisplayHash, want.ConfigHash, want.DisplayHash)
	}
}

// hashOf is hex(sha256(b)), as a render's config hash.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// damageVersion flips a byte of the sealed value set of the version.
func damageVersion(t *testing.T, d *db.DB, versionID string) {
	t.Helper()
	sealed := scalar[[]byte](t, d, "SELECT values_sealed FROM secret_versions WHERE id = ?", versionID)
	sealed[len(sealed)-1] ^= 0xff
	execRaw(t, d, "UPDATE secret_versions SET values_sealed = ? WHERE id = ?", sealed, versionID)
}

func TestService_RenderUsesTicketPair(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)

	srv.Commit(t, map[string]string{"lab/control-plane/cp.yaml.tpl": "machine:\n  network:\n    hostname: {{ .Node.Host }}-next\n"}, nil)
	h.now = h.now.Add(FetchInterval)
	pending := h.pendingAfterPoll(s)
	want := h.hostRow(approved.ID, svcCP).ConfigHash
	if h.hostRow(pending.ID, svcCP).ConfigHash == want {
		t.Fatal("the pending pair renders cp-01 as the approved one does")
	}

	config, err := s.Render(t.Context(), h.ticket(approved, svcCP))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := hashOf(config); got != want {
		t.Errorf("config hash = %s, want the approved pair's %s", got, want)
	}
}

func TestService_RenderCacheMissNoDrift(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	s.cacheMu.Lock()
	delete(s.cache, approved.ID)
	s.cacheMu.Unlock()

	// A pair still cached that a newer pending pair replaced in the database meanwhile.
	srv.Commit(t, map[string]string{"lab/notes.txt": "a second head tree"}, nil)
	h.now = h.now.Add(FetchInterval)
	replaced := h.pendingAfterPoll(s)
	srv.Commit(t, map[string]string{"lab/notes.txt": "a third head tree"}, nil)
	h.now = h.now.Add(FetchInterval)
	h.pendingAfterPoll(s)
	if !cached(s, replaced.ID) {
		t.Fatal("the replaced pair is no longer cached")
	}

	for _, tt := range []struct {
		name string
		p    Pair
	}{
		{"not cached", approved},
		{"deleted meanwhile", replaced},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Render(t.Context(), h.ticket(tt.p, svcCP))
			if !errors.Is(err, ErrNoRender) {
				t.Errorf("Render = %v, want ErrNoRender", err)
			}
		})
	}
	if row := h.hostRow(approved.ID, svcCP); row.Drift {
		t.Error("a pair the cache lacks set drift")
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) after a cache miss, want pass", res.Status, res.Output)
	}
	s.failRender(approved.ID, svcCP)
	if _, err := s.Render(t.Context(), h.ticket(approved, svcCP)); !errors.Is(err, ErrNoRender) {
		t.Fatalf("Render = %v, want ErrNoRender", err)
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusFail {
		t.Errorf("render:approved = %s after a cache miss over a failing render, want it still failing", res.Status)
	}
}

func TestService_RenderDriftSetsDrift(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	rendered := h.hostRow(approved.ID, svcCP)
	h.alterHash(approved.ID, svcCP)

	config, err := s.Render(t.Context(), h.ticket(approved, svcCP))

	if !errors.Is(err, ErrDrift) || config != nil {
		t.Fatalf("Render = %d bytes, %v; want no config and ErrDrift", len(config), err)
	}
	h.wantDrift(approved.ID, svcCP, rendered)
	if row := h.hostRow(approved.ID, "wrk-01"); row.Drift {
		t.Error("drift set on wrk-01, whose render did not change")
	}
	wantOneAudit(t, h.d, "cluster.drift", fleet.ActorSystem, `{"cluster":"`+labName+`","host":"`+svcCP+`"}`)
	if n := h.counter("hephaestus.alerts", "kind", "drift"); n != 1 {
		t.Errorf("hephaestus.alerts{kind=drift} = %d, want 1", n)
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) after drift, want pass: drift is not a render error", res.Status, res.Output)
	}
}

func TestService_DamagedSealedVersionIsRenderError(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	damageVersion(t, h.d, approved.SecretVersionID)

	_, err := s.Render(t.Context(), h.ticket(approved, svcCP))

	if err == nil || errors.Is(err, ErrDrift) || errors.Is(err, ErrNoRender) {
		t.Fatalf("Render = %v, want a render error, neither ErrDrift nor ErrNoRender", err)
	}
	if row := h.hostRow(approved.ID, svcCP); row.Drift {
		t.Error("a version that does not open set drift")
	}
	res := runCheck(t, s, "render:approved")
	if res.Status != admin.StatusFail || !strings.Contains(res.Output, labName+"/"+svcCP) {
		t.Errorf("render:approved = %s (%q), want fail naming %s/%s", res.Status, res.Output, labName, svcCP)
	}
}

func TestService_RenderWhileGitLocked(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)

	s.git.mu.Lock() // a fetch holds the store
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Render(ctx, h.ticket(approved, svcCP))
		done <- err
	}()
	select {
	case err := <-done:
		s.git.mu.Unlock()
		if err != nil {
			t.Errorf("Render while the git store is locked = %v, want a config", err)
		}
	case <-ctx.Done():
		s.git.mu.Unlock()
		<-done
		t.Error("Render waited for the git store's mutex")
	}
}

func TestService_StartupDetectsDrift(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	approved := h.approvedAfterPoll(h.service(h.fake))
	rendered := h.hostRow(approved.ID, svcCP)
	h.alterHash(approved.ID, svcCP)

	h.service(h.fake)

	h.wantDrift(approved.ID, svcCP, rendered)
	wantOneAudit(t, h.d, "cluster.drift", fleet.ActorSystem, `{"cluster":"`+labName+`","host":"`+svcCP+`"}`)
}

func TestService_RefreshDetectsDrift(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	rendered := h.hostRow(approved.ID, svcCP)
	h.alterHash(approved.ID, svcCP)

	h.requestRefresh()
	h.poll(s)

	if c := h.cluster(); !c.LastRefreshAt.Equal(h.now) {
		t.Fatalf("last refresh = %v, want %v; reported %q", c.LastRefreshAt, h.now, h.reported())
	}
	h.wantDrift(approved.ID, svcCP, rendered)
}

func TestService_UnreadableApprovedTreeFailsRender(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	approved := h.approvedAfterPoll(h.service(h.fake))
	if err := os.RemoveAll(filepath.Join(h.gitDir, h.id+".git")); err != nil {
		t.Fatalf("remove the cluster's repository: %v", err)
	}

	s := h.service(h.fake)

	if cached(s, approved.ID) {
		t.Fatal("the approved pair is cached without its repository")
	}
	res := runCheck(t, s, "render:approved")
	if res.Status != admin.StatusFail || !strings.Contains(res.Output, labName) {
		t.Errorf("render:approved = %s (%q), want fail naming %s", res.Status, res.Output, labName)
	}
}

func TestService_RenderConcurrentWithPoll(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	// Render and the checks read the clock from their own goroutines, so this test's clock is safe for that.
	var clock atomic.Int64
	clock.Store(gitT0.UnixNano())
	s, err := NewService(Config{
		Repo: h.repo, Resolver: h.fake, GitDir: h.gitDir, Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() },
		OnError: h.onError, Meter: h.meter,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.git.caBundle = srv.CAPEM()
	if err := s.Startup(t.Context()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	approved := h.approvedAfterPoll(s)
	var check admin.Check
	for _, c := range s.Checks() {
		if c.Name == "render:approved" {
			check = c
		}
	}

	// /config renders on its own goroutines while the poller stores and drops pairs in the tree cache.
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	var renders, failures atomic.Int64
	for range 2 {
		wg.Go(func() {
			for ctx.Err() == nil {
				if _, err := s.Render(ctx, h.ticket(approved, svcCP)); err != nil && ctx.Err() == nil {
					failures.Add(1)
				}
				renders.Add(1)
				check.Run(ctx)
			}
		})
	}
	for i := range 3 {
		srv.Commit(t, map[string]string{"lab/notes.txt": "head tree " + strconv.Itoa(i)}, nil)
		clock.Add(int64(FetchInterval))
		h.poll(s)
	}
	cancel()
	wg.Wait()

	if renders.Load() == 0 || failures.Load() != 0 {
		t.Errorf("%d renders, %d failed; want some, none failed; reported %q", renders.Load(), failures.Load(), h.reported())
	}
	if n := len(h.fake.seen()); n != 4 {
		t.Errorf("resolver called %d times, want 4: a pair per head tree", n)
	}
}

// breakVersions makes every read of a secret version fail as a dropped database would: neither ErrNotFound nor a row
// that does not open.
func breakVersions(t *testing.T, d *db.DB) {
	t.Helper()
	execRaw(t, d, "ALTER TABLE secret_versions RENAME TO secret_versions_gone")
}

func TestService_RepositoryErrorLeavesRenderApproved(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	breakVersions(t, h.d)

	_, err := s.Render(t.Context(), h.ticket(approved, svcCP))

	if err == nil || errors.Is(err, ErrNoRender) || errors.Is(err, ErrDrift) {
		t.Fatalf("Render = %v, want a repository error, neither ErrNoRender nor ErrDrift", err)
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) after a repository error, want pass", res.Status, res.Output)
	}
}

func TestService_StartupRepositoryErrorLeavesRenderApproved(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	h.approvedAfterPoll(h.service(h.fake))
	breakVersions(t, h.d)

	s := h.service(h.fake)

	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) after a repository error at startup, want pass", res.Status, res.Output)
	}
	for _, got := range h.reported() {
		if strings.Contains(got, "does not render") {
			t.Errorf("report %q calls a repository error a render failure", got)
		}
	}
}

func TestService_StartupDamagedVersionFailsRenderApproved(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	approved := h.approvedAfterPoll(h.service(h.fake))
	damageVersion(t, h.d, approved.SecretVersionID)

	s := h.service(h.fake)

	res := runCheck(t, s, "render:approved")
	for _, host := range []string{svcCP, "wrk-01"} {
		if res.Status != admin.StatusFail || !strings.Contains(res.Output, labName+"/"+host) {
			t.Errorf("render:approved = %s (%q), want fail naming %s/%s", res.Status, res.Output, labName, host)
		}
	}
	if row := h.hostRow(approved.ID, svcCP); row.Drift {
		t.Error("a version that does not open set drift")
	}
}

func TestService_RenderErrorFailsRenderApproved(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	approved := h.approvedAfterPoll(s)
	// A stored value set that opens but that the bundle cannot hold: the pair resolves, but does not render.
	values := h.values(approved.SecretVersionID)
	values["ref+op://lab/bundle/certs.os.crt"] = "not a certificate"
	plain, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("encode values: %v", err)
	}
	sealed := h.repo.key.SealField(datakey.Field{Table: tableVersions, Row: approved.SecretVersionID, Column: columnValues}, plain)
	execRaw(t, h.d, "UPDATE secret_versions SET values_sealed = ? WHERE id = ?", sealed, approved.SecretVersionID)

	_, err = s.Render(t.Context(), h.ticket(approved, svcCP))

	if err == nil || errors.Is(err, ErrDrift) || errors.Is(err, ErrNoRender) || errors.Is(err, datakey.ErrDecrypt) {
		t.Fatalf("Render = %v, want a render error", err)
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusFail || !strings.Contains(res.Output, labName+"/"+svcCP) {
		t.Errorf("render:approved = %s (%q), want fail naming %s/%s", res.Status, res.Output, labName, svcCP)
	}
}
