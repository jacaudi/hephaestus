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

	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/catalog"
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
	deps cliDeps
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

	c := &cliTest{dir: dir, d: d, repo: fleet.NewRepository(d), now: cliT0}
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
	var out, errOut bytes.Buffer
	cmd := newRootCmdWith(c.deps)
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
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
	m, err := c.repo.RecordBoot(t.Context(), fleet.BootRecord{
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

// storeAbsent fails when the CLI left a content store in the data dir.
func (c *cliTest) storeAbsent(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(c.dir, "store")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat <data>/store = %v, want it not to exist: the CLI never opens the store", err)
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
		if err := c.repo.RecordProgress(t.Context(), fleet.FetchProgress{Key: key, Pin: pin, Done: 1 << 20, Total: 4 << 20, At: cliT0}); err != nil {
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
