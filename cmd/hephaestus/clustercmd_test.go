package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
)

const (
	labName = "lab"
	labRepo = "https://git.example.test/lab/cluster.git"
	labPath = "clusters/lab"
	// labSecret is a resolved secret value of the fixture clusters: no command may print it.
	labSecret     = "s3cr3t-value-7f3a"
	labTalos      = "v1.12.11"
	labCredential = "ref+op://vault/git/token"
)

// labHost is the render of a host whose redacted text is redacted, with the given hashes.
func labHost(host, redacted, configHash, displayHash string) render.HostRender {
	return render.HostRender{
		Host: host, Role: "worker", IP: "10.10.20.21",
		Config: []byte("machine: " + labSecret), Redacted: []byte(redacted), ConfigHash: configHash, DisplayHash: displayHash,
		SchematicID: plainSchematic, TalosVersion: labTalos,
	}
}

// labConfig is a redacted render of host at version n.
func labConfig(host string, n int) string {
	return "machine:\n  host: " + host + "\n  version: " + strconv.Itoa(n) + "\n  token: " + render.Marker + "\n"
}

// importLab imports the cluster lab through the repository.
func (c *cliTest) importLab(t *testing.T) clusters.Cluster {
	t.Helper()
	cl, err := c.clusters.Import(t.Context(), clusters.ImportRequest{
		Name: labName, RepoURL: labRepo, Ref: "main", Subpath: labPath, Username: "deploy", CredentialRef: labCredential, At: cliT0,
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return cl
}

// storePending stores renders as the pending pair of lab's tree and returns the pair id.
func (c *cliTest) storePending(t *testing.T, tree string, renders ...render.HostRender) string {
	t.Helper()
	cl, err := c.clusters.Get(t.Context(), labName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mark := clusters.MarkRequest{ClusterID: cl.ID, Tree: tree, Revision: cl.Revision, Trigger: clusters.TriggerHead, At: cliT0}
	if ok, err := c.clusters.MarkProcessing(t.Context(), mark); err != nil || !ok {
		t.Fatalf("MarkProcessing(%s) = %v, %v; want true, nil", tree, ok, err)
	}
	id, stored, err := c.clusters.StorePending(t.Context(), clusters.StorePendingRequest{
		ClusterID: cl.ID, Tree: tree, Revision: cl.Revision, Values: map[string]string{"ref+op://vault/item/field": labSecret},
		TalosVersion: labTalos, Renders: renders, At: cliT0,
	})
	if err != nil || !stored {
		t.Fatalf("StorePending(%s) = %q, %v, %v; want a pair id", tree, id, stored, err)
	}
	return id
}

// approveLab approves the pending pair id of lab.
func (c *cliTest) approveLab(t *testing.T, pairID string) {
	t.Helper()
	req := clusters.ApproveClusterRequest{Cluster: labName, PairID: pairID, Actor: fleet.ActorCLI, At: cliT0}
	if _, err := c.clusters.ApproveCluster(t.Context(), req); err != nil {
		t.Fatalf("ApproveCluster(%s): %v", pairID, err)
	}
}

// wantUsage fails unless err is a usage error, exit code 2.
func wantUsage(t *testing.T, err error, args ...string) {
	t.Helper()
	if err == nil || exitCode(err) != exitUsage {
		t.Errorf("hephaestus %s: error = %v, exit %d; want a usage error, exit %d", strings.Join(args, " "), err, exitCode(err), exitUsage)
	}
}

func TestClusterImport_JSON(t *testing.T) {
	c := newCLITest(t)

	out := c.mustRun(t, "cluster", "import", labName, "--repo", labRepo, "--ref", "main", "--path", labPath,
		"--username", "deploy", "--credential", labCredential, "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, "name", "repo", "ref", "path", "revision", "approved_pair_id", "pending_pair_id", "failed_tree",
		"last_fetch_at", "refresh_incomplete", "fetch_error", "failed_error", "last_call_error")
	for key, want := range map[string]any{"name": labName, "repo": labRepo, "ref": "main", "path": labPath, "revision": float64(0)} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	stored, err := c.clusters.Get(t.Context(), labName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Username != "deploy" || stored.CredentialRef != labCredential || !stored.CreatedAt.Equal(cliT0) {
		t.Errorf("stored = %+v, want the username, credential reference and creation time given", stored)
	}
	if actor, _ := c.audit(t, "cluster.imported"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
	c.storeAbsent(t)
}

func TestClusterImport_RefusesUserinfo(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// secret is text the refusal must not repeat.
		secret string
	}{
		{"userinfo", []string{"--repo", "https://deploy:hunter2-pw@git.example.test/x.git"}, "hunter2-pw"},
		{"plain http", []string{"--repo", "http://git.example.test/x.git"}, ""},
		{"pasted token", []string{"--repo", labRepo, "--credential", "ghp_pastedtoken123"}, "ghp_pastedtoken123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			args := append([]string{"cluster", "import", labName, "--ref", "main", "--path", labPath}, tt.args...)

			_, err := c.run(t, args...)

			if err == nil {
				t.Fatal("import succeeded, want a refusal")
			}
			if tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
				t.Errorf("error %q repeats %q", err, tt.secret)
			}
			if list, lerr := c.clusters.List(t.Context()); lerr != nil || len(list) != 0 {
				t.Errorf("clusters = %+v, %v; want none stored", list, lerr)
			}
		})
	}
}

func TestClusterImport_Duplicate(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	_, err := c.run(t, "cluster", "import", labName, "--repo", labRepo, "--ref", "main", "--path", labPath)

	if err == nil || !strings.Contains(err.Error(), "a cluster named "+labName+" exists") {
		t.Errorf("error = %v, want one saying a cluster named lab exists", err)
	}
}

// TestClusterRefHelp_NamesABranch: the fetch only follows a branch, so the help of both commands that take --ref says so.
func TestClusterRefHelp_NamesABranch(t *testing.T) {
	c := newCLITest(t)
	for _, sub := range []string{"import", "set"} {
		out := c.mustRun(t, "cluster", sub, "--help")
		if !strings.Contains(out, "branch to follow") || strings.Contains(out, "tag") {
			t.Errorf("cluster %s help does not describe --ref as a branch to follow:\n%s", sub, out)
		}
	}
}

func TestClusterSet_ChangedRef(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	pending := c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"))

	out := c.mustRun(t, "cluster", "set", labName, "--ref", "release", "--json")

	got := jsonObject(t, out)
	if got["revision"] != float64(1) || got["ref"] != "release" || got["pending_pair_id"] != "" {
		t.Errorf("output = %v, want revision 1, ref release and no pending pair", got)
	}
	if _, err := c.clusters.Pair(t.Context(), pending); !errors.Is(err, clusters.ErrNotFound) {
		t.Errorf("Pair(%s) error = %v, want ErrNotFound: a changed ref clears the pending pair", pending, err)
	}
	stored, err := c.clusters.Get(t.Context(), labName)
	if err != nil || stored.Ref != "release" || stored.RepoURL != labRepo || stored.Subpath != labPath || stored.Username != "deploy" {
		t.Errorf("stored = %+v, %v; want only the ref changed", stored, err)
	}
	if actor, _ := c.audit(t, "cluster.set"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestClusterSet_CredentialCanBeCleared(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	c.mustRun(t, "cluster", "set", labName, "--credential", "", "--username", "")

	stored, err := c.clusters.Get(t.Context(), labName)
	if err != nil || stored.CredentialRef != "" || stored.Username != "" {
		t.Errorf("stored = %+v, %v; want the credential reference and username cleared", stored, err)
	}
}

func TestClusterSet_Refusals(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	_, err := c.run(t, "cluster", "set", labName)
	wantUsage(t, err, "cluster", "set", labName)

	_, err = c.run(t, "cluster", "set", "nope", "--ref", "x")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Errorf("unknown cluster: error = %v, want one saying no cluster is named nope", err)
	}
	if stored, gerr := c.clusters.Get(t.Context(), labName); gerr != nil || stored.Revision != 0 {
		t.Errorf("stored = %+v, %v; want revision 0 after two refused sets", stored, gerr)
	}
}

func TestClusterRemove_InUse(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	c.approveLab(t, c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1")))
	m := c.machine(t, 1, fleet.ArchAMD64)
	req := clusters.MachineRequest{MachineID: m.ID, Cluster: labName, Host: "cp-01", Actor: fleet.ActorCLI, At: cliT0}
	if _, err := c.clusters.ApproveMachine(t.Context(), req); err != nil {
		t.Fatalf("ApproveMachine: %v", err)
	}

	_, err := c.run(t, "cluster", "remove", labName)

	if err == nil || !strings.Contains(err.Error(), "machines bound") {
		t.Errorf("error = %v, want a refusal naming the bound machines", err)
	}
	if _, gerr := c.clusters.Get(t.Context(), labName); gerr != nil {
		t.Errorf("Get after a refused remove: %v, want the cluster kept", gerr)
	}
}

func TestClusterRemove_JSON(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	out := c.mustRun(t, "cluster", "remove", labName, "--json")

	if got := jsonObject(t, out); got["name"] != labName || got["removed"] != true {
		t.Errorf("output = %v, want lab removed", got)
	}
	if _, err := c.clusters.Get(t.Context(), labName); !errors.Is(err, clusters.ErrNotFound) {
		t.Errorf("Get error = %v, want ErrNotFound", err)
	}
	if actor, _ := c.audit(t, "cluster.removed"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestClusterList_ShowsErrors(t *testing.T) {
	c := newCLITest(t)
	cl := c.importLab(t)
	mark := clusters.MarkRequest{ClusterID: cl.ID, Tree: "T9", Revision: cl.Revision, Trigger: clusters.TriggerHead, At: cliT0}
	if ok, err := c.clusters.MarkProcessing(t.Context(), mark); err != nil || !ok {
		t.Fatalf("MarkProcessing = %v, %v", ok, err)
	}
	// The stored texts hold an escape sequence, as a render error quoting a file could.
	failed := clusters.StoreFailedRequest{ClusterID: cl.ID, Tree: "T9", Revision: cl.Revision, Error: "patch broke\x1b[2K the render", At: cliT0}
	if ok, err := c.clusters.StoreFailed(t.Context(), failed); err != nil || !ok {
		t.Fatalf("StoreFailed = %v, %v", ok, err)
	}
	if err := c.clusters.RecordFetch(t.Context(), cl.ID, errors.New("fetch refused\x1b[2K"), cliT0); err != nil {
		t.Fatalf("RecordFetch: %v", err)
	}
	whole := clusters.WholeCallFailure{ClusterID: cl.ID, Trigger: clusters.TriggerHead, Error: "vault unreachable\x1b[2K", At: cliT0}
	if err := c.clusters.RecordWholeCallFailure(t.Context(), whole); err != nil {
		t.Fatalf("RecordWholeCallFailure: %v", err)
	}

	// The error texts can quote a secret fragment, so the routine listing says only that each exists.
	const hidden = "(hidden: pass --errors)"
	rows := jsonRows(t, c.mustRun(t, "cluster", "list", "--json"))
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	for key, w := range map[string]any{
		"name": labName, "failed_tree": "T9", "failed_error": hidden, "fetch_error": hidden, "last_call_error": hidden,
		"last_fetch_at": "2026-11-14T12:00:00Z", "refresh_incomplete": false,
	} {
		if rows[0][key] != w {
			t.Errorf("%s = %q, want %q", key, rows[0][key], w)
		}
	}
	routine := c.mustRun(t, "cluster", "list")
	for _, leaked := range []string{"patch broke", "fetch refused", "vault unreachable"} {
		if strings.Contains(routine, leaked) {
			t.Errorf("the routine listing prints %q:\n%s", leaked, routine)
		}
	}

	rows = jsonRows(t, c.mustRun(t, "cluster", "list", "--errors", "--json"))
	for key, w := range map[string]any{
		"failed_error": "patch broke[2K the render", "fetch_error": "fetch refused[2K", "last_call_error": "vault unreachable[2K",
	} {
		if rows[0][key] != w {
			t.Errorf("with --errors, %s = %q, want %q", key, rows[0][key], w)
		}
	}
	text := c.mustRun(t, "cluster", "list", "--errors")
	for _, w := range []string{"NAME", labName, "T9", `patch broke\x1b[2K the render`} {
		if !strings.Contains(text, w) {
			t.Errorf("text output lacks %q:\n%s", w, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Errorf("text output holds a raw escape character:\n%q", text)
	}
}

// TestClusterList_UnreadableErrorTextKeepsTheRest: one stored error text that does not open is shown as unreadable;
// the listing still prints the cluster and its other texts.
func TestClusterList_UnreadableErrorTextKeepsTheRest(t *testing.T) {
	c := newCLITest(t)
	cl := c.importLab(t)
	if err := c.clusters.RecordFetch(t.Context(), cl.ID, errors.New("fetch refused"), cliT0); err != nil {
		t.Fatalf("RecordFetch: %v", err)
	}
	whole := clusters.WholeCallFailure{ClusterID: cl.ID, Trigger: clusters.TriggerHead, Error: "vault unreachable", At: cliT0}
	if err := c.clusters.RecordWholeCallFailure(t.Context(), whole); err != nil {
		t.Fatalf("RecordWholeCallFailure: %v", err)
	}
	// A sealed text moved to another column does not open: its additional data names the column it was sealed for.
	if _, err := c.d.Bun().ExecContext(t.Context(), "UPDATE clusters SET fetch_error_sealed = last_call_error_sealed WHERE id = ?", cl.ID); err != nil {
		t.Fatalf("damage the fetch error: %v", err)
	}

	rows := jsonRows(t, c.mustRun(t, "cluster", "list", "--errors", "--json"))

	if len(rows) != 1 || rows[0]["fetch_error"] != "(unreadable)" || rows[0]["last_call_error"] != "vault unreachable" {
		t.Fatalf("rows = %v, want the fetch error unreadable and the last call error shown", rows)
	}
}

func TestClusterList_Empty(t *testing.T) {
	c := newCLITest(t)

	if got := c.mustRun(t, "cluster", "list", "--json"); strings.TrimSpace(got) != "[]" {
		t.Errorf("--json output = %q, want []", got)
	}
	if got := c.mustRun(t, "cluster", "list"); !strings.Contains(got, "no clusters") {
		t.Errorf("text output = %q, want a note that there are no clusters", got)
	}
}

func TestClusterDiff_PendingVsApproved(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	c.approveLab(t, c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1")))
	pending := c.storePending(t, "T2",
		labHost("cp-01", labConfig("cp-01", 2), "c2", "d2"), labHost("cp-02", labConfig("cp-02", 1), "c3", "d3"))

	out := c.mustRun(t, "cluster", "diff", labName)

	for _, want := range []string{
		"--- approved/cp-01\n", "+++ pending/cp-01\n", "-  version: 1\n", "+  version: 2\n",
		"+++ pending/cp-02\n", "cluster approve " + labName + " " + pending,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, labSecret) {
		t.Errorf("output prints a secret value:\n%s", out)
	}

	obj := jsonObject(t, c.mustRun(t, "cluster", "diff", labName, "--json"))
	if obj["name"] != labName || obj["pair_id"] != pending {
		t.Errorf("JSON name, pair_id = %v, %v; want lab and %s", obj["name"], obj["pair_id"], pending)
	}
	hosts, _ := obj["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("JSON hosts = %v, want two", obj["hosts"])
	}
	first, _ := hosts[0].(map[string]any)
	wantKeys(t, first, "host", "diff", "old_hash", "new_hash", "secret_values_changed")
	diff, _ := first["diff"].(string)
	if first["host"] != "cp-01" || first["old_hash"] != "c1" || first["new_hash"] != "c2" ||
		!strings.Contains(diff, "--- approved/cp-01\n") || strings.Contains(diff, labSecret) {
		t.Errorf("first JSON host = %v, want cp-01 c1 -> c2 with its unified diff", first)
	}
}

func TestClusterDiff_SecretValuesChanged(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	c.approveLab(t, c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1")))
	// Same display, different config: only a secret value moved.
	c.storePending(t, "T2", labHost("cp-01", labConfig("cp-01", 1), "c2", "d1"))

	out := c.mustRun(t, "cluster", "diff", labName)

	if !strings.Contains(out, "secret values changed\n") {
		t.Errorf("output lacks the secret values changed line:\n%s", out)
	}
	if strings.Contains(out, "---") || strings.Contains(out, labSecret) {
		t.Errorf("output holds a diff or a secret value, want neither:\n%s", out)
	}
	obj := jsonObject(t, c.mustRun(t, "cluster", "diff", labName, "--json"))
	hosts, _ := obj["hosts"].([]any)
	first, _ := hosts[0].(map[string]any)
	if len(hosts) != 1 || first["secret_values_changed"] != true || first["diff"] != "" {
		t.Errorf("JSON hosts = %v, want cp-01 with secret_values_changed and an empty diff", obj["hosts"])
	}
}

func TestClusterDiff_NoPendingPair(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	_, err := c.run(t, "cluster", "diff", labName)

	if err == nil || !strings.Contains(err.Error(), "no pending pair") {
		t.Errorf("error = %v, want one saying lab has no pending pair", err)
	}
}

func TestClusterDiff_HostDrift(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	approved := c.storePending(t, "T1",
		labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"), labHost("wrk-01", labConfig("wrk-01", 1), "c9", "d9"))
	c.approveLab(t, approved)
	drift := clusters.SetDriftRequest{
		PairID: approved, Host: "cp-01", ReadHash: "c1",
		Render: clusters.DriftRender{ConfigHash: "cd", DisplayHash: "dd", Redacted: []byte(labConfig("cp-01", 7))},
	}
	if ok, err := c.clusters.SetDrift(t.Context(), drift); err != nil || !ok {
		t.Fatalf("SetDrift = %v, %v; want true, nil", ok, err)
	}

	out := c.mustRun(t, "cluster", "diff", labName, "--host", "cp-01")

	for _, want := range []string{
		"--- stored/cp-01\n", "+++ drift/cp-01\n", "-  version: 1\n", "+  version: 7\n",
		"cluster approve " + labName + " --host cp-01 --hash cd",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, labSecret) {
		t.Errorf("output prints a secret value:\n%s", out)
	}

	_, err := c.run(t, "cluster", "diff", labName, "--host", "wrk-01")
	if err == nil || !strings.Contains(err.Error(), "wrk-01") {
		t.Errorf("host without drift: error = %v, want one naming wrk-01", err)
	}
}

func TestClusterApprove_Pair(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	pending := c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"))

	out := c.mustRun(t, "cluster", "approve", labName, pending, "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, "name", "pair_id", "orphaned", "rearmed", "revoked")
	if got["name"] != labName || got["pair_id"] != pending {
		t.Errorf("output = %v, want lab and pair %s", got, pending)
	}
	stored, err := c.clusters.Get(t.Context(), labName)
	if err != nil || stored.ApprovedPairID != pending || stored.PendingPairID != "" {
		t.Errorf("stored = %+v, %v; want %s approved and nothing pending", stored, err, pending)
	}
	if actor, _ := c.audit(t, "cluster.approved"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestClusterApprove_Stale(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	shown := c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"))
	c.storePending(t, "T2", labHost("cp-01", labConfig("cp-01", 2), "c2", "d2")) // replaces the pair the admin was shown

	_, err := c.run(t, "cluster", "approve", labName, shown)

	wantUsage(t, err, "cluster", "approve", labName, shown)
	if err == nil || !strings.Contains(err.Error(), "no longer pending") {
		t.Errorf("error = %v, want one saying the pair is no longer pending", err)
	}
	if stored, gerr := c.clusters.Get(t.Context(), labName); gerr != nil || stored.ApprovedPairID != "" {
		t.Errorf("stored = %+v, %v; want nothing approved by a stale approval", stored, gerr)
	}
}

func TestClusterApprove_DriftHash(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	approved := c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"))
	c.approveLab(t, approved)
	drift := clusters.SetDriftRequest{
		PairID: approved, Host: "cp-01", ReadHash: "c1",
		Render: clusters.DriftRender{ConfigHash: "cd", DisplayHash: "dd", Redacted: []byte(labConfig("cp-01", 7))},
	}
	if ok, err := c.clusters.SetDrift(t.Context(), drift); err != nil || !ok {
		t.Fatalf("SetDrift = %v, %v; want true, nil", ok, err)
	}

	// A hash the drift no longer has is stale, in the drift's own words.
	_, err := c.run(t, "cluster", "approve", labName, "--host", "cp-01", "--hash", "old")
	wantUsage(t, err, "cluster", "approve", labName, "--host", "cp-01", "--hash", "old")
	if err == nil || !strings.Contains(err.Error(), "the drift changed") || strings.Contains(err.Error(), "pair is no longer pending") {
		t.Errorf("stale drift: error = %v, want drift wording and not the pair's", err)
	}

	out := c.mustRun(t, "cluster", "approve", labName, "--host", "cp-01", "--hash", "cd", "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, "name", "host", "hash")
	if got["name"] != labName || got["host"] != "cp-01" || got["hash"] != "cd" {
		t.Errorf("output = %v, want lab cp-01 cd", got)
	}
	rows, err := c.clusters.HostRows(t.Context(), approved)
	if err != nil || len(rows) != 1 || rows[0].Drift || rows[0].ConfigHash != "cd" {
		t.Errorf("host rows = %+v, %v; want the drift hash stored and the drift cleared", rows, err)
	}
	if actor, _ := c.audit(t, "cluster.drift_approved"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestClusterApprove_Usage(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)
	for _, args := range [][]string{
		{"cluster", "approve", labName},
		{"cluster", "approve", labName, "P1", "--host", "cp-01"},
		{"cluster", "approve", labName, "P1", "--hash", "cd"},
		{"cluster", "approve", labName, "--host", "cp-01"},
		{"cluster", "approve", labName, "--hash", "cd"},
		{"cluster", "approve", labName, "P1", "P2"},
		{"cluster", "approve"},
	} {
		_, err := c.run(t, args...)
		wantUsage(t, err, args...)
	}
}

func TestClusterRefresh_SetsFlag(t *testing.T) {
	c := newCLITest(t)
	c.importLab(t)

	out := c.mustRun(t, "cluster", "refresh", labName, "--json")

	if got := jsonObject(t, out); got["name"] != labName || got["refresh_requested"] != true {
		t.Errorf("output = %v, want lab with refresh_requested", got)
	}
	if stored, err := c.clusters.Get(t.Context(), labName); err != nil || !stored.RefreshRequested {
		t.Errorf("stored = %+v, %v; want the refresh flag set", stored, err)
	}
	if actor, detail := c.audit(t, "cluster.refresh_requested"); actor != fleet.ActorCLI || detail != `{"name":"lab"}` {
		t.Errorf("audit actor = %q, detail = %s; want %q and the cluster's name", actor, detail, fleet.ActorCLI)
	}
	_, err := c.run(t, "cluster", "refresh", "nope")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Errorf("unknown cluster: error = %v, want one saying no cluster is named nope", err)
	}
}

func TestCLI_NeverOpensStores(t *testing.T) {
	c := newCLITest(t)
	c.mustRun(t, "cluster", "import", labName, "--repo", labRepo, "--ref", "main", "--path", labPath)
	pending := c.storePending(t, "T1", labHost("cp-01", labConfig("cp-01", 1), "c1", "d1"))

	for _, args := range [][]string{
		{"cluster", "list"}, {"cluster", "diff", labName}, {"cluster", "approve", labName, pending},
		{"cluster", "set", labName, "--ref", "release"}, {"cluster", "refresh", labName}, {"cluster", "list", "--json"},
		{"audit", "list"}, {"cluster", "remove", labName},
	} {
		c.mustRun(t, args...)
		c.storeAbsent(t)
	}
}

func TestCLI_ClusterGroupsRejectStrayArguments(t *testing.T) {
	c := newCLITest(t)
	for _, args := range [][]string{{"cluster", "bogus"}, {"audit", "bogus"}, {"cluster", "list", "extra"}, {"audit", "list", "extra"}} {
		_, err := c.run(t, args...)
		wantUsage(t, err, args...)
	}
}

func TestClusterImport_RequiresRepoRefAndPath(t *testing.T) {
	c := newCLITest(t)
	for _, args := range [][]string{
		{"cluster", "import", labName},
		{"cluster", "import", labName, "--repo", labRepo, "--ref", "main"},
		{"cluster", "import", labName, "--repo", labRepo, "--path", labPath},
		{"cluster", "import", labName, "--ref", "main", "--path", labPath},
		{"cluster", "import", "--repo", labRepo, "--ref", "main", "--path", labPath},
	} {
		_, err := c.run(t, args...)
		wantUsage(t, err, args...)
	}
	if list, err := c.clusters.List(t.Context()); err != nil || len(list) != 0 {
		t.Errorf("clusters = %+v, %v; want none stored", list, err)
	}
}
