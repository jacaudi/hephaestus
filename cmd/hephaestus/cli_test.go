package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

// cliT0 is the instant the CLI believes it is, unless a test moves cliTest.now.
var cliT0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// cliTest is a configured CLI: the HEPHAESTUS_ environment, a migrated SQLite
// database in the data dir, the fixture catalog and a repository on the same file.
type cliTest struct {
	dir  string
	d    *db.DB
	repo *fleet.Repository
	// clusters is the clusters repository on the same file, sealing with the data key the CLI loads.
	clusters *clusters.Repository
	deps     cliDeps
	// now is what the CLI's clock reads; onNow, when set, runs on each read.
	now   time.Time
	onNow func()
}

// cliEnv sets the environment of a CLI over a SQLite file in a fresh data dir,
// returning the dir and the file's URL. The file does not exist yet.
func cliEnv(t *testing.T) (dir string, url secret.Secret) {
	t.Helper()
	dir = setMinimalEnv(t)
	url = secret.Secret("sqlite:" + filepath.Join(dir, "hephaestus.db"))
	t.Setenv("HEPHAESTUS_DATABASE_URL", url.Expose())
	return dir, url
}

func newCLITest(t *testing.T) *cliTest {
	t.Helper()
	dir, url := cliEnv(t)
	d, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	key, err := datakey.Parse(secret.Secret(testDataKey))
	if err != nil {
		t.Fatalf("parse the fixture data key: %v", err)
	}
	c := &cliTest{dir: dir, d: d, repo: fleet.NewRepository(d), clusters: clusters.NewRepository(d, key), now: cliT0}
	cat := testCatalog(t)
	c.deps = cliDeps{
		addrsOf: testInterfaces,
		now: func() time.Time {
			if c.onNow != nil {
				c.onNow()
			}
			return c.now
		},
		catalog: func() (*catalog.Catalog, error) { return cat, nil },
	}
	return c
}

// useCatalog makes the CLI load cat instead of the fixture catalog.
func (c *cliTest) useCatalog(cat *catalog.Catalog) {
	c.deps.catalog = func() (*catalog.Catalog, error) { return cat, nil }
}

// run executes the root command with args and returns its stdout and error.
func (c *cliTest) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, _, err := c.runStreams(t, args...)
	return out, err
}

// runStreams executes the root command with args and returns its stdout, its stderr and its error.
func (c *cliTest) runStreams(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmdWith(c.deps)
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// mustRun is run for a command that must succeed.
func (c *cliTest) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.run(t, args...)
	if err != nil {
		t.Fatalf("hephaestus %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// record makes the machine of id by its first contact, arriving as arch.
func (c *cliTest) record(t *testing.T, id fleet.Identity, arch fleet.Arch) fleet.Machine {
	t.Helper()
	m, _, err := c.repo.RecordBoot(t.Context(), fleet.BootRecord{
		Identity: id, Arch: arch, Trigger: fleet.TriggerFirstContact,
		Event: fleet.BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: cliT0,
	})
	if err != nil {
		t.Fatalf("first contact %+v: %v", id, err)
	}
	return m
}

func cliMAC(n int) string { return fmt.Sprintf("aa:bb:cc:00:00:%02x", n) }

func cliUUID(n int) string { return fmt.Sprintf("4c4c4544-0042-3510-8052-b4c04f4e4d%02x", n) }

// machine makes the n-th fixture machine: its own UUID and the MAC cliMAC(n).
func (c *cliTest) machine(t *testing.T, n int, arch fleet.Arch) fleet.Machine {
	t.Helper()
	return c.record(t, fleet.Identity{UUID: cliUUID(n), MAC: cliMAC(n), Serial: "S", Product: "P"}, arch)
}

// pin is the current pin id of the fixture catalog's entry for arch.
func (c *cliTest) pin(t *testing.T, entry string, arch fleet.Arch) string {
	t.Helper()
	cat, err := c.deps.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	e, ok := cat.Lookup(entry, string(arch))
	if !ok {
		t.Fatalf("catalog has no %s/%s", entry, arch)
	}
	return e.Pin.ID
}

// arm sets m's boot-next to entry at the given time through the repository,
// at the fixture pin, as the CLI would.
func (c *cliTest) arm(t *testing.T, m fleet.Machine, entry string, at time.Time) {
	t.Helper()
	pin := "unpinned"
	if e, ok := testCatalog(t).Lookup(entry, string(m.Arch)); ok {
		pin = e.Pin.ID
	}
	if _, err := c.repo.SetBootNext(t.Context(), fleet.SetBootNextRequest{MachineID: m.ID, Entry: entry, Pin: pin, At: at}); err != nil {
		t.Fatalf("SetBootNext(%s, %s): %v", m.ID, entry, err)
	}
}

// row is the cache row of entry for arch.
func (c *cliTest) row(t *testing.T, entry string, arch fleet.Arch) (fleet.CacheRow, bool) {
	t.Helper()
	row, ok, err := c.repo.CacheRow(t.Context(), fleet.CacheKey{Entry: entry, Arch: arch})
	if err != nil {
		t.Fatalf("CacheRow(%s/%s): %v", entry, arch, err)
	}
	return row, ok
}

// restoreReady makes the row of entry for arch ready, as a finished fetch would.
func (c *cliTest) restoreReady(t *testing.T, entry string, arch fleet.Arch) {
	t.Helper()
	moved, err := c.repo.RestoreReady(t.Context(), fleet.CacheKey{Entry: entry, Arch: arch}, c.pin(t, entry, arch), cliT0)
	if err != nil || !moved {
		t.Fatalf("RestoreReady(%s/%s) = %v, %v; want it moved", entry, arch, moved, err)
	}
}

// breakRow makes the row of entry for arch broken by an integrity failure with
// the given cause; it must be the only row, so that ClaimFetch claims it.
func (c *cliTest) breakRow(t *testing.T, entry string, arch fleet.Arch, cause string) {
	t.Helper()
	key, pin := fleet.CacheKey{Entry: entry, Arch: arch}, c.pin(t, entry, arch)
	if _, err := c.repo.RequestFetch(t.Context(), key, pin, 0, cliT0); err != nil {
		t.Fatalf("RequestFetch: %v", err)
	}
	if claimed, ok, err := c.repo.ClaimFetch(t.Context(), cliT0); err != nil || !ok || claimed.Key() != key {
		t.Fatalf("ClaimFetch = %+v, %v, %v; want the %v row", claimed, ok, err, key)
	}
	f := fleet.FetchFailure{Key: key, Pin: pin, Cause: cause, Integrity: true, At: cliT0}
	if row, err := c.repo.RecordFetchFailure(t.Context(), f); err != nil || row.State != fleet.CacheBroken {
		t.Fatalf("RecordFetchFailure = %+v, %v; want a broken row", row, err)
	}
}

// derive returns a catalog of the fixture's entries: edit may change each
// entry's spec, and drops the entry by returning false.
func derive(t *testing.T, base *catalog.Catalog, edit func(*catalog.Spec) bool) *catalog.Catalog {
	t.Helper()
	var specs []catalog.Spec
	pins := map[catalog.Key]catalog.Pin{}
	entries := base.Entries()
	for i := range entries {
		if spec := entries[i].Spec; edit(&spec) {
			specs = append(specs, spec)
			pins[spec.Key()] = entries[i].Pin
		}
	}
	cat, err := catalog.New(specs, pins)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return cat
}

// without is the catalog minus the one entry for arch.
func without(t *testing.T, base *catalog.Catalog, entry, arch string) *catalog.Catalog {
	t.Helper()
	return derive(t, base, func(s *catalog.Spec) bool { return s.Key() != catalog.Key{Entry: entry, Arch: arch} })
}

// jsonObject decodes out as one JSON object.
func jsonObject(t *testing.T, out string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	return v
}

// jsonRows decodes out as a JSON array of objects.
func jsonRows(t *testing.T, out string) []map[string]any {
	t.Helper()
	var v []map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output is not a JSON array of objects: %v\n%s", err, out)
	}
	return v
}

// wantKeys fails unless obj has exactly the keys, the shape the design pins.
func wantKeys(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(obj))
	slices.Sort(keys)
	if !slices.Equal(got, keys) {
		t.Errorf("JSON keys = %v, want %v", got, keys)
	}
}

// storeAbsent fails when the CLI left a content store or a git store in the data dir.
func (c *cliTest) storeAbsent(t *testing.T) {
	t.Helper()
	for _, name := range []string{"store", "git"} {
		if _, err := os.Stat(filepath.Join(c.dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat <data>/%s = %v, want it not to exist: the CLI never opens the content store or the git store", name, err)
		}
	}
}

// audit returns the actor and detail of the only audit event of action.
func (c *cliTest) audit(t *testing.T, action string) (actor, detail string) {
	t.Helper()
	q := "SELECT actor, detail FROM audit_events WHERE action = ?"
	if err := c.d.Bun().NewRaw(q, action).Scan(t.Context(), &actor, &detail); err != nil {
		t.Fatalf("audit event %s: %v", action, err)
	}
	return actor, detail
}

func TestBootNextSet_JSON(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json")

	got := jsonObject(t, out)
	want := map[string]any{
		"machine_id": m.ID, "macs": []any{cliMAC(1)}, "entry": "tshell", "arch": "amd64",
		"set_at": "2026-11-14T12:00:00Z", "expires_at": "2026-11-15T12:00:00Z",
		"state": "wanted", "previous_error": "", "warning": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}

	stored, err := c.repo.Get(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.BootNext != "tshell" || !stored.BootNextSetAt.Equal(cliT0) {
		t.Errorf("boot-next = %q set %v, want tshell set %v", stored.BootNext, stored.BootNextSetAt, cliT0)
	}
	row, ok := c.row(t, "tshell", fleet.ArchAMD64)
	if !ok || row.Priority != 1 || row.Pin != c.pin(t, "tshell", fleet.ArchAMD64) {
		t.Errorf("cache row = %+v, %v; want one at priority 1 and the catalog pin", row, ok)
	}
}

func TestBootNextSet_Text(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")

	for _, want := range []string{
		"machine " + m.ID + " (" + cliMAC(1) + ")\n",
		"entry: tshell\n", "arch: amd64\n",
		"expires: 2026-11-15T12:00:00Z\n", "utility: wanted\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "previous error") || strings.Contains(out, "WARNING") {
		t.Errorf("output mentions a previous error or a warning with neither present:\n%s", out)
	}
}

func TestBootNextSet_ExpiryIsTheDecisionTTL(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json")

	want := cliT0.Add(boot.BootNextTTL).Format(time.RFC3339)
	if got := jsonObject(t, out)["expires_at"]; got != want {
		t.Errorf("expires_at = %v, want set_at + boot.BootNextTTL = %s", got, want)
	}
}

func TestBootNextSet_ByUUID(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "boot-next", "set", strings.ToUpper(cliUUID(1)), "tlive", "--json")

	if got := jsonObject(t, out)["machine_id"]; got != m.ID {
		t.Errorf("machine_id = %v, want %s", got, m.ID)
	}
	if stored, err := c.repo.Get(t.Context(), m.ID); err != nil || stored.BootNext != "tlive" {
		t.Errorf("boot-next = %q, %v; want tlive", stored.BootNext, err)
	}
}

// junkMAC is the MAC of the junk-UUID machines these tests make.
const junkMAC = "52:54:00:e2:e0:0a"

func TestBootNextSet_JunkMachineByUUIDIsRefused(t *testing.T) {
	for _, uuid := range []string{zeroUUID, "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		t.Run(uuid, func(t *testing.T) {
			c := newCLITest(t)
			m := c.record(t, fleet.Identity{UUID: uuid, MAC: junkMAC, Serial: "S", Product: "P"}, fleet.ArchAMD64)

			_, err := c.run(t, "boot-next", "set", uuid, "tshell")

			if err == nil {
				t.Fatal("boot-next set by a junk UUID succeeded, want a refusal")
			}
			if got := exitCode(err); got != 1 {
				t.Errorf("exit code = %d, want 1 (%v)", got, err)
			}
			if want := m.ID + " is identified only by its MAC (junk or shared UUID); name it by the MAC it boots from"; err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if armed, err := c.repo.ListBootNext(t.Context()); err != nil || len(armed) != 0 {
				t.Errorf("ListBootNext = %v, %v; a refused set arms nothing", armed, err)
			}
		})
	}
}

func TestBootNextSet_JunkMachineByMACBindsIt(t *testing.T) {
	c := newCLITest(t)
	m := c.record(t, fleet.Identity{UUID: zeroUUID, MAC: junkMAC, Serial: "S", Product: "P"}, fleet.ArchAMD64)

	got := jsonObject(t, c.mustRun(t, "boot-next", "set", "52-54-00-E2-E0-0A", "tshell", "--json"))

	if got["bound_mac"] != junkMAC {
		t.Errorf("bound_mac = %v, want the normalized %s", got["bound_mac"], junkMAC)
	}
	if stored, err := c.repo.Get(t.Context(), m.ID); err != nil || stored.BootNext != "tshell" || stored.BootNextMAC != junkMAC {
		t.Errorf("stored boot-next = %q bound to %q, %v; want tshell bound to %s", stored.BootNext, stored.BootNextMAC, err, junkMAC)
	}

	out := c.mustRun(t, "boot-next", "set", junkMAC, "tshell")
	if want := "bound MAC: " + junkMAC + "\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestBootNextSet_NonJunkMachineBindsNothing(t *testing.T) {
	for _, ref := range []string{cliMAC(1), cliUUID(1)} {
		t.Run(ref, func(t *testing.T) {
			c := newCLITest(t)
			m := c.machine(t, 1, fleet.ArchAMD64)

			got := jsonObject(t, c.mustRun(t, "boot-next", "set", ref, "tshell", "--json"))

			if _, ok := got["bound_mac"]; ok {
				t.Errorf("output has bound_mac = %v, want the key omitted", got["bound_mac"])
			}
			if stored, err := c.repo.Get(t.Context(), m.ID); err != nil || stored.BootNextMAC != "" {
				t.Errorf("stored bound MAC = %q, %v; want none", stored.BootNextMAC, err)
			}
			if out := c.mustRun(t, "boot-next", "set", ref, "tshell"); strings.Contains(out, "bound MAC") {
				t.Errorf("output mentions a bound MAC with none:\n%s", out)
			}
		})
	}
}

func TestBootNextSet_Refusals(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchAMD64)
	c.machine(t, 2, fleet.ArchARM64)
	c.record(t, fleet.Identity{UUID: zeroUUID, MAC: cliMAC(0xa)}, fleet.ArchAMD64)
	c.record(t, fleet.Identity{UUID: zeroUUID, MAC: cliMAC(0xb)}, fleet.ArchAMD64)

	tests := []struct {
		name     string
		cat      *catalog.Catalog
		args     []string
		wantCode int
		wantErr  string
	}{
		{
			name: "unknown MAC", args: []string{"boot-next", "set", "aa:bb:cc:99:99:99", "tshell"},
			wantCode: 1, wantErr: "no machine matches aa:bb:cc:99:99:99; it must have booted through hephaestus once",
		},
		{
			name: "ambiguous UUID", args: []string{"boot-next", "set", zeroUUID, "tshell"},
			wantCode: 1, wantErr: zeroUUID + " matches 2 machines; name one by MAC: aa:bb:cc:00:00:0a, aa:bb:cc:00:00:0b",
		},
		{
			name: "unknown entry", args: []string{"boot-next", "set", cliMAC(1), "nope"},
			wantCode: 1, wantErr: "unknown utility nope; known: tlive, tshell",
		},
		{
			name: "no build for the arch", cat: without(t, testCatalog(t), "tshell", "arm64"),
			args:     []string{"boot-next", "set", cliMAC(2), "tshell"},
			wantCode: 1, wantErr: "tshell has no arm64 build; it supports amd64",
		},
		{name: "one argument", args: []string{"boot-next", "set", cliMAC(1)}, wantCode: 2},
		{name: "three arguments", args: []string{"boot-next", "set", cliMAC(1), "tshell", "extra"}, wantCode: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.cat != nil {
				c.useCatalog(tt.cat)
				t.Cleanup(func() { c.useCatalog(testCatalog(t)) })
			}
			_, err := c.run(t, tt.args...)
			if err == nil {
				t.Fatalf("hephaestus %s succeeded, want an error", strings.Join(tt.args, " "))
			}
			if got := exitCode(err); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (%v)", got, tt.wantCode, err)
			}
			if tt.wantErr != "" && err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err, tt.wantErr)
			}
			if armed, err := c.repo.ListBootNext(t.Context()); err != nil || len(armed) != 0 {
				t.Errorf("ListBootNext = %v, %v; a refused set arms nothing", armed, err)
			}
		})
	}
}

func TestBootNextSet_RearmPrintsPreviousError(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchAMD64)
	c.breakRow(t, "tshell", fleet.ArchAMD64, "sha256 mismatch")

	out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")
	if !strings.Contains(out, "previous error: sha256 mismatch\n") || !strings.Contains(out, "utility: wanted\n") {
		t.Errorf("output lacks the previous error or the re-armed state:\n%s", out)
	}

	// The error was cleared by the first set; a second one has none to report.
	again := jsonObject(t, c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json"))
	if again["previous_error"] != "" {
		t.Errorf("previous_error after a second set = %v, want none", again["previous_error"])
	}
}

func TestBootNextSet_Warning(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)
	c.useCatalog(derive(t, testCatalog(t), func(s *catalog.Spec) bool {
		if s.Entry == "tshell" {
			s.Warning = "this erases disks"
		}
		return true
	}))

	out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")
	want := "WARNING: machine " + m.ID + " (" + cliMAC(1) + "): this erases disks\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("output does not start with %q:\n%s", want, out)
	}

	// JSON carries the warning as a field, and nothing else precedes the object.
	if got := jsonObject(t, c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json"))["warning"]; got != "this erases disks" {
		t.Errorf("warning = %v, want %q", got, "this erases disks")
	}
}

func TestBootNextClear(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)
	c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")

	got := jsonObject(t, c.mustRun(t, "boot-next", "clear", cliMAC(1), "--json"))
	want := map[string]any{"machine_id": m.ID, "entry": "tshell", "cleared": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
	if stored, err := c.repo.Get(t.Context(), m.ID); err != nil || stored.BootNext != "" {
		t.Errorf("boot-next after clear = %q, %v; want none", stored.BootNext, err)
	}
	if actor, detail := c.audit(t, "boot_next.cleared"); actor != "cli" || !strings.Contains(detail, `"reason":"cli"`) {
		t.Errorf("audit = actor %q detail %q; want actor cli and reason cli", actor, detail)
	}

	out := c.mustRun(t, "boot-next", "clear", cliMAC(1))
	if want := "no boot-next set for " + m.ID + "\n"; out != want {
		t.Errorf("second clear printed %q, want %q", out, want)
	}
}

func TestBootNextClear_ChangedMeanwhile(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)
	c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")

	// The CLI reads its clock after it has read the flag and before it clears
	// it; setting the flag again in that window is the race a compare-and-clear
	// exists for.
	c.onNow = func() {
		c.onNow = nil
		c.arm(t, m, "tshell", cliT0.Add(time.Minute))
	}
	out := c.mustRun(t, "boot-next", "clear", cliMAC(1))

	if want := "boot-next changed meanwhile; nothing cleared\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if stored, err := c.repo.Get(t.Context(), m.ID); err != nil || stored.BootNext != "tshell" {
		t.Errorf("boot-next = %q, %v; the newer flag must survive", stored.BootNext, err)
	}
}

func TestBootNextClear_JSONWithNothingSet(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	got := jsonObject(t, c.mustRun(t, "boot-next", "clear", cliMAC(1), "--json"))
	want := map[string]any{"machine_id": m.ID, "entry": "", "cleared": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

func TestBootNextClear_Refusals(t *testing.T) {
	c := newCLITest(t)
	for _, tt := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"unknown machine", []string{"boot-next", "clear", cliMAC(9)}, 1},
		{"no argument", []string{"boot-next", "clear"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.run(t, tt.args...)
			if err == nil || exitCode(err) != tt.wantCode {
				t.Errorf("error = %v, exit code %d; want an error with exit code %d", err, exitCode(err), tt.wantCode)
			}
		})
	}
}

func TestBootNextList_Statuses(t *testing.T) {
	c := newCLITest(t)
	// tshell has no arm64 build here, so an arm64 machine armed with it is unsupported.
	c.useCatalog(without(t, testCatalog(t), "tshell", "arm64"))

	cases := []struct {
		name   string
		arch   fleet.Arch
		entry  string
		setAgo time.Duration
		prep   func(t *testing.T, m fleet.Machine)
		want   string
	}{
		{name: "not requested", arch: fleet.ArchAMD64, entry: "tshell", setAgo: time.Hour, want: "not_requested",
			prep: func(t *testing.T, _ fleet.Machine) {
				t.Helper()
				if err := c.repo.DeleteCacheRow(t.Context(), fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64}); err != nil {
					t.Fatalf("DeleteCacheRow: %v", err)
				}
			}},
		{name: "ready", arch: fleet.ArchAMD64, entry: "tlive", setAgo: time.Hour, want: "ready",
			prep: func(t *testing.T, _ fleet.Machine) { t.Helper(); c.restoreReady(t, "tlive", fleet.ArchAMD64) }},
		{name: "wanted", arch: fleet.ArchARM64, entry: "tlive", setAgo: time.Minute, want: "wanted"},
		{name: "exactly 24 h is not expired", arch: fleet.ArchAMD64, entry: "tlive", setAgo: boot.BootNextTTL, want: "ready"},
		{name: "expired", arch: fleet.ArchAMD64, entry: "tlive", setAgo: boot.BootNextTTL + time.Second, want: "expired"},
		{name: "expired beats unknown entry", arch: fleet.ArchAMD64, entry: "gone", setAgo: 25 * time.Hour, want: "expired"},
		{name: "unknown entry", arch: fleet.ArchAMD64, entry: "gone", setAgo: time.Hour, want: "unknown_entry"},
		{name: "unsupported", arch: fleet.ArchARM64, entry: "tshell", setAgo: time.Hour, want: "unsupported"},
	}
	byID := map[string]int{} // machine ID to case index
	for i, tc := range cases {
		m := c.record(t, fleet.Identity{UUID: cliUUID(i + 1), MAC: cliMAC(i + 1)}, tc.arch)
		c.arm(t, m, tc.entry, cliT0.Add(-tc.setAgo))
		if tc.prep != nil {
			tc.prep(t, m)
		}
		byID[m.ID] = i
	}

	rows := jsonRows(t, c.mustRun(t, "boot-next", "list", "--json"))

	if len(rows) != len(cases) {
		t.Fatalf("got %d rows, want %d: %v", len(rows), len(cases), rows)
	}
	wantKeys(t, rows[0], "machine_id", "macs", "entry", "arch", "set_at", "age_seconds", "status")
	for _, row := range rows {
		id, _ := row["machine_id"].(string)
		i, ok := byID[id]
		if !ok {
			t.Errorf("row for unknown machine %v", row)
			continue
		}
		tc := cases[i]
		if row["status"] != tc.want || row["entry"] != tc.entry || row["arch"] != string(tc.arch) {
			t.Errorf("%s: row = %v, want status %s entry %s arch %s", tc.name, row, tc.want, tc.entry, tc.arch)
		}
		if got, want := row["age_seconds"], float64(tc.setAgo/time.Second); got != want {
			t.Errorf("%s: age_seconds = %v, want %v", tc.name, got, want)
		}
		if got, want := row["set_at"], cliT0.Add(-tc.setAgo).Format(time.RFC3339); got != want {
			t.Errorf("%s: set_at = %v, want %v", tc.name, got, want)
		}
	}
}

func TestBootNextList_RowAtAnotherPinIsNotRequested(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)
	c.arm(t, m, "tshell", cliT0)
	key := fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64}
	if moved, err := c.repo.Repin(t.Context(), key, c.pin(t, "tshell", fleet.ArchAMD64), "oldpin", cliT0); err != nil || !moved {
		t.Fatalf("Repin = %v, %v; want the row moved to oldpin", moved, err)
	}

	rows := jsonRows(t, c.mustRun(t, "boot-next", "list", "--json"))

	if len(rows) != 1 || rows[0]["status"] != "not_requested" {
		t.Errorf("rows = %v, want one not_requested: the stored row is not for the catalog's pin", rows)
	}
}

func TestBootNextList_Text(t *testing.T) {
	c := newCLITest(t)

	out := c.mustRun(t, "boot-next", "list")
	if !strings.Contains(out, "no boot-next set") {
		t.Errorf("empty list printed %q, want a note that nothing is set", out)
	}
	if empty := jsonRows(t, c.mustRun(t, "boot-next", "list", "--json")); empty == nil || len(empty) != 0 {
		t.Errorf("empty JSON list = %#v, want []", empty)
	}

	m := c.machine(t, 1, fleet.ArchAMD64)
	c.arm(t, m, "tshell", cliT0.Add(-2*time.Hour))
	out = c.mustRun(t, "boot-next", "list")
	for _, want := range []string{"MACHINE", "STATUS", m.ID, cliMAC(1), "tshell", "amd64", "7200", "wanted"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
}

func TestBootNextList_ShowsBoundMAC(t *testing.T) {
	c := newCLITest(t)
	junk := c.record(t, fleet.Identity{UUID: zeroUUID, MAC: junkMAC, Serial: "S", Product: "P"}, fleet.ArchAMD64)
	plain := c.machine(t, 1, fleet.ArchAMD64)
	c.mustRun(t, "boot-next", "set", junkMAC, "tshell")
	c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell")

	rows := jsonRows(t, c.mustRun(t, "boot-next", "list", "--json"))

	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(rows), rows)
	}
	for _, row := range rows {
		switch row["machine_id"] {
		case junk.ID:
			wantKeys(t, row, "machine_id", "macs", "entry", "arch", "set_at", "age_seconds", "status", "bound_mac")
			if row["bound_mac"] != junkMAC {
				t.Errorf("junk machine bound_mac = %v, want %s", row["bound_mac"], junkMAC)
			}
		case plain.ID:
			wantKeys(t, row, "machine_id", "macs", "entry", "arch", "set_at", "age_seconds", "status")
		default:
			t.Errorf("row for unknown machine %v", row)
		}
	}
	out := c.mustRun(t, "boot-next", "list")
	for _, want := range []string{"BOUND_MAC", junkMAC} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
}

func TestUtilityFetch(t *testing.T) {
	c := newCLITest(t)

	rows := jsonRows(t, c.mustRun(t, "utility", "fetch", "tlive", "--json"))
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want amd64 and arm64: %v", len(rows), rows)
	}
	wantKeys(t, rows[0], "entry", "arch", "pin", "state", "previous_error")
	for i, arch := range []fleet.Arch{fleet.ArchAMD64, fleet.ArchARM64} {
		want := map[string]any{"entry": "tlive", "arch": string(arch), "pin": c.pin(t, "tlive", arch), "state": "wanted", "previous_error": ""}
		if !reflect.DeepEqual(rows[i], want) {
			t.Errorf("row %d = %v, want %v", i, rows[i], want)
		}
		if row, ok := c.row(t, "tlive", arch); !ok || row.Priority != 0 {
			t.Errorf("%s row = %+v, %v; want one at priority 0", arch, row, ok)
		}
	}

	t.Run("one arch", func(t *testing.T) {
		c := newCLITest(t)
		rows := jsonRows(t, c.mustRun(t, "utility", "fetch", "tshell", "--arch", "arm64", "--json"))
		if len(rows) != 1 || rows[0]["arch"] != "arm64" {
			t.Errorf("rows = %v, want the arm64 row only", rows)
		}
		if _, ok := c.row(t, "tshell", fleet.ArchAMD64); ok {
			t.Error("an amd64 row exists after fetching arm64 only")
		}
	})

	t.Run("text", func(t *testing.T) {
		c := newCLITest(t)
		c.breakRow(t, "tlive", fleet.ArchAMD64, "sha256 mismatch")
		out := c.mustRun(t, "utility", "fetch", "tlive", "--arch", "amd64")
		for _, want := range []string{"tlive/amd64: wanted\n", "previous error: sha256 mismatch\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("a row at another pin is reported stale", func(t *testing.T) {
		c := newCLITest(t)
		key := fleet.CacheKey{Entry: "tlive", Arch: fleet.ArchAMD64}
		if _, err := c.repo.RequestFetch(t.Context(), key, "oldpin", 0, cliT0); err != nil {
			t.Fatalf("RequestFetch: %v", err)
		}
		rows := jsonRows(t, c.mustRun(t, "utility", "fetch", "tlive", "--arch", "amd64", "--json"))
		if len(rows) != 1 || rows[0]["state"] != "stale_pin" {
			t.Errorf("rows = %v, want state stale_pin: the request left the old-pin row alone", rows)
		}
	})
}

func TestUtilityFetch_Refusals(t *testing.T) {
	c := newCLITest(t)
	c.useCatalog(without(t, testCatalog(t), "tshell", "arm64"))

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"unknown entry", []string{"utility", "fetch", "nope"}, 1, "unknown utility nope; known: tlive, tshell"},
		{"unknown arch", []string{"utility", "fetch", "tlive", "--arch", "ppc64"}, 1, "tlive has no ppc64 build; it supports amd64, arm64"},
		{"arch the entry lacks", []string{"utility", "fetch", "tshell", "--arch", "arm64"}, 1, "tshell has no arm64 build; it supports amd64"},
		{"no entry", []string{"utility", "fetch"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.run(t, tt.args...)
			if err == nil {
				t.Fatalf("hephaestus %s succeeded, want an error", strings.Join(tt.args, " "))
			}
			if got := exitCode(err); got != tt.wantCode || (tt.wantErr != "" && err.Error() != tt.wantErr) {
				t.Errorf("error = %q (exit %d), want %q (exit %d)", err, got, tt.wantErr, tt.wantCode)
			}
		})
	}
	if rows, err := c.repo.CacheRows(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("CacheRows = %v, %v; a refused fetch requests nothing", rows, err)
	}
}

func TestUtilityList_JSON(t *testing.T) {
	c := newCLITest(t)
	cat := testCatalog(t)

	rows := jsonRows(t, c.mustRun(t, "utility", "list", "--json"))

	if len(rows) != 4 {
		t.Fatalf("got %d rows, want tlive and tshell for two arches: %v", len(rows), rows)
	}
	wantKeys(t, rows[0], "entry", "arch", "version", "platforms", "pin", "state", "bytes_done", "bytes_total", "last_error", "updated_at", "tofu", "min_ram_bytes")
	for i, e := range cat.Entries() {
		var platforms []any
		for _, p := range e.Platforms {
			platforms = append(platforms, string(p))
		}
		want := map[string]any{
			"entry": e.Entry, "arch": e.Arch, "version": e.Version, "platforms": platforms, "pin": e.Pin.ID,
			"state": "not_requested", "bytes_done": float64(0), "bytes_total": float64(0), "last_error": "", "updated_at": "",
			"tofu": true, "min_ram_bytes": float64(e.MinRAMBytes()),
		}
		if !reflect.DeepEqual(rows[i], want) {
			t.Errorf("row %d = %v, want %v", i, rows[i], want)
		}
	}

	t.Run("a ready row", func(t *testing.T) {
		c := newCLITest(t)
		c.mustRun(t, "utility", "fetch", "tlive", "--arch", "amd64")
		c.restoreReady(t, "tlive", fleet.ArchAMD64)
		row := rowFor(t, jsonRows(t, c.mustRun(t, "utility", "list", "--json")), "tlive", "amd64")
		if row["state"] != "ready" || row["updated_at"] != "2026-11-14T12:00:00Z" {
			t.Errorf("row = %v, want ready, updated at %s", row, cliT0.Format(time.RFC3339))
		}
		if other := rowFor(t, jsonRows(t, c.mustRun(t, "utility", "list", "--json")), "tlive", "arm64"); other["state"] != "not_requested" {
			t.Errorf("tlive/arm64 = %v, want not_requested", other["state"])
		}
	})

	t.Run("a row at another pin", func(t *testing.T) {
		c := newCLITest(t)
		key := fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64}
		if _, err := c.repo.RequestFetch(t.Context(), key, "oldpin", 0, cliT0); err != nil {
			t.Fatalf("RequestFetch: %v", err)
		}
		if row := rowFor(t, jsonRows(t, c.mustRun(t, "utility", "list", "--json")), "tshell", "amd64"); row["state"] != "stale_pin" {
			t.Errorf("state = %v, want stale_pin", row["state"])
		}
	})

	t.Run("a broken row", func(t *testing.T) {
		c := newCLITest(t)
		c.breakRow(t, "tshell", fleet.ArchAMD64, "sha256 mismatch")
		row := rowFor(t, jsonRows(t, c.mustRun(t, "utility", "list", "--json")), "tshell", "amd64")
		if row["state"] != "broken" || row["last_error"] != "sha256 mismatch" {
			t.Errorf("row = %v, want broken with last_error %q", row, "sha256 mismatch")
		}
	})

	t.Run("progress", func(t *testing.T) {
		c := newCLITest(t)
		key, pin := fleet.CacheKey{Entry: "tlive", Arch: fleet.ArchAMD64}, c.pin(t, "tlive", fleet.ArchAMD64)
		c.mustRun(t, "utility", "fetch", "tlive", "--arch", "amd64")
		if _, ok, err := c.repo.ClaimFetch(t.Context(), cliT0); err != nil || !ok {
			t.Fatalf("ClaimFetch = %v, %v", ok, err)
		}
		if err := c.repo.RecordProgress(t.Context(), fleet.FetchProgress{Key: key, Pin: pin, Done: 1 << 20, Total: 4 << 20}); err != nil {
			t.Fatalf("RecordProgress: %v", err)
		}
		row := rowFor(t, jsonRows(t, c.mustRun(t, "utility", "list", "--json")), "tlive", "amd64")
		if row["state"] != "fetching" || row["bytes_done"] != float64(1<<20) || row["bytes_total"] != float64(4<<20) {
			t.Errorf("row = %v, want fetching at 1 MiB of 4 MiB", row)
		}
	})
}

func TestUtilityList_Text(t *testing.T) {
	c := newCLITest(t)
	c.mustRun(t, "utility", "fetch", "tlive", "--arch", "amd64")
	c.restoreReady(t, "tlive", fleet.ArchAMD64)
	e, _ := testCatalog(t).Lookup("tlive", "amd64")

	out := c.mustRun(t, "utility", "list")

	for _, want := range []string{
		"ENTRY", "TOFU", "MIN_RAM_MIB", "tlive", "tshell", "efi,pcbios",
		"not_requested", "ready", strconv.FormatInt(e.MinRAMBytes()>>20, 10),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
}

// rowFor finds the utility list row of entry and arch.
func rowFor(t *testing.T, rows []map[string]any, entry, arch string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["entry"] == entry && row["arch"] == arch {
			return row
		}
	}
	t.Fatalf("no %s/%s row in %v", entry, arch, rows)
	return nil
}

func TestCLI_RefusesPendingMigrations(t *testing.T) {
	_, url := cliEnv(t)
	cat := testCatalog(t)
	c := &cliTest{deps: cliDeps{
		addrsOf: testInterfaces,
		now:     func() time.Time { return cliT0 },
		catalog: func() (*catalog.Catalog, error) { return cat, nil },
	}}

	for _, args := range [][]string{
		{"boot-next", "list"}, {"boot-next", "set", cliMAC(1), "tshell"}, {"boot-next", "clear", cliMAC(1)},
		{"utility", "list"}, {"utility", "fetch", "tlive"},
	} {
		_, err := c.run(t, args...)
		if err == nil || exitCode(err) != 1 || !strings.Contains(err.Error(), `start "hephaestus serve" once to apply them`) {
			t.Errorf("hephaestus %s: error = %v, exit %d; want exit 1 telling the admin to start serve", strings.Join(args, " "), err, exitCode(err))
		}
	}

	// goose_db_version may exist; no application table may.
	d, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	var n int
	q := "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('machines', 'utility_cache', 'audit_events')"
	if err := d.Bun().NewRaw(q).Scan(t.Context(), &n); err != nil || n != 0 {
		t.Errorf("application tables after refused commands = %d, %v; the CLI never migrates", n, err)
	}
}

func TestCLI_RefusesBadConfig(t *testing.T) {
	c := newCLITest(t)
	t.Setenv("HEPHAESTUS_FQDN", "")

	_, err := c.run(t, "boot-next", "list")

	if err == nil || !strings.Contains(err.Error(), "HEPHAESTUS_FQDN is required") {
		t.Errorf("error = %v, want serve's config validation to refuse", err)
	}
}

func TestCLI_NeverTouchesStore(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchAMD64)

	for _, args := range [][]string{
		{"boot-next", "set", cliMAC(1), "tlive"}, {"boot-next", "list"}, {"boot-next", "clear", cliMAC(1)},
		{"utility", "fetch", "tshell"}, {"utility", "list"}, {"boot-next", "list", "--json"},
	} {
		c.mustRun(t, args...)
		c.storeAbsent(t)
	}
}

func TestCLI_GroupsRejectStrayArguments(t *testing.T) {
	c := newCLITest(t)
	for _, args := range [][]string{{"boot-next", "bogus"}, {"utility", "bogus"}, {"boot-next", "list", "extra"}, {"utility", "list", "extra"}} {
		_, err := c.run(t, args...)
		if err == nil || exitCode(err) != 2 {
			t.Errorf("hephaestus %s: error = %v, exit %d; want a usage error, exit 2", strings.Join(args, " "), err, exitCode(err))
		}
	}
}

func TestBootNextSet_RowAtAnotherPinIsStale(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchAMD64)
	// A ready row for an older pin: RequestFetch leaves it alone, and boot row 3
	// would not serve it, so set must not call the utility ready.
	key := fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64}
	if _, err := c.repo.RequestFetch(t.Context(), key, "oldpin", 0, cliT0); err != nil {
		t.Fatalf("RequestFetch: %v", err)
	}
	if moved, err := c.repo.RestoreReady(t.Context(), key, "oldpin", cliT0); err != nil || !moved {
		t.Fatalf("RestoreReady = %v, %v; want the old-pin row ready", moved, err)
	}

	if out := c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell"); !strings.Contains(out, "utility: stale_pin\n") {
		t.Errorf("text output lacks %q:\n%s", "utility: stale_pin", out)
	}
	if got := jsonObject(t, c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json"))["state"]; got != "stale_pin" {
		t.Errorf("state = %v, want stale_pin", got)
	}
}

// hostileCause carries what a hostile mirror could put in an HTTP reason phrase:
// a screen clear and a tab.
const hostileCause = "the upstream answered 200 \x1b[2J\x1b[1;1H\tdone"

func TestCLI_UpstreamErrorsArePrintable(t *testing.T) {
	const printable = "the upstream answered 200 [2J[1;1Hdone"
	tests := []struct {
		name string
		args []string
	}{
		{"boot-next set", []string{"boot-next", "set", cliMAC(1), "tshell"}},
		{"utility fetch", []string{"utility", "fetch", "tshell", "--arch", "amd64"}},
		{"utility list", []string{"utility", "list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			c.machine(t, 1, fleet.ArchAMD64)
			c.breakRow(t, "tshell", fleet.ArchAMD64, hostileCause)

			out := c.mustRun(t, tt.args...)

			if strings.ContainsAny(out, "\x1b\t") {
				t.Errorf("output holds a raw ESC or TAB:\n%q", out)
			}
			if !strings.Contains(out, printable) {
				t.Errorf("output lacks the printable error %q:\n%s", printable, out)
			}
		})
	}

	t.Run("JSON holds the printable error", func(t *testing.T) {
		c := newCLITest(t)
		c.machine(t, 1, fleet.ArchAMD64)
		c.breakRow(t, "tshell", fleet.ArchAMD64, hostileCause)
		got := jsonObject(t, c.mustRun(t, "boot-next", "set", cliMAC(1), "tshell", "--json"))["previous_error"]
		if got != printable {
			t.Errorf("previous_error = %q, want %q", got, printable)
		}
	})
}

func TestSafeText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "the upstream answered 404 Not Found", "the upstream answered 404 Not Found"},
		{"quotes and backslash kept", `open "a\b"`, `open "a\b"`},
		{"unicode kept", "héllo 世界", "héllo 世界"},
		{"escape and tab", "a\x1b[2J\tb", `a\x1b[2J\tb`},
		{"newline and carriage return", "a\r\nb", `a\r\nb`},
		{"invalid UTF-8", "a\xffb", `a\xffb`},
		{"bidi override", "a\u202eb", `a\u202eb`},
		{"C1 control", "a\u009bb", `a\u009bb`},
		{"zero-width space", "a\u200bb", `a\u200bb`},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeText(tt.in); got != tt.want {
				t.Errorf("safeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUtilityFetch_PartialFailurePrintsTheRowsRequested(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want func(t *testing.T, out string)
	}{
		{"text", []string{"utility", "fetch", "tlive"}, func(t *testing.T, out string) {
			t.Helper()
			if !strings.Contains(out, "tlive/amd64: wanted\n") || strings.Contains(out, "arm64") {
				t.Errorf("output = %q, want the amd64 row and no arm64 row", out)
			}
		}},
		{"JSON", []string{"utility", "fetch", "tlive", "--json"}, func(t *testing.T, out string) {
			t.Helper()
			if rows := jsonRows(t, out); len(rows) != 1 || rows[0]["arch"] != "amd64" {
				t.Errorf("rows = %v, want the amd64 row only", rows)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			// The CLI reads its clock once per request: the second request, for
			// arm64, finds its table gone.
			calls := 0
			c.onNow = func() {
				calls++
				if calls == 2 {
					if _, err := c.d.Bun().ExecContext(t.Context(), "DROP TABLE utility_cache"); err != nil {
						t.Errorf("drop utility_cache: %v", err)
					}
				}
			}

			out, err := c.run(t, tt.args...)

			if err == nil || exitCode(err) != 1 {
				t.Fatalf("error = %v, exit %d; want a failure with exit 1", err, exitCode(err))
			}
			tt.want(t, out)
		})
	}
}

func TestUtilityList_MinRAMRoundsUpToMiB(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := writeUtilityRows(cmd, []utilityRow{{Entry: "e", MinRAMBytes: 1<<20 + 1}}); err != nil {
		t.Fatalf("writeUtilityRows: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if fields := strings.Fields(lines[len(lines)-1]); fields[len(fields)-1] != "2" {
		t.Errorf("row %q: minimum RAM for 1 MiB + 1 byte, want 2 MiB, the least that holds it", lines[len(lines)-1])
	}
}

func TestUtilityList_SignedEntryIsNotTOFU(t *testing.T) {
	c := newCLITest(t)
	c.useCatalog(derive(t, testCatalog(t), func(s *catalog.Spec) bool {
		if s.Entry == "tshell" {
			s.Objects[0].Signature = catalog.Signature{SigURL: "https://upstream.invalid/tshell.zip.asc", Key: "tshell.asc"}
		}
		return true
	}))

	rows := jsonRows(t, c.mustRun(t, "utility", "list", "--json"))
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want tlive and tshell for two arches: %v", len(rows), rows)
	}

	for _, row := range rows {
		if want := row["entry"] == "tlive"; row["tofu"] != want {
			t.Errorf("%v/%v: tofu = %v, want %v", row["entry"], row["arch"], row["tofu"], want)
		}
	}
}

// TestUtilityList_JSONCauseIsPrintable stores a cause with a C1 control rune
// through the repository; the JSON listing carries it without that rune.
func TestUtilityList_JSONCauseIsPrintable(t *testing.T) {
	c := newCLITest(t)
	c.breakRow(t, "tshell", fleet.ArchAMD64, "bad\u009b mirror")

	out := c.mustRun(t, "utility", "list", "--json")

	if strings.Contains(out, "\u009b") {
		t.Errorf("utility list --json holds the U+009B control rune: %q", out)
	}
	row := rowFor(t, jsonRows(t, out), "tshell", "amd64")
	if row["last_error"] != "bad mirror" {
		t.Errorf("last_error = %q, want %q", row["last_error"], "bad mirror")
	}
}

// TestUtilityList_TextEscapesLegacyCause seeds a last_error with a raw ESC and
// TAB by SQL: storage now strips them, but a row written before that change
// still holds them, and the text listing must show them escaped.
func TestUtilityList_TextEscapesLegacyCause(t *testing.T) {
	c := newCLITest(t)
	c.breakRow(t, "tshell", fleet.ArchAMD64, "placeholder")
	q := "UPDATE utility_cache SET last_error = ? WHERE entry = 'tshell' AND arch = 'amd64'"
	if _, err := c.d.Bun().ExecContext(t.Context(), q, "legacy \x1b[2J\tmirror"); err != nil {
		t.Fatalf("seed last_error: %v", err)
	}

	out := c.mustRun(t, "utility", "list")

	if strings.ContainsAny(out, "\x1b\t") {
		t.Errorf("utility list holds a raw ESC or TAB:\n%q", out)
	}
	if want := `legacy \x1b[2J\tmirror`; !strings.Contains(out, want) {
		t.Errorf("utility list lacks the escaped cause %q:\n%s", want, out)
	}
}

// TestCLI_JSONPrintsLegacyCauseAsPrintable seeds a stored error with a C1
// control, a bidi override and a zero-width space by SQL: a row written before
// storage cleaned them still holds them, and encoding/json prints them raw.
// Every --json command that shows a stored error prints it printable.
func TestCLI_JSONPrintsLegacyCauseAsPrintable(t *testing.T) {
	const legacy, printable = "legacy\u009b mirror\u202e\u200b", "legacy mirror"
	tests := []struct {
		name  string
		args  []string
		field func(t *testing.T, out string) any
	}{
		{"utility list", []string{"utility", "list", "--json"}, func(t *testing.T, out string) any {
			t.Helper()
			return rowFor(t, jsonRows(t, out), "tshell", "amd64")["last_error"]
		}},
		{"utility fetch", []string{"utility", "fetch", "tshell", "--arch", "amd64", "--json"}, func(t *testing.T, out string) any {
			t.Helper()
			return rowFor(t, jsonRows(t, out), "tshell", "amd64")["previous_error"]
		}},
		{"boot-next set", []string{"boot-next", "set", cliMAC(1), "tshell", "--json"}, func(t *testing.T, out string) any {
			t.Helper()
			return jsonObject(t, out)["previous_error"]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			c.machine(t, 1, fleet.ArchAMD64)
			c.breakRow(t, "tshell", fleet.ArchAMD64, "placeholder")
			q := "UPDATE utility_cache SET last_error = ? WHERE entry = 'tshell' AND arch = 'amd64'"
			if _, err := c.d.Bun().ExecContext(t.Context(), q, legacy); err != nil {
				t.Fatalf("seed last_error: %v", err)
			}

			out := c.mustRun(t, tt.args...)

			if strings.ContainsAny(out, "\u009b\u202e\u200b") {
				t.Errorf("%s holds a control, bidi or zero-width rune: %q", tt.name, out)
			}
			if got := tt.field(t, out); got != printable {
				t.Errorf("%s error = %q, want %q", tt.name, got, printable)
			}
		})
	}
}
