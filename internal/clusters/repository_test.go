package clusters

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

const (
	labName        = "lab"
	labRepo        = "https://git.example.test/lab/cluster.git"
	labRef         = "main"
	labPath        = "clusters/lab"
	labUser        = "deploy"
	labCredRef     = "ref+op://vault/git/token"
	talosVersion   = "v1.12.11"
	plainSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	// dataKey is the fixture data key: base64 of 32 bytes of 0x01.
	dataKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	// leaked is a text that must never appear in a stored column in clear.
	leaked = "s3cr3t-value-7f3a"
)

func TestRepository_SQLite(t *testing.T) { runRepositorySuite(t, dbtest.SQLite) }

// testRepo is a repository over a fresh database and the fixture key.
func testRepo(t *testing.T, open func(testing.TB) *db.DB) (*Repository, *db.DB) {
	t.Helper()
	key, err := datakey.Parse(secret.Secret(dataKey))
	if err != nil {
		t.Fatalf("parse the fixture data key: %v", err)
	}
	d := open(t)
	return NewRepository(d, key), d
}

func labImport(at time.Time) ImportRequest {
	return ImportRequest{
		Name: labName, RepoURL: labRepo, Ref: labRef, Subpath: labPath, Username: labUser, CredentialRef: labCredRef, At: at,
	}
}

func mustImport(t *testing.T, r *Repository, req ImportRequest) Cluster {
	t.Helper()
	c, err := r.Import(t.Context(), req)
	if err != nil {
		t.Fatalf("Import(%s): %v", req.Name, err)
	}
	return c
}

func mustGet(t *testing.T, r *Repository, name string) Cluster {
	t.Helper()
	c, err := r.Get(t.Context(), name)
	if err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	return c
}

// mustSet applies req and returns the stored cluster.
func mustSet(t *testing.T, r *Repository, req SetRequest) Cluster {
	t.Helper()
	c, err := r.Set(t.Context(), req)
	if err != nil {
		t.Fatalf("Set(%+v): %v", req, err)
	}
	return c
}

// markReq marks tree processing on c at the revision c carries.
func markReq(c Cluster, tree string, trigger Trigger, at time.Time) MarkRequest {
	return MarkRequest{ClusterID: c.ID, Tree: tree, Revision: c.Revision, Trigger: trigger, At: at}
}

func mustMark(t *testing.T, r *Repository, c Cluster, tree string, trigger Trigger, at time.Time) {
	t.Helper()
	ok, err := r.MarkProcessing(t.Context(), markReq(c, tree, trigger, at))
	if err != nil || !ok {
		t.Fatalf("MarkProcessing(%s, %s) = %v, %v; want true, nil", tree, trigger, ok, err)
	}
}

// labRenders are the renders of the two hosts of cluster lab. Only cp-01 has a schematic body.
func labRenders() []render.HostRender {
	return []render.HostRender{
		{
			Host: "cp-01", Role: "control-plane", IP: "10.10.20.11",
			Config: []byte("machine: " + leaked), Redacted: []byte("machine:\n  token: ****\n  host: cp-01\n"),
			ConfigHash: "c1", DisplayHash: "d1", SchematicID: plainSchematic,
			SchematicBody: []byte("customization: {}\n"), TalosVersion: talosVersion,
		},
		{
			Host: "wrk-01", Role: "worker", IP: "10.10.20.21",
			Config: []byte("machine: " + leaked), Redacted: []byte("machine:\n  token: ****\n  host: wrk-01\n"),
			ConfigHash: "c2", DisplayHash: "d2", SchematicID: plainSchematic, TalosVersion: talosVersion,
		},
	}
}

// pendingReq stores the lab renders as the pending pair of tree on c.
func pendingReq(c Cluster, tree string, at time.Time) StorePendingRequest {
	return StorePendingRequest{
		ClusterID: c.ID, Tree: tree, Revision: c.Revision,
		Values:       map[string]string{"ref+op://vault/item/password": leaked},
		TalosVersion: talosVersion, Renders: labRenders(), At: at,
	}
}

// mustStore marks tree for a new head and stores its pending pair, returning the pair id.
func mustStore(t *testing.T, r *Repository, c Cluster, tree string, at time.Time) string {
	t.Helper()
	mustMark(t, r, c, tree, TriggerHead, at)
	id, stored, err := r.StorePending(t.Context(), pendingReq(c, tree, at))
	if err != nil || !stored || id == "" {
		t.Fatalf("StorePending(%s) = %q, %v, %v; want a pair id, true, nil", tree, id, stored, err)
	}
	return id
}

// mustFail marks tree for a new head and stores it as the failed tree.
func mustFail(t *testing.T, r *Repository, c Cluster, tree, text string, at time.Time) {
	t.Helper()
	mustMark(t, r, c, tree, TriggerHead, at)
	stored, err := r.StoreFailed(t.Context(), StoreFailedRequest{ClusterID: c.ID, Tree: tree, Revision: c.Revision, Error: text, At: at})
	if err != nil || !stored {
		t.Fatalf("StoreFailed(%s) = %v, %v; want true, nil", tree, stored, err)
	}
}

// scalar runs a query that returns one value.
func scalar[T any](t *testing.T, d *db.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := d.Bun().NewRaw(query, args...).Scan(t.Context(), &v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

// execRaw runs a statement for its side effect.
func execRaw(t *testing.T, d *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Bun().ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// count is the number of rows of table that match where.
func count(t *testing.T, d *db.DB, table, where string, args ...any) int {
	t.Helper()
	return scalar[int](t, d, "SELECT count(*) FROM "+table+" WHERE "+where, args...)
}

// auditRow is the part of an audit_events row the tests compare.
type auditRow struct {
	Action, Actor, Detail string
}

func auditsOf(t *testing.T, d *db.DB, action string) []auditRow {
	t.Helper()
	var out []auditRow
	err := d.Bun().NewSelect().TableExpr("audit_events").ColumnExpr("action, actor, detail").Where("action = ?", action).OrderExpr("id").Scan(t.Context(), &out)
	if err != nil {
		t.Fatalf("read audit events %s: %v", action, err)
	}
	return out
}

// wantOneAudit fails unless exactly one audit event has action, and it has the given actor and detail.
func wantOneAudit(t *testing.T, d *db.DB, action, actor, detail string) {
	t.Helper()
	got := auditsOf(t, d, action)
	if len(got) != 1 || got[0].Actor != actor || got[0].Detail != detail {
		t.Fatalf("%s audits = %+v; want one by %q with detail %s", action, got, actor, detail)
	}
}

// wantNoMark fails unless c carries no processing mark.
func wantNoMark(t *testing.T, c Cluster) {
	t.Helper()
	if c.ProcessingTree != "" || c.ProcessingRevision != nil {
		t.Fatalf("processing mark = %q at %v; want none", c.ProcessingTree, c.ProcessingRevision)
	}
}

// wantNotContain fails when blob holds text.
func wantNotContain(t *testing.T, what string, blob []byte, text string) {
	t.Helper()
	if len(blob) == 0 || bytes.Contains(blob, []byte(text)) {
		t.Fatalf("%s = %q; want sealed bytes without %q", what, blob, text)
	}
}

// flipLast changes the last byte of b, which damages a sealed value.
func flipLast(b []byte) []byte {
	out := bytes.Clone(b)
	out[len(out)-1] ^= 0xff
	return out
}

// runRepositorySuite is the clusters suite both databases run: TestRepository_SQLite untagged,
// TestRepository_Postgres under the integration tag.
func runRepositorySuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("NewRepository", func(t *testing.T) {
		r, _ := testRepo(t, open)
		if _, err := r.Get(t.Context(), labName); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get on an empty table error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ImportValid", func(t *testing.T) {
		r, d := testRepo(t, open)
		got := mustImport(t, r, labImport(t0))
		if got.ID == "" || got.Name != labName || got.RepoURL != labRepo || got.Ref != labRef || got.Subpath != labPath ||
			got.Username != labUser || got.CredentialRef != labCredRef || got.Revision != 0 {
			t.Fatalf("Import = %+v, want the lab definition at revision 0 with an id", got)
		}
		if !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0) {
			t.Fatalf("times = %v, %v; want t0", got.CreatedAt, got.UpdatedAt)
		}
		wantNoMark(t, got)
		if stored := mustGet(t, r, labName); stored.ID != got.ID || stored.CredentialRef != labCredRef {
			t.Fatalf("Get = %+v, want the imported cluster %s", stored, got.ID)
		}
		wantOneAudit(t, d, "cluster.imported", fleet.ActorCLI, `{"name":"lab"}`)
	})

	t.Run("ImportAcceptsNoCredential", func(t *testing.T) {
		r, _ := testRepo(t, open)
		req := labImport(t0)
		req.Username, req.CredentialRef = "", ""
		if got := mustImport(t, r, req); got.CredentialRef != "" || got.Username != "" {
			t.Fatalf("Import = %+v, want no username and no credential", got)
		}
	})

	t.Run("ImportRefusesDuplicate", func(t *testing.T) {
		r, d := testRepo(t, open)
		mustImport(t, r, labImport(t0))
		_, err := r.Import(t.Context(), labImport(t0.Add(time.Minute)))
		if !errors.Is(err, ErrExists) {
			t.Fatalf("second Import error = %v, want ErrExists", err)
		}
		if n := len(auditsOf(t, d, "cluster.imported")); n != 1 {
			t.Fatalf("import audits = %d, want 1", n)
		}
	})

	t.Run("ImportRefusesHTTP", func(t *testing.T) {
		r, d := testRepo(t, open)
		req := labImport(t0)
		req.RepoURL = "http://git.example.test/lab/cluster.git"
		if _, err := r.Import(t.Context(), req); err == nil {
			t.Fatal("Import of an http URL succeeded")
		}
		if got, err := r.List(t.Context()); err != nil || len(got) != 0 {
			t.Fatalf("List = %v, %v; want no clusters", got, err)
		}
		if n := len(auditsOf(t, d, "cluster.imported")); n != 0 {
			t.Fatalf("import audits = %d, want 0", n)
		}
	})

	t.Run("ImportRefusesUserinfo", func(t *testing.T) {
		r, _ := testRepo(t, open)
		req := labImport(t0)
		req.RepoURL = "https://deploy:" + leaked + "@git.example.test/lab/cluster.git"
		_, err := r.Import(t.Context(), req)
		if err == nil {
			t.Fatal("Import of a URL with userinfo succeeded")
		}
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("the error quotes the URL password: %v", err)
		}
	})

	t.Run("ImportRefusesBadName", func(t *testing.T) {
		for _, name := range []string{"Lab", "a/b", "", "-lab", "lab-", "la_b", strings.Repeat("a", 64)} {
			t.Run(name, func(t *testing.T) {
				r, _ := testRepo(t, open)
				req := labImport(t0)
				req.Name = name
				if _, err := r.Import(t.Context(), req); err == nil {
					t.Fatalf("Import of name %q succeeded", name)
				}
			})
		}
	})

	t.Run("ImportRefusesBadCredential", func(t *testing.T) {
		for _, ref := range []string{"op://v/i/f", "ref+op://v/i", "ref+op://v/i/f?attribute=otp", "ref+vault://a/b/c"} {
			t.Run(ref, func(t *testing.T) {
				r, _ := testRepo(t, open)
				req := labImport(t0)
				req.CredentialRef = ref
				if _, err := r.Import(t.Context(), req); err == nil {
					t.Fatalf("Import of credential %q succeeded", ref)
				}
			})
		}
	})

	t.Run("SetChangedSourceClears", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		pairID := mustStore(t, r, c, "T1", t0.Add(time.Minute))
		pair, err := r.Pair(t.Context(), pairID)
		if err != nil {
			t.Fatalf("Pair: %v", err)
		}
		mustFail(t, r, c, "T2", "boom", t0.Add(2*time.Minute))
		mustMark(t, r, c, "T3", TriggerHead, t0.Add(3*time.Minute))
		execRaw(t, d, "UPDATE clusters SET approved_pair_id = ? WHERE id = ?", "approved-pair", c.ID)

		got := mustSet(t, r, SetRequest{Name: labName, RepoURL: new("https://git.example.test/other/cluster.git"), At: t0.Add(4 * time.Minute)})
		if got.Revision != 1 || got.PendingPairID != "" || got.FailedTree != "" || got.LastHeadTree != "" || got.ApprovedPairID != "approved-pair" {
			t.Fatalf("Set = %+v, want revision 1, the pending pair, failed tree and last head cleared, the approved pair kept", got)
		}
		wantNoMark(t, got)
		if !got.UpdatedAt.Equal(t0.Add(4 * time.Minute)) {
			t.Fatalf("UpdatedAt = %v, want the time of the Set", got.UpdatedAt)
		}
		if got.FailedErrorSealed != nil {
			t.Fatalf("FailedErrorSealed = %q, want NULL with the failed tree", got.FailedErrorSealed)
		}
		if n := count(t, d, "cluster_pairs", "id = ?", pairID) + count(t, d, "host_renders", "pair_id = ?", pairID) +
			count(t, d, "secret_versions", "id = ?", pair.SecretVersionID); n != 0 {
			t.Fatalf("%d rows of the replaced pending pair remain, want none", n)
		}
		wantOneAudit(t, d, "cluster.set", fleet.ActorCLI, `{"name":"lab","changed":["repo_url"]}`)
	})

	t.Run("SetSameSourceKeepsPending", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		pairID := mustStore(t, r, c, "T1", t0)
		got := mustSet(t, r, SetRequest{Name: labName, RepoURL: new(labRepo), Ref: new(labRef), Subpath: new(labPath), Username: new("other"), At: t0.Add(time.Minute)})
		if got.Revision != 1 || got.PendingPairID != pairID || got.LastHeadTree != "T1" || got.Username != "other" {
			t.Fatalf("Set = %+v, want revision 1, pending pair %s and last head T1 kept, username changed", got, pairID)
		}
	})

	t.Run("SetCredentialClearsSealed", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if err := r.SetCredential(t.Context(), c.ID, secret.Secret(leaked), t0); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		// A changed ref is a changed source; the read credential stays.
		mustSet(t, r, SetRequest{Name: labName, Ref: new("release"), At: t0.Add(time.Minute)})
		if _, ok, err := r.Credential(t.Context(), c.ID); err != nil || !ok {
			t.Fatalf("Credential after a changed ref = %v, %v; want it kept", ok, err)
		}
		got := mustSet(t, r, SetRequest{Name: labName, CredentialRef: new("ref+op://vault/git/other"), At: t0.Add(2 * time.Minute)})
		if got.CredentialRef != "ref+op://vault/git/other" || got.CredentialSealed != nil || got.Revision != 2 {
			t.Fatalf("Set = %+v, want the new reference, no sealed credential, revision 2", got)
		}
		if _, ok, err := r.Credential(t.Context(), c.ID); err != nil || ok {
			t.Fatalf("Credential after a changed reference = %v, %v; want absent", ok, err)
		}
	})

	t.Run("SetRefusesBadInput", func(t *testing.T) {
		r, _ := testRepo(t, open)
		mustImport(t, r, labImport(t0))
		for _, req := range []SetRequest{
			{Name: labName, RepoURL: new("http://git.example.test/r.git")},
			{Name: labName, RepoURL: new("https://u:p@git.example.test/r.git")},
			{Name: labName, CredentialRef: new("op://v/i/f")},
		} {
			if _, err := r.Set(t.Context(), req); err == nil {
				t.Errorf("Set(%+v) succeeded", req)
			}
		}
		if got := mustGet(t, r, labName); got.Revision != 0 || got.RepoURL != labRepo {
			t.Fatalf("cluster = %+v after refused sets, want it unchanged", got)
		}
	})

	t.Run("SetUnknown", func(t *testing.T) {
		r, _ := testRepo(t, open)
		if _, err := r.Set(t.Context(), SetRequest{Name: labName, At: t0}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Set of an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("RemoveInUse", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		execRaw(t, d, "INSERT INTO machines (id, smbios_uuid, serial, product, arch, state, cluster_id, host_name, created_at, last_seen_at) "+
			"VALUES (?, ?, 'S', 'P', 'amd64', 'assigned', ?, 'cp-01', ?, ?)", "m1", "4c4c4544-0042-3510-8052-b4c04f4e4d01", c.ID, t0, t0)
		if err := r.Remove(t.Context(), labName, "admin", t0); !errors.Is(err, ErrClusterInUse) {
			t.Fatalf("Remove error = %v, want ErrClusterInUse", err)
		}
		mustGet(t, r, labName)
		if n := len(auditsOf(t, d, "cluster.removed")); n != 0 {
			t.Fatalf("removed audits = %d, want 0", n)
		}
	})

	t.Run("RemoveFree", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustStore(t, r, c, "T1", t0)
		if err := r.Remove(t.Context(), labName, "admin", t0.Add(time.Minute)); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := r.Get(t.Context(), labName); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after Remove error = %v, want ErrNotFound", err)
		}
		if n := count(t, d, "cluster_pairs", "1 = 1") + count(t, d, "secret_versions", "1 = 1") + count(t, d, "host_renders", "1 = 1"); n != 0 {
			t.Fatalf("%d pair, version and render rows remain, want none (cascade)", n)
		}
		wantOneAudit(t, d, "cluster.removed", "admin", `{"name":"lab"}`)
		if err := r.Remove(t.Context(), labName, "admin", t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Remove error = %v, want ErrNotFound", err)
		}
	})

	t.Run("List", func(t *testing.T) {
		r, _ := testRepo(t, open)
		for _, name := range []string{"zeta", "alpha", "mid"} {
			req := labImport(t0)
			req.Name = name
			mustImport(t, r, req)
		}
		got, err := r.List(t.Context())
		if err != nil || len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "mid" || got[2].Name != "zeta" {
			t.Fatalf("List = %+v, %v; want alpha, mid, zeta", got, err)
		}
	})

	t.Run("GetKnown", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if got := mustGet(t, r, labName); got.ID != c.ID || got.Name != labName || !got.CreatedAt.Equal(t0) {
			t.Fatalf("Get = %+v, want the imported cluster %s", got, c.ID)
		}
	})

	t.Run("GetUnknown", func(t *testing.T) {
		r, _ := testRepo(t, open)
		mustImport(t, r, labImport(t0))
		if _, err := r.Get(t.Context(), "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get error = %v, want ErrNotFound", err)
		}
	})

	t.Run("RequestRefresh", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		execRaw(t, d, "UPDATE clusters SET attempts = 3, next_attempt_at = ?, rate_limited_until = ? WHERE id = ?", t0.Add(time.Hour), t0.Add(24*time.Hour), c.ID)
		if err := r.RequestRefresh(t.Context(), labName, t0.Add(time.Minute)); err != nil {
			t.Fatalf("RequestRefresh: %v", err)
		}
		got := mustGet(t, r, labName)
		if !got.RefreshRequested || got.Attempts != 0 || !got.NextAttemptAt.IsZero() || !got.RateLimitedUntil.IsZero() {
			t.Fatalf("cluster = %+v, want the flag set and attempts, next attempt and rate limit cleared", got)
		}
		if err := r.RequestRefresh(t.Context(), "nope", t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RequestRefresh of an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("MarkConsumesRefresh", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if err := r.RequestRefresh(t.Context(), labName, t0); err != nil {
			t.Fatalf("RequestRefresh: %v", err)
		}
		mustMark(t, r, c, "T1", TriggerRefresh, t0.Add(time.Minute))
		got := mustGet(t, r, labName)
		if got.RefreshRequested || got.ProcessingTree != "T1" || got.ProcessingRevision == nil || *got.ProcessingRevision != 0 || !got.LastDailyAt.IsZero() {
			t.Fatalf("cluster = %+v, want the flag consumed and T1 processing at revision 0", got)
		}
	})

	t.Run("MarkConsumesDaily", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if err := r.RequestRefresh(t.Context(), labName, t0); err != nil {
			t.Fatalf("RequestRefresh: %v", err)
		}
		at := t0.Add(time.Minute)
		mustMark(t, r, c, "T1", TriggerDaily, at)
		got := mustGet(t, r, labName)
		if !got.LastDailyAt.Equal(at) || !got.RefreshRequested {
			t.Fatalf("cluster = %+v, want last_daily_at at the mark and the refresh flag kept", got)
		}
	})

	t.Run("MarkConsumesNothingForHeadAndContinue", func(t *testing.T) {
		for _, trigger := range []Trigger{TriggerHead, TriggerContinue, TriggerCredential} {
			t.Run(string(trigger), func(t *testing.T) {
				r, _ := testRepo(t, open)
				c := mustImport(t, r, labImport(t0))
				if err := r.RequestRefresh(t.Context(), labName, t0); err != nil {
					t.Fatalf("RequestRefresh: %v", err)
				}
				mustMark(t, r, c, "T1", trigger, t0.Add(time.Minute))
				if got := mustGet(t, r, labName); !got.RefreshRequested || !got.LastDailyAt.IsZero() || got.ProcessingTree != "T1" {
					t.Fatalf("cluster = %+v, want the flag and last_daily_at untouched and T1 processing", got)
				}
			})
		}
	})

	t.Run("MarkRefusesUnknownTrigger", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		ok, err := r.MarkProcessing(t.Context(), markReq(c, "T1", Trigger("bogus"), t0))
		if err == nil || ok {
			t.Fatalf("MarkProcessing = %v, %v; want an error", ok, err)
		}
		wantNoMark(t, mustGet(t, r, labName))
	})

	t.Run("MarkRefusedOnStaleRevision", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustSet(t, r, SetRequest{Name: labName, Username: new("other"), At: t0})
		ok, err := r.MarkProcessing(t.Context(), markReq(c, "T1", TriggerHead, t0))
		if err != nil || ok {
			t.Fatalf("MarkProcessing at the old revision = %v, %v; want false, nil", ok, err)
		}
		wantNoMark(t, mustGet(t, r, labName))
	})

	t.Run("MarkRefusedWhileMarked", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustMark(t, r, c, "T1", TriggerHead, t0)
		if err := r.RequestRefresh(t.Context(), labName, t0); err != nil {
			t.Fatalf("RequestRefresh: %v", err)
		}
		ok, err := r.MarkProcessing(t.Context(), markReq(c, "T2", TriggerRefresh, t0))
		if err != nil || ok {
			t.Fatalf("second MarkProcessing = %v, %v; want false, nil", ok, err)
		}
		if got := mustGet(t, r, labName); got.ProcessingTree != "T1" || !got.RefreshRequested {
			t.Fatalf("cluster = %+v, want T1 still processing and the refused trigger unconsumed", got)
		}
	})

	t.Run("MarkAtRevisionZero", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		ok, err := r.MarkProcessing(t.Context(), markReq(c, "T1", TriggerHead, t0))
		if err != nil || !ok {
			t.Fatalf("MarkProcessing at revision 0 = %v, %v; want true, nil", ok, err)
		}
		if got := mustGet(t, r, labName); got.ProcessingTree != "T1" || got.ProcessingRevision == nil || *got.ProcessingRevision != 0 {
			t.Fatalf("cluster = %+v, want T1 processing with revision 0 (not NULL)", got)
		}
	})

	t.Run("ClearProcessingKeepsLastHead", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustStore(t, r, c, "T1", t0)
		mustMark(t, r, c, "T2", TriggerHead, t0.Add(time.Minute))
		if err := r.ClearProcessing(t.Context(), c.ID); err != nil {
			t.Fatalf("ClearProcessing: %v", err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if got.LastHeadTree != "T1" {
			t.Fatalf("LastHeadTree = %q, want T1 unchanged", got.LastHeadTree)
		}
	})

	t.Run("ConvertInterrupted", func(t *testing.T) {
		r, _ := testRepo(t, open)
		a := mustImport(t, r, labImport(t0))
		other := labImport(t0)
		other.Name = "idle"
		idle := mustImport(t, r, other)
		mustMark(t, r, a, "T1", TriggerHead, t0)

		n, err := r.ConvertInterrupted(t.Context(), t0.Add(time.Minute))
		if err != nil || n != 1 {
			t.Fatalf("ConvertInterrupted = %d, %v; want 1, nil", n, err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		errs, err := r.Errors(t.Context(), a.ID)
		if err != nil || got.FailedTree != "T1" || got.LastHeadTree != "T1" || errs.Failed != "interrupted" {
			t.Fatalf("cluster = %+v, errors = %+v, %v; want T1 failed as interrupted and recorded as the last head", got, errs, err)
		}
		if idleGot := mustGet(t, r, "idle"); idleGot.FailedTree != "" || idleGot.LastHeadTree != "" || idleGot.ID != idle.ID {
			t.Fatalf("idle cluster = %+v, want it untouched", idleGot)
		}
		if n, err := r.ConvertInterrupted(t.Context(), t0.Add(2*time.Minute)); err != nil || n != 0 {
			t.Fatalf("second ConvertInterrupted = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("StorePendingReplacesOlder", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustFail(t, r, c, "T0", "boom", t0)
		first := mustStore(t, r, c, "T1", t0.Add(time.Minute))
		old, err := r.Pair(t.Context(), first)
		if err != nil {
			t.Fatalf("Pair(%s): %v", first, err)
		}
		if got := mustGet(t, r, labName); got.FailedTree != "" || got.FailedErrorSealed != nil || got.LastHeadTree != "T1" || got.PendingPairID != first {
			t.Fatalf("cluster after the first pair = %+v, want the failed tree cleared, T1 recorded and %s pending", got, first)
		}

		second := mustStore(t, r, c, "T2", t0.Add(2*time.Minute))
		if second == first {
			t.Fatalf("second pair id = first id %s", first)
		}
		if _, err := r.Pair(t.Context(), first); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Pair(first) error = %v, want ErrNotFound", err)
		}
		if n := count(t, d, "host_renders", "pair_id = ?", first) + count(t, d, "secret_versions", "id = ?", old.SecretVersionID); n != 0 {
			t.Fatalf("%d renders and versions of the replaced pair remain, want none", n)
		}
		if n := count(t, d, "host_renders", "pair_id = ?", second); n != 2 {
			t.Fatalf("renders of the new pair = %d, want 2", n)
		}
		if pairs, versions := count(t, d, "cluster_pairs", "1 = 1"), count(t, d, "secret_versions", "1 = 1"); pairs != 1 || versions != 1 {
			t.Fatalf("pairs = %d, versions = %d; want 1 each", pairs, versions)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if got.PendingPairID != second || got.LastHeadTree != "T2" {
			t.Fatalf("cluster = %+v, want %s pending and T2 recorded", got, second)
		}
	})

	t.Run("StorePendingStaleRevision", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustMark(t, r, c, "T1", TriggerHead, t0)
		// The mark survives a change that is not to the source, so only the revision is stale.
		mustSet(t, r, SetRequest{Name: labName, Username: new("other"), At: t0.Add(time.Minute)})
		id, stored, err := r.StorePending(t.Context(), pendingReq(c, "T1", t0.Add(2*time.Minute)))
		if err != nil || stored || id != "" {
			t.Fatalf("StorePending = %q, %v, %v; want no pair, false, nil", id, stored, err)
		}
		if n := count(t, d, "cluster_pairs", "1 = 1") + count(t, d, "secret_versions", "1 = 1") + count(t, d, "host_renders", "1 = 1"); n != 0 {
			t.Fatalf("%d rows were written, want none", n)
		}
		if got := mustGet(t, r, labName); got.PendingPairID != "" || got.ProcessingTree != "T1" {
			t.Fatalf("cluster = %+v, want nothing recorded and the mark as it was", got)
		}
	})

	t.Run("StorePendingAfterMarkCleared", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustMark(t, r, c, "T1", TriggerHead, t0)
		mustSet(t, r, SetRequest{Name: labName, Ref: new("release"), At: t0.Add(time.Minute)})
		current := mustGet(t, r, labName)
		// The tree is the one the old source processed; the new revision is what the caller read last.
		req := pendingReq(current, "T1", t0.Add(2*time.Minute))
		if id, stored, err := r.StorePending(t.Context(), req); err != nil || stored || id != "" {
			t.Fatalf("StorePending without a mark = %q, %v, %v; want no pair, false, nil", id, stored, err)
		}
		if n := count(t, d, "cluster_pairs", "1 = 1"); n != 0 {
			t.Fatalf("pairs = %d, want 0", n)
		}
	})

	t.Run("StorePendingUnknownCluster", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := Cluster{ID: "gone"}
		if id, stored, err := r.StorePending(t.Context(), pendingReq(c, "T1", t0)); err != nil || stored || id != "" {
			t.Fatalf("StorePending for a removed cluster = %q, %v, %v; want no pair, false, nil", id, stored, err)
		}
	})

	t.Run("StorePendingRefreshSetsLastRefresh", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		execRaw(t, d, "UPDATE clusters SET refresh_incomplete = ? WHERE id = ?", true, c.ID)
		mustMark(t, r, c, "T1", TriggerRefresh, t0)
		req := pendingReq(c, "T1", t0.Add(time.Minute))
		req.Refresh = true
		if _, stored, err := r.StorePending(t.Context(), req); err != nil || !stored {
			t.Fatalf("StorePending = %v, %v; want true, nil", stored, err)
		}
		got := mustGet(t, r, labName)
		if !got.LastRefreshAt.Equal(t0.Add(time.Minute)) || got.RefreshIncomplete || got.PendingPairID == "" {
			t.Fatalf("cluster = %+v, want last_refresh_at at the store, refresh complete and a pending pair", got)
		}
	})

	t.Run("StorePendingRefreshKeepsLastHead", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustStore(t, r, c, "T1", t0)
		mustFail(t, r, c, "T2", "boom", t0.Add(time.Minute))
		// A refresh of the newest good tree: the failed tree and the last head belong to the head.
		mustMark(t, r, c, "T1", TriggerRefresh, t0.Add(2*time.Minute))
		req := pendingReq(c, "T1", t0.Add(3*time.Minute))
		req.Refresh = true
		pairID, stored, err := r.StorePending(t.Context(), req)
		if err != nil || !stored {
			t.Fatalf("StorePending = %v, %v; want true, nil", stored, err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if got.LastHeadTree != "T2" || got.FailedTree != "T2" || got.FailedErrorSealed == nil || got.PendingPairID != pairID {
			t.Fatalf("cluster = %+v, want last head T2 and failed tree T2 kept, %s pending", got, pairID)
		}
	})

	t.Run("StorePendingRefreshClearsFailedOfSameTree", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustStore(t, r, c, "T1", t0)
		mustFail(t, r, c, "T2", "boom", t0.Add(time.Minute))
		// The retry of the failed tree succeeds.
		mustMark(t, r, c, "T2", TriggerRefresh, t0.Add(2*time.Minute))
		req := pendingReq(c, "T2", t0.Add(3*time.Minute))
		req.Refresh = true
		if _, stored, err := r.StorePending(t.Context(), req); err != nil || !stored {
			t.Fatalf("StorePending = %v, %v; want true, nil", stored, err)
		}
		got := mustGet(t, r, labName)
		if got.FailedTree != "" || got.FailedErrorSealed != nil || got.LastHeadTree != "T2" {
			t.Fatalf("cluster = %+v, want the failed tree cleared and last head T2", got)
		}
	})

	t.Run("StoreFailedSealsError", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		text := "render: value " + leaked + " is not valid"
		mustFail(t, r, c, "T1", text, t0)
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if got.FailedTree != "T1" || got.LastHeadTree != "T1" {
			t.Fatalf("cluster = %+v, want T1 failed and recorded as the last head", got)
		}
		errs, err := r.Errors(t.Context(), c.ID)
		if err != nil || errs.Failed != text {
			t.Fatalf("Errors = %+v, %v; want the failed text", errs, err)
		}
		wantNotContain(t, "failed_error_sealed", scalar[[]byte](t, d, "SELECT failed_error_sealed FROM clusters WHERE id = ?", c.ID), leaked)
	})

	t.Run("StoreFailedRefusedWithoutMark", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		stored, err := r.StoreFailed(t.Context(), StoreFailedRequest{ClusterID: c.ID, Tree: "T1", Revision: 0, Error: "x", At: t0})
		if err != nil || stored {
			t.Fatalf("StoreFailed without a mark = %v, %v; want false, nil", stored, err)
		}
		if got := mustGet(t, r, labName); got.FailedTree != "" || got.LastHeadTree != "" {
			t.Fatalf("cluster = %+v, want nothing recorded", got)
		}
	})

	t.Run("RecordRefreshIncomplete", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustMark(t, r, c, "T1", TriggerRefresh, t0)
		ok, err := r.RecordRefresh(t.Context(), RefreshRecord{ClusterID: c.ID, Tree: "T1", Revision: 0, Incomplete: true, At: t0.Add(time.Minute)})
		if err != nil || !ok {
			t.Fatalf("RecordRefresh = %v, %v; want true, nil", ok, err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if !got.RefreshIncomplete || !got.LastRefreshAt.IsZero() || got.PendingPairID != "" {
			t.Fatalf("cluster = %+v, want refresh_incomplete, no last refresh and no pair", got)
		}
		if n := count(t, d, "cluster_pairs", "1 = 1"); n != 0 {
			t.Fatalf("pairs = %d, want 0", n)
		}
	})

	t.Run("RecordRefreshComplete", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		execRaw(t, d, "UPDATE clusters SET attempts = 2, refresh_incomplete = ? WHERE id = ?", true, c.ID)
		mustMark(t, r, c, "T1", TriggerRefresh, t0)
		at := t0.Add(time.Minute)
		ok, err := r.RecordRefresh(t.Context(), RefreshRecord{ClusterID: c.ID, Tree: "T1", Revision: 0, At: at})
		if err != nil || !ok {
			t.Fatalf("RecordRefresh = %v, %v; want true, nil", ok, err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if !got.LastRefreshAt.Equal(at) || got.RefreshIncomplete || got.Attempts != 0 {
			t.Fatalf("cluster = %+v, want last_refresh_at at the record, complete, attempts 0", got)
		}
	})

	t.Run("RecordRefreshRefusedOnStaleMark", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustMark(t, r, c, "T1", TriggerRefresh, t0)
		ok, err := r.RecordRefresh(t.Context(), RefreshRecord{ClusterID: c.ID, Tree: "T2", Revision: 0, At: t0})
		if err != nil || ok {
			t.Fatalf("RecordRefresh for another tree = %v, %v; want false, nil", ok, err)
		}
		if got := mustGet(t, r, labName); got.ProcessingTree != "T1" || !got.LastRefreshAt.IsZero() {
			t.Fatalf("cluster = %+v, want T1 still processing and no refresh recorded", got)
		}
	})

	t.Run("Errors", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		got, err := r.Errors(t.Context(), c.ID)
		if err != nil || got != (ClusterErrors{}) {
			t.Fatalf("Errors = %+v, %v; want three empty strings", got, err)
		}
		if _, err := r.Errors(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Errors of an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ErrorsWrongColumn", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustFail(t, r, c, "T1", "boom", t0)
		execRaw(t, d, "UPDATE clusters SET fetch_error_sealed = failed_error_sealed WHERE id = ?", c.ID)
		if _, err := r.Errors(t.Context(), c.ID); !errors.Is(err, datakey.ErrDecrypt) {
			t.Fatalf("Errors with a value moved to another column error = %v, want datakey.ErrDecrypt", err)
		}
	})

	t.Run("RecordFetch", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		text := "fetch " + leaked + ": connection refused"
		if err := r.RecordFetch(t.Context(), c.ID, errors.New(text), t0.Add(time.Minute)); err != nil {
			t.Fatalf("RecordFetch: %v", err)
		}
		errs, err := r.Errors(t.Context(), c.ID)
		if err != nil || errs.Fetch != text {
			t.Fatalf("Errors = %+v, %v; want the fetch text", errs, err)
		}
		if got := mustGet(t, r, labName); !got.LastFetchAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("LastFetchAt = %v, want the fetch time", got.LastFetchAt)
		}
		wantNotContain(t, "fetch_error_sealed", scalar[[]byte](t, d, "SELECT fetch_error_sealed FROM clusters WHERE id = ?", c.ID), leaked)
	})

	t.Run("RecordFetchClears", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if err := r.RecordFetch(t.Context(), c.ID, errors.New("down"), t0); err != nil {
			t.Fatalf("RecordFetch: %v", err)
		}
		if err := r.RecordFetch(t.Context(), c.ID, nil, t0.Add(time.Minute)); err != nil {
			t.Fatalf("RecordFetch(nil): %v", err)
		}
		got := mustGet(t, r, labName)
		if errs, err := r.Errors(t.Context(), c.ID); err != nil || errs.Fetch != "" || got.FetchErrorSealed != nil || !got.LastFetchAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("Errors = %+v, %v; cluster = %+v; want the fetch error cleared and the fetch time moved", errs, err, got)
		}
	})

	t.Run("WholeCallBackoff", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		f := WholeCallFailure{ClusterID: c.ID, Trigger: TriggerHead, Error: "unreachable", At: t0}
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("RecordWholeCallFailure: %v", err)
		}
		got := mustGet(t, r, labName)
		if got.Attempts != 1 || !got.NextAttemptAt.Equal(t0.Add(fleet.Backoff(1))) || !got.RateLimitedUntil.IsZero() {
			t.Fatalf("cluster = %+v, want attempts 1, the first backoff and no rate limit", got)
		}
		f.At = t0.Add(time.Hour)
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("second RecordWholeCallFailure: %v", err)
		}
		got = mustGet(t, r, labName)
		if got.Attempts != 2 || !got.NextAttemptAt.Equal(f.At.Add(fleet.Backoff(2))) {
			t.Fatalf("cluster = %+v, want attempts 2 and the second backoff from the second failure", got)
		}
	})

	t.Run("RateLimitedUntilNextDaily", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		nextDaily := t0.Add(24 * time.Hour)
		f := WholeCallFailure{ClusterID: c.ID, Trigger: TriggerHead, RateLimited: true, NextDaily: nextDaily, Error: "rate limited", At: t0}
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("RecordWholeCallFailure: %v", err)
		}
		if got := mustGet(t, r, labName); !got.RateLimitedUntil.Equal(nextDaily) || got.Attempts != 1 {
			t.Fatalf("cluster = %+v, want rate_limited_until at the next daily and attempts 1", got)
		}
	})

	t.Run("RefreshTriggerRearmed", func(t *testing.T) {
		for _, trigger := range []Trigger{TriggerRefresh, TriggerDaily, TriggerContinue} {
			t.Run(string(trigger), func(t *testing.T) {
				r, _ := testRepo(t, open)
				c := mustImport(t, r, labImport(t0))
				if err := r.RequestRefresh(t.Context(), labName, t0); err != nil {
					t.Fatalf("RequestRefresh: %v", err)
				}
				mustMark(t, r, c, "T1", trigger, t0)
				if trigger == TriggerRefresh {
					if got := mustGet(t, r, labName); got.RefreshRequested {
						t.Fatal("the mark did not consume the refresh request")
					}
				}
				f := WholeCallFailure{ClusterID: c.ID, Tree: "T1", Trigger: trigger, Error: "unreachable", At: t0.Add(time.Minute)}
				if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
					t.Fatalf("RecordWholeCallFailure: %v", err)
				}
				got := mustGet(t, r, labName)
				wantNoMark(t, got)
				if !got.RefreshRequested {
					t.Fatalf("cluster = %+v, want refresh_requested set again for the %s work", got, trigger)
				}
			})
		}
	})

	t.Run("HeadClearsMark", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		mustStore(t, r, c, "T0", t0)
		mustMark(t, r, c, "T1", TriggerHead, t0.Add(time.Minute))
		f := WholeCallFailure{ClusterID: c.ID, Tree: "T1", Trigger: TriggerHead, Error: "unreachable", At: t0.Add(2 * time.Minute)}
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("RecordWholeCallFailure: %v", err)
		}
		got := mustGet(t, r, labName)
		wantNoMark(t, got)
		if got.LastHeadTree != "T0" || got.RefreshRequested || got.FailedTree != "" {
			t.Fatalf("cluster = %+v, want the last head T0 unchanged, no refresh request and no failed tree", got)
		}
	})

	t.Run("LastCallErrorSealed", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		text := "resolve " + leaked + ": timeout"
		f := WholeCallFailure{ClusterID: c.ID, Trigger: TriggerHead, Error: text, At: t0}
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("RecordWholeCallFailure: %v", err)
		}
		errs, err := r.Errors(t.Context(), c.ID)
		if err != nil || errs.LastCall != text {
			t.Fatalf("Errors = %+v, %v; want the last call text", errs, err)
		}
		wantNotContain(t, "last_call_error_sealed", scalar[[]byte](t, d, "SELECT last_call_error_sealed FROM clusters WHERE id = ?", c.ID), leaked)
	})

	t.Run("RecordWholeCallFailureUnknown", func(t *testing.T) {
		r, _ := testRepo(t, open)
		err := r.RecordWholeCallFailure(t.Context(), WholeCallFailure{ClusterID: "gone", Trigger: TriggerHead, Error: "x", At: t0})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("RecordWholeCallFailure for an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ResetAttempts", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		f := WholeCallFailure{ClusterID: c.ID, Trigger: TriggerHead, Error: "unreachable", At: t0}
		if err := r.RecordWholeCallFailure(t.Context(), f); err != nil {
			t.Fatalf("RecordWholeCallFailure: %v", err)
		}
		if err := r.ResetAttempts(t.Context(), c.ID); err != nil {
			t.Fatalf("ResetAttempts: %v", err)
		}
		got := mustGet(t, r, labName)
		errs, err := r.Errors(t.Context(), c.ID)
		if err != nil || got.Attempts != 0 || !got.NextAttemptAt.IsZero() || errs.LastCall != "" {
			t.Fatalf("cluster = %+v, errors = %+v, %v; want attempts 0, no next attempt and no last call error", got, errs, err)
		}
	})

	t.Run("CredentialRoundTrip", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if err := r.SetCredential(t.Context(), c.ID, secret.Secret(leaked), t0); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		got, ok, err := r.Credential(t.Context(), c.ID)
		if err != nil || !ok || got.Expose() != leaked {
			t.Fatalf("Credential = %v, %v; want the stored value, true, nil", ok, err)
		}
		wantNotContain(t, "credential_sealed", scalar[[]byte](t, d, "SELECT credential_sealed FROM clusters WHERE id = ?", c.ID), leaked)
		if err := r.SetCredential(t.Context(), "gone", secret.Secret(leaked), t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetCredential for an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("CredentialAbsent", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		if got, ok, err := r.Credential(t.Context(), c.ID); err != nil || ok || got != "" {
			t.Fatalf("Credential = %v, %v, %v; want none", got, ok, err)
		}
		if _, _, err := r.Credential(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Credential of an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("Pair", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		got, err := r.Pair(t.Context(), id)
		if err != nil {
			t.Fatalf("Pair: %v", err)
		}
		if got.ID != id || got.ClusterID != c.ID || got.TreeHash != "T1" || got.SecretVersionID == "" || got.TalosVersion != talosVersion ||
			!got.CreatedAt.Equal(t0) || !got.ApprovedAt.IsZero() || got.ApprovedBy != "" {
			t.Fatalf("Pair = %+v, want the stored pending pair of T1", got)
		}
	})

	t.Run("PairUnknown", func(t *testing.T) {
		r, _ := testRepo(t, open)
		if _, err := r.Pair(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Pair error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ValuesRoundTrip", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		pair, err := r.Pair(t.Context(), mustStore(t, r, c, "T1", t0))
		if err != nil {
			t.Fatalf("Pair: %v", err)
		}
		got, err := r.Values(t.Context(), pair.SecretVersionID)
		if err != nil || len(got) != 1 || got["ref+op://vault/item/password"] != leaked {
			t.Fatalf("Values = %v, %v; want the stored value set", got, err)
		}
		sealed := scalar[[]byte](t, d, "SELECT values_sealed FROM secret_versions WHERE id = ?", pair.SecretVersionID)
		wantNotContain(t, "values_sealed", sealed, leaked)
		if sealed[0] != datakey.FormatV1 {
			t.Fatalf("values_sealed starts with %#x, want the format byte %#x", sealed[0], datakey.FormatV1)
		}
		if _, err := r.Values(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Values of an unknown version error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ValuesDamagedRow", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		pair, err := r.Pair(t.Context(), mustStore(t, r, c, "T1", t0))
		if err != nil {
			t.Fatalf("Pair: %v", err)
		}
		sealed := scalar[[]byte](t, d, "SELECT values_sealed FROM secret_versions WHERE id = ?", pair.SecretVersionID)
		execRaw(t, d, "UPDATE secret_versions SET values_sealed = ? WHERE id = ?", flipLast(sealed), pair.SecretVersionID)
		if _, err := r.Values(t.Context(), pair.SecretVersionID); !errors.Is(err, datakey.ErrDecrypt) {
			t.Fatalf("Values of a damaged row error = %v, want datakey.ErrDecrypt", err)
		}
	})

	t.Run("ValuesNotJSON", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		pair, err := r.Pair(t.Context(), mustStore(t, r, c, "T1", t0))
		if err != nil {
			t.Fatalf("Pair: %v", err)
		}
		key, err := datakey.Parse(secret.Secret(dataKey))
		if err != nil {
			t.Fatalf("parse the fixture data key: %v", err)
		}
		field := datakey.Field{Table: "secret_versions", Row: pair.SecretVersionID, Column: "values_sealed"}
		execRaw(t, d, "UPDATE secret_versions SET values_sealed = ? WHERE id = ?", key.SealField(field, []byte("not json "+leaked)), pair.SecretVersionID)
		_, err = r.Values(t.Context(), pair.SecretVersionID)
		if !errors.Is(err, datakey.ErrDecrypt) || strings.Contains(err.Error(), leaked) {
			t.Fatalf("Values of a row that is not a value set error = %v, want datakey.ErrDecrypt without its content", err)
		}
	})

	t.Run("HostRows", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		got, err := r.HostRows(t.Context(), id)
		if err != nil || len(got) != 2 {
			t.Fatalf("HostRows = %+v, %v; want two rows", got, err)
		}
		wants := labRenders()
		for i := range wants {
			row, want := got[i], &wants[i]
			if row.PairID != id || row.Host != want.Host || row.Role != want.Role || row.IP != want.IP ||
				row.ConfigHash != want.ConfigHash || row.DisplayHash != want.DisplayHash ||
				row.SchematicID != want.SchematicID || row.TalosVersion != want.TalosVersion {
				t.Fatalf("row %d = %+v, want the render of %s", i, row, want.Host)
			}
			if !bytes.Equal(row.Redacted, want.Redacted) || !bytes.Equal(row.SchematicBody, want.SchematicBody) {
				t.Fatalf("row %d redacted = %q, body = %q; want %q and %q", i, row.Redacted, row.SchematicBody, want.Redacted, want.SchematicBody)
			}
			if row.Drift || row.DriftConfigHash != "" || row.DriftDisplayHash != "" || row.DriftRedacted != nil {
				t.Fatalf("row %d = %+v, want no drift", i, row)
			}
		}
	})

	t.Run("HostRowsSealsRedacted", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		sealed := scalar[[]byte](t, d, "SELECT redacted_sealed FROM host_renders WHERE pair_id = ? AND host_name = ?", id, "cp-01")
		wantNotContain(t, "redacted_sealed", sealed, "token: ****")
		body := scalar[[]byte](t, d, "SELECT schematic_body FROM host_renders WHERE pair_id = ? AND host_name = ?", id, "cp-01")
		if string(body) != "customization: {}\n" {
			t.Fatalf("schematic_body = %q, want it stored plain", body)
		}
	})

	t.Run("HostRowsDamaged", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		sealed := scalar[[]byte](t, d, "SELECT redacted_sealed FROM host_renders WHERE pair_id = ? AND host_name = ?", id, "cp-01")
		execRaw(t, d, "UPDATE host_renders SET redacted_sealed = ? WHERE pair_id = ? AND host_name = ?", flipLast(sealed), id, "cp-01")
		if _, err := r.HostRows(t.Context(), id); !errors.Is(err, datakey.ErrDecrypt) {
			t.Fatalf("HostRows with a damaged row error = %v, want datakey.ErrDecrypt", err)
		}
	})

	t.Run("SetDrift", func(t *testing.T) {
		r, d := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		drift := DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("machine:\n  token: ****\n  drifted: true\n")}
		ok, err := r.SetDrift(t.Context(), SetDriftRequest{PairID: id, Host: "cp-01", ReadHash: "c1", Render: drift})
		if err != nil || !ok {
			t.Fatalf("SetDrift = %v, %v; want true, nil", ok, err)
		}
		rows, err := r.HostRows(t.Context(), id)
		if err != nil || len(rows) != 2 {
			t.Fatalf("HostRows = %+v, %v; want two rows", rows, err)
		}
		if cp := rows[0]; !cp.Drift || cp.DriftConfigHash != "dc" || cp.DriftDisplayHash != "dd" || !bytes.Equal(cp.DriftRedacted, drift.Redacted) || cp.ConfigHash != "c1" {
			t.Fatalf("cp-01 = %+v, want drift recorded beside its render", cp)
		}
		if wrk := rows[1]; wrk.Drift || wrk.DriftRedacted != nil {
			t.Fatalf("wrk-01 = %+v, want no drift", wrk)
		}
		wantNotContain(t, "drift_redacted_sealed", scalar[[]byte](t, d, "SELECT drift_redacted_sealed FROM host_renders WHERE pair_id = ? AND host_name = ?", id, "cp-01"), "drifted")
	})

	t.Run("SetDriftRefusedOnMismatch", func(t *testing.T) {
		r, _ := testRepo(t, open)
		c := mustImport(t, r, labImport(t0))
		id := mustStore(t, r, c, "T1", t0)
		drift := DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("x")}
		for name, req := range map[string]SetDriftRequest{
			"other pair":      {PairID: "gone", Host: "cp-01", ReadHash: "c1", Render: drift},
			"stale read hash": {PairID: id, Host: "cp-01", ReadHash: "stale", Render: drift},
			"unknown host":    {PairID: id, Host: "nope", ReadHash: "c1", Render: drift},
		} {
			if ok, err := r.SetDrift(t.Context(), req); err != nil || ok {
				t.Errorf("SetDrift(%s) = %v, %v; want false, nil", name, ok, err)
			}
		}
		rows, err := r.HostRows(t.Context(), id)
		if err != nil {
			t.Fatalf("HostRows: %v", err)
		}
		for i := range rows {
			if row := &rows[i]; row.Drift || row.DriftConfigHash != "" || row.DriftRedacted != nil {
				t.Fatalf("row %s = %+v, want nothing written", row.Host, *row)
			}
		}
	})
}
