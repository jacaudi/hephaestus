package main

import (
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// seenAt sets when the machine was last seen.
func (c *cliTest) seenAt(t *testing.T, m fleet.Machine, at time.Time) {
	t.Helper()
	q := "UPDATE machines SET last_seen_at = ? WHERE id = ?"
	if _, err := c.d.Bun().NewRaw(q, at, m.ID).Exec(t.Context()); err != nil {
		t.Fatalf("set last_seen_at of %s: %v", m.ID, err)
	}
}

// wantUnknownAction fails unless the stored unknown_action setting is want.
func (c *cliTest) wantUnknownAction(t *testing.T, want fleet.UnknownAction) {
	t.Helper()
	got, err := c.repo.UnknownAction(t.Context())
	if err != nil || got != want {
		t.Errorf("unknown_action = %q, %v; want %q", got, err, want)
	}
}

func TestUnknownActionHold_NeedsYes(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
	recent := c.machine(t, 2, fleet.ArchAMD64)
	old := c.machine(t, 3, fleet.ArchAMD64)
	rejected := c.machine(t, 4, fleet.ArchAMD64)
	c.trigger(t, rejected, fleet.TriggerReject)
	c.now = cliT0.Add(10 * 24 * time.Hour)
	c.seenAt(t, recent, c.now.Add(-6*24*time.Hour))
	c.seenAt(t, old, c.now.Add(-8*24*time.Hour))
	c.seenAt(t, rejected, c.now)

	out, err := c.run(t, "unknown-action", "set", "hold", "--json")

	wantUsage(t, err, "unknown-action", "set", "hold", "--json")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error = %v, want one that names --yes", err)
	}
	got := jsonObject(t, out)
	wantKeys(t, got, "setting", "applied", "unbound_hosts", "discovered")
	if got["setting"] != "hold" || got["applied"] != false {
		t.Errorf("output = %v, want hold, not applied", got)
	}
	hosts, _ := got["unbound_hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["cluster"] != labName || hosts[0].(map[string]any)["host"] != hostWrk { //nolint:forcetypeassert // the test controls the JSON shape
		t.Errorf("unbound_hosts = %v, want only lab/%s: %s has a machine", hosts, hostWrk, hostCP)
	}
	seen, _ := got["discovered"].([]any)
	if len(seen) != 1 || seen[0].(map[string]any)["id"] != recent.ID { //nolint:forcetypeassert // the test controls the JSON shape
		t.Errorf("discovered = %v, want only machine %s: the others are bound, rejected or unseen for 8 days", seen, recent.ID)
	}
	c.wantUnknownAction(t, fleet.UnknownActionLocal)

	text, err := c.run(t, "unknown-action", "set", "hold")

	wantUsage(t, err, "unknown-action", "set", "hold")
	for _, want := range []string{"lab/" + hostWrk, recent.ID, cliMAC(2), "--yes"} {
		if !strings.Contains(text+err.Error(), want) {
			t.Errorf("text output %q and error %q lack %q", text, err, want)
		}
	}
	for _, unwanted := range []string{old.ID, rejected.ID, "lab/" + hostCP} {
		if strings.Contains(text, unwanted) {
			t.Errorf("text output lists %q, which holds nothing", unwanted)
		}
	}
	c.wantUnknownAction(t, fleet.UnknownActionLocal)

	out = c.mustRun(t, "unknown-action", "set", "hold", "--yes", "--json")

	if got := jsonObject(t, out); got["applied"] != true || got["setting"] != "hold" {
		t.Errorf("output = %v, want hold applied", got)
	}
	c.wantUnknownAction(t, fleet.UnknownActionHold)
	if actor, detail := c.audit(t, "settings.unknown_action"); actor != fleet.ActorCLI || !strings.Contains(detail, "hold") {
		t.Errorf("audit = %q, %q; want actor cli and a detail naming hold", actor, detail)
	}
}

func TestUnknownActionHold_NothingToList(t *testing.T) {
	c := newCLITest(t)

	out, err := c.run(t, "unknown-action", "set", "hold", "--json")

	wantUsage(t, err, "unknown-action", "set", "hold", "--json")
	got := jsonObject(t, out)
	if hosts, ok := got["unbound_hosts"].([]any); !ok || len(hosts) != 0 {
		t.Errorf("unbound_hosts = %v, want []", got["unbound_hosts"])
	}
	if seen, ok := got["discovered"].([]any); !ok || len(seen) != 0 {
		t.Errorf("discovered = %v, want []", got["discovered"])
	}
}

func TestUnknownActionLocal(t *testing.T) {
	c := newCLITest(t)
	if err := c.repo.SetUnknownAction(t.Context(), fleet.UnknownActionHold, fleet.ActorCLI, cliT0); err != nil {
		t.Fatalf("SetUnknownAction: %v", err)
	}

	out := c.mustRun(t, "unknown-action", "set", "local", "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, "setting", "applied")
	if got["setting"] != "local" || got["applied"] != true {
		t.Errorf("output = %v, want local applied", got)
	}
	c.wantUnknownAction(t, fleet.UnknownActionLocal)
	if text := c.mustRun(t, "unknown-action", "set", "local"); !strings.Contains(text, "local") {
		t.Errorf("text output %q does not name the setting", text)
	}
	c.storeAbsent(t)
}

func TestUnknownAction_Usage(t *testing.T) {
	c := newCLITest(t)
	for _, args := range [][]string{
		{"unknown-action"},
		{"unknown-action", "bogus"},
		{"unknown-action", "set"},
		{"unknown-action", "set", "maybe"},
		{"unknown-action", "set", ""},
		{"unknown-action", "set", "local", "hold"},
	} {
		_, err := c.run(t, args...)
		if len(args) == 1 {
			continue // the bare group prints its help
		}
		wantUsage(t, err, args...)
	}
	c.wantUnknownAction(t, fleet.UnknownActionLocal)
}
