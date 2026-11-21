package boot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// The clusters service is what serve passes as Config.Configs.
var _ ConfigSource = (*clusters.Service)(nil)

const (
	plainSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	talosVersion   = "v1.12.11"
)

// testToken is a 43-character ticket token, the length MintTicket mints.
const testToken = "TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT"

// installReq is the fixture install request with cmdline and token.
func installReq(cmdline, token string) InstallRequest {
	return InstallRequest{
		Address: netip.MustParseAddr(testAddress), FQDN: testFQDN, Schematic: plainSchematic,
		Version: talosVersion, Arch: fleet.ArchAMD64, Cmdline: cmdline, Token: token,
	}
}

// countedLength is the kernel command line length InstallScript checks, written
// out term by term: the kernel's name and a space, the cached command line, the
// two Talos arguments, the FQDN, the config path, a 36-character UUID and a
// 43-character token.
func countedLength(cmdline, fqdn string) int {
	return len("k ") + len(cmdline) + len(" talos.halt_if_installed=1 talos.config=https://") + len(fqdn) +
		len("/config?u=") + 36 + len("&t=") + 43
}

func TestInstallScript_Exact(t *testing.T) {
	got, err := InstallScript(installReq("console=tty0", testToken))
	if err != nil {
		t.Fatalf("InstallScript: %v", err)
	}

	base := "http://10.10.20.5/t/" + plainSchematic + "/v1.12.11/amd64/"
	want := "#!ipxe\n" +
		"imgfree\n" +
		"kernel --name k " + base + "kernel console=tty0 talos.halt_if_installed=1 " +
		"talos.config=https://hephaestus.example.test/config?u=${uuid}&t=" + testToken + "\n" +
		"initrd " + base + "initramfs\n" +
		"boot\n"
	if got != want {
		t.Errorf("InstallScript =\n%s\nwant\n%s", got, want)
	}
}

func TestInstallScript_LengthBoundary(t *testing.T) {
	fits := MaxCommandLine - countedLength("", testFQDN)
	cases := []struct {
		name    string
		cmdline string
		wantErr error
	}{
		{"exactly 2046", strings.Repeat("a", fits), nil},
		{"2047", strings.Repeat("a", fits+1), ErrCommandLineTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := InstallScript(installReq(tc.cmdline, testToken))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("InstallScript with a %d-byte command line (counted %d) = %v, want %v",
					len(tc.cmdline), countedLength(tc.cmdline, testFQDN), err, tc.wantErr)
			}
		})
	}
}

func FuzzInstallScript(f *testing.F) {
	fits := MaxCommandLine - countedLength("", testFQDN)
	for _, c := range []struct{ cmdline, fqdn string }{
		{"console=tty0", testFQDN},
		{strings.Repeat("a", fits), testFQDN},
		{strings.Repeat("a", fits+1), testFQDN},
		{"", ""},
		{"console=ttyS0,115200 talos.platform=metal", strings.Repeat("h", 300)},
	} {
		f.Add(c.cmdline, c.fqdn)
	}
	f.Fuzz(func(t *testing.T, cmdline, fqdn string) {
		req := installReq(cmdline, testToken)
		req.FQDN = fqdn
		script, err := InstallScript(req)
		n := countedLength(cmdline, fqdn)
		switch {
		case err == nil && n > MaxCommandLine:
			t.Errorf("InstallScript accepted a counted length of %d, over %d", n, MaxCommandLine)
		case err != nil && !errors.Is(err, ErrCommandLineTooLong):
			t.Errorf("InstallScript = %v, want nil or ErrCommandLineTooLong", err)
		case err != nil && n <= MaxCommandLine:
			t.Errorf("InstallScript refused a counted length of %d, within %d", n, MaxCommandLine)
		case err == nil && !strings.HasPrefix(script, "#!ipxe\nimgfree\n"):
			t.Errorf("script = %q, want the iPXE install script", script)
		}
	})
}

const (
	testCluster = "lab"
	testHost    = "cp-01"
	// fixtureCmdline is the fixture row's cached command line.
	fixtureCmdline = "console=tty0"
)

// fakeConfigs is a ConfigSource whose answers the test sets. It records the
// machines EnsureAssets was asked for.
type fakeConfigs struct {
	drift     bool
	pair      clusters.Pair
	ensureErr error
	ensured   []string
}

func newFakeConfigs() *fakeConfigs {
	return &fakeConfigs{pair: clusters.Pair{ID: "pair-1", ClusterID: testCluster, SecretVersionID: "version-1", TalosVersion: talosVersion}}
}

func (f *fakeConfigs) Drift(context.Context, fleet.Machine) (bool, error) { return f.drift, nil }

func (f *fakeConfigs) EnsureAssets(_ context.Context, m fleet.Machine, _ time.Time) error {
	f.ensured = append(f.ensured, m.ID)
	return f.ensureErr
}

func (f *fakeConfigs) Approved(_ context.Context, clusterID string) (clusters.Pair, error) {
	if clusterID != f.pair.ClusterID {
		return clusters.Pair{}, fmt.Errorf("approved pair of %s: %w", clusterID, clusters.ErrNotFound)
	}
	return f.pair, nil
}

func (f *fakeConfigs) Render(context.Context, fleet.Ticket) ([]byte, error) {
	return nil, errors.New("fakeConfigs: Render is not used by /boot")
}

// newInstallRig is newRig with src as the config source.
func newInstallRig(t *testing.T, src *fakeConfigs, opts ...Option) *rig {
	t.Helper()
	return newRigWith(t, func(c *Config) { c.Configs = src }, opts...)
}

// fixtureTalosKey is the talos_cache key of the fixture machine's tuple.
func fixtureTalosKey() fleet.TalosKey {
	return fleet.TalosKey{Schematic: plainSchematic, Version: talosVersion, Arch: fleet.ArchAMD64}
}

// talosContent is the fixture's Talos assets by part, with cmdline as the
// command line.
func talosContent(cmdline string) map[fleet.TalosPart][]byte {
	return map[fleet.TalosPart][]byte{
		fleet.TalosKernel:    bytes.Repeat([]byte("K"), 4096),
		fleet.TalosInitramfs: bytes.Repeat([]byte("I"), 2048),
		fleet.TalosCmdline:   []byte(cmdline),
	}
}

// pushTalos pushes the fixture assets with cmdline to the store and tags them
// under the fixture key, as a finished fetch does, and returns what they are.
func (g *rig) pushTalos(cmdline string) fleet.TalosAssets {
	g.t.Helper()
	content := talosContent(cmdline)
	assets := fleet.TalosAssets{}
	var layers []ocispec.Descriptor
	for _, p := range fleet.TalosParts() {
		b := content[p]
		sum := sha256.Sum256(b)
		a := fleet.TalosAsset{Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
		l, err := store.Layer(string(p), a.Size, a.SHA256)
		if err != nil {
			g.t.Fatalf("store.Layer: %v", err)
		}
		if err := g.st.PushBlob(g.t.Context(), l, bytes.NewReader(b)); err != nil {
			g.t.Fatalf("PushBlob %s: %v", p, err)
		}
		layers = append(layers, l)
		assets[p] = a
	}
	if _, err := g.st.TagArtifact(g.t.Context(), fixtureTalosKey().Tag(), "application/vnd.hephaestus.talos.v1", layers); err != nil {
		g.t.Fatalf("TagArtifact: %v", err)
	}
	return assets
}

// readyTalos makes the fixture key's row ready with assets recorded.
func (g *rig) readyTalos(assets fleet.TalosAssets) {
	g.t.Helper()
	ctx := g.t.Context()
	if _, err := fleet.WantTalosTx(ctx, g.d.Bun(), fleet.TalosWant{Key: fixtureTalosKey()}, testNow()); err != nil {
		g.t.Fatalf("WantTalosTx: %v", err)
	}
	row, ok, err := g.repo.ClaimTalosRow(ctx, fixtureTalosKey(), testNow())
	if err != nil || !ok {
		g.t.Fatalf("ClaimTalosRow = %v, %v; want the wanted row claimed", ok, err)
	}
	if moved, err := g.repo.MarkTalosReady(ctx, row.Claim(), assets, testNow()); err != nil || !moved {
		g.t.Fatalf("MarkTalosReady = %v, %v; want the row ready", moved, err)
	}
}

// stockTalos is pushTalos then readyTalos with what was pushed.
func (g *rig) stockTalos(cmdline string) {
	g.t.Helper()
	g.readyTalos(g.pushTalos(cmdline))
}

// talosRow is the fixture key's row, which must exist.
func (g *rig) talosRow() fleet.TalosRow {
	g.t.Helper()
	row, ok, err := g.repo.Talos(g.t.Context(), fixtureTalosKey())
	if err != nil || !ok {
		g.t.Fatalf("Talos = %v, %v; want a row", ok, err)
	}
	return row
}

// talosBlob is the path of part's blob in the store, as the row records it.
func (g *rig) talosBlob(p fleet.TalosPart) string {
	g.t.Helper()
	return filepath.Join(g.storeRoot, "blobs", "sha256", g.talosRow().Assets[p].SHA256)
}

// assign makes the fixture machine by a first /boot and binds it, assigned, to
// the fixture host with the fixture Talos tuple.
func (g *rig) assign() {
	g.t.Helper()
	g.boot(bootQ)
	g.exec("UPDATE machines SET state = ?, cluster_id = ?, host_name = ?, talos_schematic = ?, talos_version = ? WHERE smbios_uuid = ?",
		string(fleet.StateAssigned), testCluster, testHost, plainSchematic, talosVersion, bootUUID)
}

// wantHold fails unless body is a hold with reason.
func wantHold(t *testing.T, body, reason string) {
	t.Helper()
	if !strings.Contains(body, "echo hephaestus: holding: "+reason+"\n") {
		t.Errorf("body = %q, want a hold on %q", body, reason)
	}
}

// metricLine returns the value of the exposition line that starts with prefix,
// and false when there is none.
func metricLine(body, prefix string) (string, bool) {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line[strings.LastIndexByte(line, ' ')+1:], true
		}
	}
	return "", false
}

func TestBoot_AssignedNotReadyHoldsRow8AndEnsures(t *testing.T) {
	src := newFakeConfigs()
	g := newInstallRig(t, src)
	g.assign()

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if id := g.machine().ID; len(src.ensured) != 1 || src.ensured[0] != id {
		t.Errorf("EnsureAssets calls = %v, want one for %s", src.ensured, id)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 8 AND action = 'hold'"); n != 1 {
		t.Errorf("row 8 hold events = %d, want 1", n)
	}
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
}

// TestBoot_EnsureAssetsNotBoundIsQuiet pins that a machine not bound to a host
// of an approved pair has nothing to fetch: it holds at row 8 and nothing is
// reported.
func TestBoot_EnsureAssetsNotBoundIsQuiet(t *testing.T) {
	src := newFakeConfigs()
	src.ensureErr = fmt.Errorf("clusters: want assets: %w", clusters.ErrNotFound)
	g := newInstallRig(t, src)
	g.assign()

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if len(src.ensured) != 1 {
		t.Errorf("EnsureAssets calls = %v, want one", src.ensured)
	}
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
}

func TestBoot_EnsureAssetsFailureIsReported(t *testing.T) {
	src := newFakeConfigs()
	src.ensureErr = errors.New("database is down")
	g := newInstallRig(t, src)
	g.assign()

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if len(g.errs) != 1 || !errors.Is(g.errs[0], src.ensureErr) {
		t.Errorf("reported errors = %v, want the EnsureAssets failure", g.errs)
	}
}

func TestBoot_AssignedDriftHoldsRow9(t *testing.T) {
	src := newFakeConfigs()
	src.drift = true
	g := newInstallRig(t, src)
	g.assign()
	g.stockTalos(fixtureCmdline)

	wantHold(t, g.boot(bootQ), "configuration drift: re-approval required")

	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 9 AND action = 'hold'"); n != 1 {
		t.Errorf("row 9 hold events = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
	if len(src.ensured) != 0 {
		t.Errorf("EnsureAssets calls = %v, want none for ready assets", src.ensured)
	}
}

func TestBoot_AssignedReadyServesInstallScript(t *testing.T) {
	metrics := newMetrics(t)
	src := newFakeConfigs()
	g := newInstallRig(t, src, WithMeter(metrics.Meter()))
	g.assign()
	g.stockTalos(fixtureCmdline)

	body := g.boot(bootQ)

	_, token, ok := strings.Cut(body, "&t=")
	token, _, _ = strings.Cut(token, "\n")
	if !ok || len(token) != 43 {
		t.Fatalf("body = %q, want a 43-character ticket token after &t=", body)
	}
	want, err := InstallScript(installReq(fixtureCmdline, token))
	if err != nil {
		t.Fatalf("InstallScript: %v", err)
	}
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	sum := sha256.Sum256([]byte(token))
	m := g.machine()
	if n := g.count("SELECT count(*) FROM tickets WHERE token_sha256 = ? AND machine_id = ? AND cluster_id = ? AND host_name = ? AND pair_id = ? AND secret_version_id = ? AND consumed_at IS NULL AND revoked_at IS NULL",
		hex.EncodeToString(sum[:]), m.ID, testCluster, testHost, "pair-1", "version-1"); n != 1 {
		t.Errorf("live tickets for the token, machine, host and the approved pair = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 10 AND action = 'install'"); n != 1 {
		t.Errorf("row 10 install events = %d, want 1", n)
	}
	if v, ok := metricLine(scrapeMetrics(t, metrics), "hephaestus_tickets_minted_total"); !ok || v != "1" {
		t.Errorf("hephaestus_tickets_minted_total = %q (found %t), want 1", v, ok)
	}
}

// TestBoot_InstallMintsAFreshTicketEachBoot pins that a second install boot
// revokes the first ticket and mints another.
func TestBoot_InstallMintsAFreshTicketEachBoot(t *testing.T) {
	g := newInstallRig(t, newFakeConfigs())
	g.assign()
	g.stockTalos(fixtureCmdline)

	first, second := g.boot(bootQ), g.boot(bootQ)

	if first == second {
		t.Error("the two install scripts are identical, want a fresh token")
	}
	if n := g.count("SELECT count(*) FROM tickets"); n != 2 {
		t.Errorf("tickets = %d, want 2", n)
	}
	if n := g.count("SELECT count(*) FROM tickets WHERE consumed_at IS NULL AND revoked_at IS NULL"); n != 1 {
		t.Errorf("live tickets = %d, want 1", n)
	}
}

func TestBoot_ApprovedPairFailureIs500(t *testing.T) {
	src := newFakeConfigs()
	g := newInstallRig(t, src)
	g.assign()
	g.stockTalos(fixtureCmdline)
	src.pair.ClusterID = "another"

	rec := g.secure("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusInternalServerError)
	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
}

func TestBoot_CmdlineTooLongHolds(t *testing.T) {
	metrics := newMetrics(t)
	g := newInstallRig(t, newFakeConfigs(), WithMeter(metrics.Meter()))
	g.assign()
	cmdline := strings.Repeat("a", MaxCommandLine-countedLength("", testFQDN)+1)
	g.stockTalos(cmdline)

	wantHold(t, g.boot(bootQ), "kernel command line too long")

	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 10 AND action = 'hold' AND reason = 'kernel command line too long'"); n != 1 {
		t.Errorf("row 10 too-long hold events = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
	m := g.machine()
	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.cmdline_too_long' AND actor = 'system' AND machine_id = ? AND detail LIKE ? AND detail LIKE ?",
		m.ID, `%"machine_id":"`+m.ID+`"%`, `%"length":2047%`); n != 1 {
		t.Errorf("alert.cmdline_too_long audit rows naming the machine and length 2047 = %d, want 1", n)
	}
	if v, ok := metricLine(scrapeMetrics(t, metrics), `hephaestus_alerts_total{kind="cmdline_too_long"`); !ok || v != "1" {
		t.Errorf(`hephaestus_alerts_total{kind="cmdline_too_long"} = %q (found %t), want 1`, v, ok)
	}
}

func TestBoot_ReadyRowLostLayerRequeues(t *testing.T) {
	src := newFakeConfigs()
	g := newInstallRig(t, src)
	g.assign()
	g.stockTalos(fixtureCmdline)
	if err := os.Remove(g.talosBlob(fleet.TalosKernel)); err != nil {
		t.Fatalf("remove kernel blob: %v", err)
	}

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if got := g.talosRow().State; got != fleet.CacheWanted {
		t.Errorf("Talos row state = %q, want %q", got, fleet.CacheWanted)
	}
	if len(g.errs) != 1 || !errors.Is(g.errs[0], errTalosMissing) {
		t.Errorf("reported errors = %v, want one errTalosMissing", g.errs)
	}
	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
}

func TestBoot_ReadyRowDigestMismatchNotReady(t *testing.T) {
	g := newInstallRig(t, newFakeConfigs())
	g.assign()
	assets := g.pushTalos(fixtureCmdline)
	k := assets[fleet.TalosKernel]
	k.SHA256 = strings.Repeat("ab", 32)
	assets[fleet.TalosKernel] = k
	g.readyTalos(assets)

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if got := g.talosRow().State; got != fleet.CacheWanted {
		t.Errorf("Talos row state = %q, want %q (the tag is not the recorded content)", got, fleet.CacheWanted)
	}
	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
}

func TestBoot_ReadyRowUnreadableNotReady(t *testing.T) {
	g := newInstallRig(t, newFakeConfigs())
	g.assign()
	g.stockTalos(fixtureCmdline)
	g.makeUnreadable(g.talosBlob(fleet.TalosKernel))

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if got := g.talosRow().State; got != fleet.CacheReady {
		t.Errorf("Talos row state = %q, want it left %q", got, fleet.CacheReady)
	}
	if len(g.errs) != 1 || errors.Is(g.errs[0], errTalosMissing) {
		t.Errorf("reported errors = %v, want one that is not errTalosMissing", g.errs)
	}
}
