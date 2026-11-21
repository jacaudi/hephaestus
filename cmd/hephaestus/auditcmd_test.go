package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// auditSeed stores a test.* audit event the given time before the CLI's clock.
func (c *cliTest) auditSeed(t *testing.T, ago time.Duration, action, machineID, detail string) {
	t.Helper()
	ev := fleet.AuditEvent{At: cliT0.Add(-ago), Actor: fleet.ActorCLI, Action: action, MachineID: machineID, Detail: map[string]string{"note": detail}}
	if err := c.repo.Audit(t.Context(), ev); err != nil {
		t.Fatalf("Audit(%s): %v", action, err)
	}
}

// testActions are the test.* actions of rows, in order: machines' own boot events share the log.
func testActions(rows []map[string]any) []string {
	var got []string
	for _, row := range rows {
		if action, _ := row["action"].(string); strings.HasPrefix(action, "test.") {
			got = append(got, action)
		}
	}
	return got
}

func TestAuditList_SinceAndMachine(t *testing.T) {
	c := newCLITest(t)
	m1, m2 := c.machine(t, 1, fleet.ArchAMD64), c.machine(t, 2, fleet.ArchAMD64)
	c.auditSeed(t, 48*time.Hour, "test.old", m1.ID, "two days back")
	c.auditSeed(t, time.Hour, "test.recent", m1.ID, "an hour back")
	c.auditSeed(t, 30*time.Minute, "test.other", m2.ID, "another machine")
	c.auditSeed(t, 10*time.Minute, "test.global", "", "no machine")

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"everything", nil, []string{"test.old", "test.recent", "test.other", "test.global"}},
		{"since", []string{"--since", "2h"}, []string{"test.recent", "test.other", "test.global"}},
		{"machine by MAC", []string{"--machine", cliMAC(1)}, []string{"test.old", "test.recent"}},
		{"machine by UUID", []string{"--machine", cliUUID(2)}, []string{"test.other"}},
		{"since and machine", []string{"--since", "2h", "--machine", cliMAC(1)}, []string{"test.recent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := jsonRows(t, c.mustRun(t, slices.Concat([]string{"audit", "list", "--json"}, tt.args)...))

			if got := testActions(rows); !slices.Equal(got, tt.want) {
				t.Errorf("test actions = %v, want %v", got, tt.want)
			}
		})
	}

	rows := jsonRows(t, c.mustRun(t, "audit", "list", "--json", "--machine", cliMAC(2)))
	var other map[string]any
	for _, row := range rows {
		if row["action"] == "test.other" {
			other = row
		}
	}
	wantKeys(t, other, "at", "actor", "action", "machine_id", "detail")
	if other["at"] != "2026-11-14T11:30:00Z" || other["actor"] != fleet.ActorCLI || other["machine_id"] != m2.ID ||
		other["detail"] != `{"note":"another machine"}` {
		t.Errorf("row = %v, want the event stored at 11:30 for machine %s", other, m2.ID)
	}
}

func TestAuditList_Text(t *testing.T) {
	c := newCLITest(t)
	c.auditSeed(t, time.Minute, "test.noted", "", "a note")

	out := c.mustRun(t, "audit", "list")

	for _, want := range []string{"AT", "ACTOR", "ACTION", "MACHINE", "DETAIL", "test.noted", "2026-11-14T11:59:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAuditList_Empty(t *testing.T) {
	c := newCLITest(t)

	if got := c.mustRun(t, "audit", "list", "--json"); strings.TrimSpace(got) != "[]" {
		t.Errorf("--json output = %q, want []", got)
	}
	if got := c.mustRun(t, "audit", "list"); !strings.Contains(got, "no audit events") {
		t.Errorf("text output = %q, want a note that there are none", got)
	}
}

func TestAuditList_Refusals(t *testing.T) {
	c := newCLITest(t)

	for _, since := range []string{"soon", "-1h", "0s"} {
		_, err := c.run(t, "audit", "list", "--since", since)
		wantUsage(t, err, "audit", "list", "--since", since)
	}
	_, err := c.run(t, "audit", "list", "--machine", cliMAC(9))
	if err == nil || !strings.Contains(err.Error(), "no machine matches") {
		t.Errorf("unknown machine: error = %v, want the no-match refusal", err)
	}
}
