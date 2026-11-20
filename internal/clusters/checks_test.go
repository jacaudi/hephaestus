package clusters

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/secrets"
)

// runCheck runs the service's check with the name once.
func runCheck(t *testing.T, s *Service, name string) admin.Result {
	t.Helper()
	for _, c := range s.Checks() {
		if c.Name == name {
			return c.Run(t.Context())
		}
	}
	t.Fatalf("Checks() has no %q", name)
	return admin.Result{}
}

// nullTransport answers every request with an empty 200.
type nullTransport struct{}

func (nullTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// counterAt is a counter that has counted n requests to 1Password.
func counterAt(t *testing.T, n int) *secrets.Counter {
	t.Helper()
	c := secrets.NewCounter(nullTransport{}, func() time.Time { return gitT0 })
	for range n {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://my.1password.com/api", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := c.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	return c
}

func TestChecks_Names(t *testing.T) {
	repo, _ := testRepo(t, dbtest.SQLite)
	s, err := NewService(Config{Repo: repo, GitDir: t.TempDir(), Now: func() time.Time { return gitT0 }, OnError: func(context.Context, error) {}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got := map[string]bool{}
	for _, c := range s.Checks() {
		got[c.Name] = c.Critical
	}
	want := map[string]bool{
		"secrets:cache": false, "secrets:token": false, "secrets:budget": false, "secrets:client": false,
		"git:fetch": false, "render:approved": true,
	}
	if len(got) != len(want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
	for name, critical := range want {
		if c, ok := got[name]; !ok || c != critical {
			t.Errorf("check %s: present = %v, critical = %v; want present, critical = %v", name, ok, c, critical)
		}
	}
}

func TestChecks_ClientWarnAndClear(t *testing.T) {
	repo, _ := testRepo(t, dbtest.SQLite)
	fake := newFakeResolver(map[string]string{svcCredRef: svcPassword})
	s, err := NewService(Config{Repo: repo, Resolver: fake, GitDir: t.TempDir(), Now: func() time.Time { return gitT0 }, OnError: func(context.Context, error) {}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	refs := []string{svcCredRef}
	call := func(whole error) {
		t.Helper()
		fake.set(func(f *fakeResolver) { f.whole = whole })
		if _, err := s.resolve(t.Context(), refs); (err != nil) != (whole != nil) {
			t.Fatalf("resolve = %v, want the failure %v", err, whole)
		}
	}
	panicked := &secrets.WholeCallError{Err: errors.New("core"), Panicked: true}

	for range ClientWarnFailures - 1 {
		call(panicked)
	}
	if res := runCheck(t, s, "secrets:client"); res.Status != admin.StatusPass {
		t.Errorf("secrets:client after %d failures = %s (%q), want pass", ClientWarnFailures-1, res.Status, res.Output)
	}
	call(panicked)
	if res := runCheck(t, s, "secrets:client"); res.Status != admin.StatusWarn || !strings.Contains(res.Output, "restart") {
		t.Errorf("secrets:client after %d failures, the last a panic = %s (%q); want warn naming a restart", ClientWarnFailures, res.Status, res.Output)
	}
	call(&secrets.WholeCallError{Err: errors.New("transport")})
	if res := runCheck(t, s, "secrets:client"); res.Status != admin.StatusWarn || strings.Contains(res.Output, "restart") {
		t.Errorf("secrets:client after a failure that did not panic = %s (%q); want warn without a restart", res.Status, res.Output)
	}
	call(nil)
	if res := runCheck(t, s, "secrets:client"); res.Status != admin.StatusPass {
		t.Errorf("secrets:client after a success = %s (%q), want pass", res.Status, res.Output)
	}
}

func TestChecks_RenderApprovedKeyedOnPair(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	h := newSvcHarness(t, srv, "")
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	srv.Commit(t, svcFiles(t), nil)
	s := h.service(h.fake)
	failing := h.approvedAfterPoll(s)
	damageVersion(t, h.d, failing.SecretVersionID)
	if _, err := s.Render(t.Context(), h.ticket(failing, svcCP)); err == nil {
		t.Fatal("Render of a damaged version succeeded")
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusFail {
		t.Fatalf("render:approved = %s, want fail", res.Status)
	}

	srv.Commit(t, map[string]string{"lab/notes.txt": "a second head tree"}, nil)
	h.now = h.now.Add(FetchInterval)
	next := h.pendingAfterPoll(s)
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusFail {
		t.Errorf("render:approved = %s with the failing pair still approved, want fail", res.Status)
	}
	approveForTest(t, h.d, h.id, next.ID)
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) once another pair is approved, want pass", res.Status, res.Output)
	}
	if _, err := s.Render(t.Context(), h.ticket(next, svcCP)); err != nil {
		t.Fatalf("Render of the new approved pair: %v", err)
	}
	if res := runCheck(t, s, "render:approved"); res.Status != admin.StatusPass {
		t.Errorf("render:approved = %s (%q) after the new pair rendered, want pass", res.Status, res.Output)
	}
}

func TestChecks_TokenBudgetCacheFetch(t *testing.T) {
	fetchText := "dial " + labRepo + ": connection refused"
	tests := []struct {
		name     string
		check    string
		resolver bool
		requests int
		// setup changes cluster lab, imported at gitT0; the checks run at gitT0 plus a day unless at is set.
		setup      func(t *testing.T, r *Repository, d *db.DB, id string)
		at         time.Duration
		want       admin.Status
		wantOutput string
	}{
		{name: "token set", check: "secrets:token", resolver: true, want: admin.StatusPass},
		{name: "no token with a cluster", check: "secrets:token", want: admin.StatusWarn, wantOutput: "HEPHAESTUS_OP_TOKEN"},
		{
			name: "no token and no cluster", check: "secrets:token", want: admin.StatusPass,
			setup: func(t *testing.T, r *Repository, _ *db.DB, _ string) {
				t.Helper()
				if err := r.Remove(t.Context(), labName, fleet.ActorCLI, gitT0); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
		},
		{name: "budget below half", check: "secrets:budget", requests: 499, want: admin.StatusPass},
		{name: "budget at half", check: "secrets:budget", requests: 500, want: admin.StatusWarn, wantOutput: "500"},
		{
			name: "refresh incomplete", check: "secrets:cache", want: admin.StatusWarn, wantOutput: labName,
			setup: func(t *testing.T, _ *Repository, d *db.DB, id string) {
				t.Helper()
				execRaw(t, d, "UPDATE clusters SET refresh_incomplete = ? WHERE id = ?", true, id)
			},
		},
		{
			name: "approved and refreshed within two days", check: "secrets:cache", want: admin.StatusPass, at: 3 * 24 * time.Hour,
			setup: func(t *testing.T, _ *Repository, d *db.DB, id string) {
				t.Helper()
				execRaw(t, d, "UPDATE clusters SET approved_pair_id = ?, last_refresh_at = ? WHERE id = ?", "P1", stamp(gitT0.Add(25*time.Hour)), id)
			},
		},
		{
			name: "approved and no refresh for two days", check: "secrets:cache", want: admin.StatusWarn, wantOutput: labName, at: 2 * 24 * time.Hour,
			setup: func(t *testing.T, _ *Repository, d *db.DB, id string) {
				t.Helper()
				execRaw(t, d, "UPDATE clusters SET approved_pair_id = ? WHERE id = ?", "P1", id)
			},
		},
		{
			name: "fetched recently", check: "git:fetch", want: admin.StatusPass, at: time.Hour,
			setup: func(t *testing.T, r *Repository, _ *db.DB, id string) {
				t.Helper()
				if err := r.RecordFetch(t.Context(), id, nil, gitT0.Add(31*time.Minute)); err != nil {
					t.Fatalf("RecordFetch: %v", err)
				}
			},
		},
		{
			name: "fetch older than 30 minutes", check: "git:fetch", want: admin.StatusWarn, wantOutput: labName, at: time.Hour,
			setup: func(t *testing.T, r *Repository, _ *db.DB, id string) {
				t.Helper()
				if err := r.RecordFetch(t.Context(), id, nil, gitT0.Add(30*time.Minute)); err != nil {
					t.Fatalf("RecordFetch: %v", err)
				}
			},
		},
		{
			name: "last fetch failed", check: "git:fetch", want: admin.StatusWarn, wantOutput: labName, at: time.Minute,
			setup: func(t *testing.T, r *Repository, _ *db.DB, id string) {
				t.Helper()
				if err := r.RecordFetch(t.Context(), id, errors.New(fetchText), gitT0); err != nil {
					t.Fatalf("RecordFetch: %v", err)
				}
			},
		},
		{
			name: "failed tree", check: "git:fetch", want: admin.StatusWarn, wantOutput: labName, at: time.Minute,
			setup: func(t *testing.T, r *Repository, d *db.DB, id string) {
				t.Helper()
				if err := r.RecordFetch(t.Context(), id, nil, gitT0); err != nil {
					t.Fatalf("RecordFetch: %v", err)
				}
				execRaw(t, d, "UPDATE clusters SET failed_tree = ? WHERE id = ?", "503a824e9a24e59db3b6aa890aa8511ae8b684a4", id)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, d := testRepo(t, dbtest.SQLite)
			c := mustImport(t, repo, labImport(gitT0))
			if tt.setup != nil {
				tt.setup(t, repo, d, c.ID)
			}
			at := gitT0.Add(cmp.Or(tt.at, 24*time.Hour))
			cfg := Config{
				Repo: repo, Counter: counterAt(t, tt.requests), GitDir: filepath.Join(t.TempDir(), "git"),
				Now: func() time.Time { return at }, OnError: func(context.Context, error) {},
			}
			if tt.resolver {
				cfg.Resolver = newFakeResolver(nil)
			}
			s, err := NewService(cfg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			res := runCheck(t, s, tt.check)

			if res.Status != tt.want || !strings.Contains(res.Output, tt.wantOutput) {
				t.Errorf("%s = %s (%q), want %s with %q", tt.check, res.Status, res.Output, tt.want, tt.wantOutput)
			}
			if strings.Contains(res.Output, labRepo) {
				t.Errorf("%s output %q quotes the repository URL", tt.check, res.Output)
			}
		})
	}
}
