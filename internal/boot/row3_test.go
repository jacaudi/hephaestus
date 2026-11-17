package boot

import (
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	entryName = "tshell"
	entryArch = "amd64"

	clearedAction = "boot_next.cleared"
)

// shellKey is the cache row key of the fixture entry for the fixture machine.
func shellKey() fleet.CacheKey { return fleet.CacheKey{Entry: entryName, Arch: fleet.ArchAMD64} }

// shell is the fixture catalog's tshell entry for amd64.
func (g *rig) shell() catalog.Entry {
	g.t.Helper()
	e, ok := g.fix.Catalog.Lookup(entryName, entryArch)
	if !ok {
		g.t.Fatalf("fixture catalog has no %s/%s", entryName, entryArch)
	}
	return e
}

// flag arms the fixture machine's boot-next with entry, set at at, as the CLI
// does. The machine must exist.
func (g *rig) flag(entry string, at time.Time) {
	g.t.Helper()
	g.flagVia(g.repo, entry, at)
}

// flagVia is flag through repo, a second repository over the same database.
func (g *rig) flagVia(repo *fleet.Repository, entry string, at time.Time) {
	g.t.Helper()
	req := fleet.SetBootNextRequest{MachineID: g.machine().ID, Entry: entry, Pin: g.shell().Pin.ID, At: at}
	if _, err := repo.SetBootNext(g.t.Context(), req); err != nil {
		g.t.Fatalf("SetBootNext: %v", err)
	}
}

// armed makes the fixture machine by a first /boot, then flags it with the
// fixture entry, set an hour before testNow.
func (g *rig) armed() {
	g.t.Helper()
	g.boot(efiQ)
	g.flag(entryName, testNow().Add(-time.Hour))
}

// stock puts the fixture entry's content in the store and marks its cache row
// ready at the pin, as a finished fetch would.
func (g *rig) stock() {
	g.t.Helper()
	catalogtest.Stock(g.t, g.st, g.fix, catalog.Key{Entry: entryName, Arch: entryArch})
	moved, err := g.repo.RestoreReady(g.t.Context(), shellKey(), g.shell().Pin.ID, testNow())
	if err != nil || !moved {
		g.t.Fatalf("RestoreReady = %v, %v; want the wanted row moved to ready", moved, err)
	}
}

// cacheRow is the fixture entry's cache row, which must exist.
func (g *rig) cacheRow() fleet.CacheRow {
	g.t.Helper()
	row, ok, err := g.repo.CacheRow(g.t.Context(), shellKey())
	if err != nil || !ok {
		g.t.Fatalf("CacheRow = %v, %v; want a row", ok, err)
	}
	return row
}

// cleared counts the boot_next.cleared audit events whose detail holds
// fragment; "" counts them all.
func (g *rig) cleared(fragment string) int {
	g.t.Helper()
	return g.count("SELECT count(*) FROM audit_events WHERE action = ? AND detail LIKE ?", clearedAction, "%"+fragment+"%")
}

// reason is the audit detail fragment for a clear reason.
func reason(r fleet.ClearReason) string { return `"reason":"` + string(r) + `"` }

// wantReportExit fails unless body is the first-contact script of an unknown
// machine whose unknown_action is local: the report, then exit. It is what
// rows 4 to 10 answer a discovered machine.
func wantReportExit(t *testing.T, body string) {
	t.Helper()
	if !strings.HasPrefix(body, "#!ipxe\nparams\n") || !strings.HasSuffix(body, "exit\n") {
		t.Errorf("body is not the report then exit: %q", body)
	}
}

func TestBoot_Row3ServesRecipe(t *testing.T) {
	metrics := newMetrics(t)
	g := newRig(t, WithMeter(metrics.Meter()))
	g.armed()
	g.stock()

	rec, line := g.wide("/boot?" + efiQ)

	wantStatus(t, rec, http.StatusOK)
	if got, want := rec.Body.String(), g.shell().Script(netip.MustParseAddr(testAddress)); got != want {
		t.Errorf("body = %q, want the entry's script %q", got, want)
	}
	if m := g.machine(); m.BootNext != "" || !m.BootNextSetAt.IsZero() {
		t.Errorf("boot-next = %q set at %v, want the flag cleared", m.BootNext, m.BootNextSetAt)
	}
	if n := g.cleared(""); n != 1 {
		t.Errorf("boot_next.cleared events = %d, want 1", n)
	}
	if n := g.cleared(reason(fleet.ClearServed)); n != 1 {
		t.Errorf("served clears = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 3 AND action = 'utility'"); n != 1 {
		t.Errorf("row 3 utility events = %d, want 1", n)
	}
	want := map[string]any{"utility": entryName, "boot_next": entryName, "boot_next_clear": "served", "platform": "efi", "decision_row": float64(3), "action": "utility"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log line %q = %v, want %v (line %v)", k, line[k], v, line)
		}
	}
	body := scrapeMetrics(t, metrics)
	var found bool
	for l := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(l, `hephaestus_utility_boots_total{entry="tshell"`) {
			found = true
			if !strings.HasSuffix(l, " 1") {
				t.Errorf("utility boots line = %q, want a count of 1", l)
			}
		}
	}
	if !found {
		t.Errorf("metrics lack hephaestus_utility_boots_total{entry=\"tshell\"}:\n%s", body)
	}
}

func TestBoot_Row3NotReadyKeepsFlag(t *testing.T) {
	g := newRig(t)
	g.armed()
	// SetBootNext already asked for the row; drop it so /boot has to.
	if err := g.repo.DeleteCacheRow(t.Context(), shellKey()); err != nil {
		t.Fatalf("DeleteCacheRow: %v", err)
	}

	body := g.boot(efiQ)

	wantReportExit(t, body)
	if got := g.machine().BootNext; got != entryName {
		t.Errorf("boot-next = %q, want the flag kept", got)
	}
	if n := g.cleared(""); n != 0 {
		t.Errorf("boot_next.cleared events = %d, want 0", n)
	}
	row := g.cacheRow()
	if row.State != fleet.CacheWanted || row.Priority != 1 || row.Pin != g.shell().Pin.ID {
		t.Errorf("cache row = %q priority %d pin %q, want wanted priority 1 at the pin", row.State, row.Priority, row.Pin)
	}
}

func TestBoot_Row3BrokenNoRefetchLoop(t *testing.T) {
	g := newRig(t)
	g.armed()
	// SetBootNext re-arms a broken row, so break it after flagging.
	g.exec("UPDATE utility_cache SET state = 'broken', attempts = 10 WHERE entry = ? AND arch = ?", entryName, entryArch)
	before := g.cacheRow()

	for range 3 {
		wantReportExit(t, g.boot(efiQ))
	}

	after := g.cacheRow()
	if after.State != fleet.CacheBroken || after.Attempts != 10 || after.Priority != before.Priority || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("cache row = %+v, want it unchanged from %+v", after, before)
	}
	if got := g.machine().BootNext; got != entryName {
		t.Errorf("boot-next = %q, want the flag kept", got)
	}
}

func TestBoot_Row3UnknownPlatformKeepsFlag(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"linux", bootQ + "&platform=linux"},
		{"no platform", bootQ},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.armed()
			g.stock()

			body := g.boot(tc.query)

			wantReportExit(t, body)
			if got := g.machine().BootNext; got != entryName {
				t.Errorf("boot-next = %q, want the flag kept", got)
			}
			if n := g.cleared(""); n != 0 {
				t.Errorf("boot_next.cleared events = %d, want 0", n)
			}
		})
	}
}

func TestBoot_Row3Unsupported(t *testing.T) {
	g := newRig(t)
	g.armed()
	g.stock()

	body := g.boot(bootQ + "&platform=pcbios")

	wantReportExit(t, body)
	if got := g.machine().BootNext; got != "" {
		t.Errorf("boot-next = %q, want it cleared", got)
	}
	if n := g.cleared(reason(fleet.ClearUnsupported)); n != 1 {
		t.Errorf("unsupported clears = %d, want 1", n)
	}
}

func TestBoot_Row3Expired(t *testing.T) {
	g := newRig(t)
	g.boot(efiQ)
	g.flag(entryName, testNow().Add(-25*time.Hour))
	g.stock()

	body := g.boot(efiQ)

	wantReportExit(t, body)
	if got := g.machine().BootNext; got != "" {
		t.Errorf("boot-next = %q, want it cleared", got)
	}
	if n := g.cleared(reason(fleet.ClearExpired)); n != 1 {
		t.Errorf("expired clears = %d, want 1", n)
	}
}

func TestBoot_Row3UnknownEntry(t *testing.T) {
	g := newRig(t)
	g.boot(efiQ)
	g.exec("UPDATE machines SET boot_next = 'gone', boot_next_set_at = ? WHERE smbios_uuid = ?", testNow().Add(-time.Hour), bootUUID)

	body := g.boot(efiQ)

	wantReportExit(t, body)
	if got := g.machine().BootNext; got != "" {
		t.Errorf("boot-next = %q, want it cleared", got)
	}
	if n := g.cleared(reason(fleet.ClearUnknownEntry)); n != 1 {
		t.Errorf("unknown_entry clears = %d, want 1", n)
	}
}

func TestBoot_Row3LayerMissingRequeues(t *testing.T) {
	g := newRig(t)
	g.armed()
	g.stock()
	// The store root is the temp dir testConfig made; the blob is named by its digest.
	file := g.shell().Pin.Objects[0].Files[0]
	blob := filepath.Join(g.storeRoot, "blobs", "sha256", file.SHA256)
	if err := os.Remove(blob); err != nil {
		t.Fatalf("remove a layer blob: %v", err)
	}

	body := g.boot(efiQ)

	wantReportExit(t, body)
	if got := g.machine().BootNext; got != entryName {
		t.Errorf("boot-next = %q, want the flag kept", got)
	}
	if row := g.cacheRow(); row.State != fleet.CacheWanted {
		t.Errorf("cache row = %q, want wanted again", row.State)
	}
	if len(g.errs) != 1 || !errors.Is(g.errs[0], ErrUtilityMissing) {
		t.Errorf("reported errors = %v, want one ErrUtilityMissing", g.errs)
	}
}

// rearmOnClock returns a clock that, on each of its first calls, sets the
// fixture machine's boot-next again through a second repository, one set-at per
// call, and reads testNow from then on. It reads the clock once per /boot
// attempt, so each call is one more decision of the request under test.
func (g *rig) rearmOnClock(sets ...time.Time) func() time.Time {
	other := fleet.NewRepository(g.d)
	return func() time.Time {
		if len(sets) > 0 {
			at := sets[0]
			sets = sets[1:]
			g.flagVia(other, entryName, at)
		}
		return testNow()
	}
}

func TestBoot_Row3CLIChangeRedecides(t *testing.T) {
	clock := testNow
	g := newRig(t, WithClock(func() time.Time { return clock() }))
	g.armed()
	g.stock()
	set := testNow().Add(time.Second)
	clock = g.rearmOnClock(set)

	body := g.boot(efiQ)

	if got, want := body, g.shell().Script(netip.MustParseAddr(testAddress)); got != want {
		t.Errorf("body = %q, want the entry's script %q", got, want)
	}
	if n := g.cleared(""); n != 1 {
		t.Errorf("boot_next.cleared events = %d, want 1 (served once)", n)
	}
	if n := g.cleared(`"set_at":"` + set.Format(time.RFC3339Nano) + `"`); n != 1 {
		t.Errorf("clears of the flag set at %v = %d, want 1", set, n)
	}
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
}

func TestBoot_Row3CLIChangeTwiceFallsThrough(t *testing.T) {
	clock := testNow
	g := newRig(t, WithClock(func() time.Time { return clock() }))
	g.armed()
	g.stock()
	first, second := testNow().Add(time.Second), testNow().Add(2*time.Second)
	clock = g.rearmOnClock(first, second)

	body := g.boot(efiQ)

	wantReportExit(t, body)
	if n := g.cleared(""); n != 0 {
		t.Errorf("boot_next.cleared events = %d, want 0", n)
	}
	m := g.machine()
	if m.BootNext != entryName || !m.BootNextSetAt.Equal(second) {
		t.Errorf("boot-next = %q set at %v, want %q set at %v", m.BootNext, m.BootNextSetAt, entryName, second)
	}
	if len(g.errs) != 1 || !errors.Is(g.errs[0], ErrBootNextRace) {
		t.Errorf("reported errors = %v, want one ErrBootNextRace", g.errs)
	}
}
