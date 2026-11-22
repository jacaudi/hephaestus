package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	hostCP  = "cp-01"
	hostWrk = "wrk-01"
	// recordedHash is a sha256 for a recorded Talos asset.
	recordedHash = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

// machineKeys are the keys of a machine row in JSON.
func machineKeys() []string {
	return []string{
		"id", "uuid", "macs", "serial", "product", "arch", "state", "prior_state", "orphaned_from", "cluster", "host",
		"talos_schematic", "talos_version", "junk", "last_seen_at",
	}
}

// labApproved imports the cluster lab and approves a pair with the hosts cp-01 and wrk-01.
func (c *cliTest) labApproved(t *testing.T) {
	t.Helper()
	c.importLab(t)
	id := c.storePending(t, "T1",
		labHost(hostCP, labConfig(hostCP, 1), "c1", "d1"), labHost(hostWrk, labConfig(hostWrk, 1), "c2", "d2"))
	c.approveLab(t, id)
}

// get is the stored machine with the id.
func (c *cliTest) get(t *testing.T, id string) fleet.Machine {
	t.Helper()
	m, err := c.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return m
}

// assign approves m into the host of lab through the repository.
func (c *cliTest) assign(t *testing.T, m fleet.Machine, host string) fleet.Machine {
	t.Helper()
	req := clusters.MachineRequest{MachineID: m.ID, Cluster: labName, Host: host, Actor: fleet.ActorCLI, At: cliT0}
	out, err := c.clusters.ApproveMachine(t.Context(), req)
	if err != nil {
		t.Fatalf("ApproveMachine(%s, %s): %v", m.ID, host, err)
	}
	return out
}

// mint mints a ticket for the assigned machine m, as /boot does, and returns its token and the stored ticket.
func (c *cliTest) mint(t *testing.T, m fleet.Machine) (string, fleet.Ticket) {
	t.Helper()
	cl, err := c.clusters.Get(t.Context(), labName)
	if err != nil {
		t.Fatalf("Get(%s): %v", labName, err)
	}
	pair, err := c.clusters.Pair(t.Context(), cl.ApprovedPairID)
	if err != nil {
		t.Fatalf("Pair(%s): %v", cl.ApprovedPairID, err)
	}
	cur := c.get(t, m.ID)
	token, tk, err := c.repo.MintTicket(t.Context(), fleet.MintRequest{
		MachineID: cur.ID, ClusterID: cl.ID, HostName: *cur.HostName, PairID: pair.ID, SecretVersionID: pair.SecretVersionID,
		TalosSchematic: *cur.TalosSchematic, TalosVersion: *cur.TalosVersion, Arch: cur.Arch, At: cliT0,
	})
	if err != nil {
		t.Fatalf("MintTicket(%s): %v", m.ID, err)
	}
	return token, tk
}

// install serves the assigned machine m its config, as /config does: the machine is installing afterwards.
func (c *cliTest) install(t *testing.T, m fleet.Machine) fleet.Machine {
	t.Helper()
	_, tk := c.mint(t, m)
	res, err := c.repo.RecordConfigServed(t.Context(), fleet.RecordConfigRequest{TicketID: tk.ID, At: cliT0})
	if err != nil || !res.Recorded {
		t.Fatalf("RecordConfigServed = %+v, %v; want the delivery recorded", res, err)
	}
	return res.Machine
}

// trigger applies trigger to m through the repository.
func (c *cliTest) trigger(t *testing.T, m fleet.Machine, trigger fleet.Trigger) fleet.Machine {
	t.Helper()
	out, err := c.repo.Transition(t.Context(), fleet.TransitionRequest{MachineID: m.ID, Trigger: trigger, Actor: fleet.ActorCLI, At: cliT0})
	if err != nil {
		t.Fatalf("Transition(%s, %s): %v", m.ID, trigger, err)
	}
	return out
}

// talosKey is the key of the Talos row of m, which must be bound.
func talosKey(m fleet.Machine) fleet.TalosKey {
	return fleet.TalosKey{Schematic: *m.TalosSchematic, Version: *m.TalosVersion, Arch: m.Arch}
}

// talosRow is the stored row of key; it must exist.
func (c *cliTest) talosRow(t *testing.T, key fleet.TalosKey) fleet.TalosRow {
	t.Helper()
	row, ok, err := c.repo.Talos(t.Context(), key)
	if err != nil || !ok {
		t.Fatalf("Talos(%s) = %v, %v; want the row", key.Tag(), ok, err)
	}
	return row
}

// claim moves the row at key to fetching and returns its claim.
func (c *cliTest) claim(t *testing.T, key fleet.TalosKey) fleet.TalosClaim {
	t.Helper()
	row, ok, err := c.repo.ClaimTalosRow(t.Context(), key, cliT0)
	if err != nil || !ok {
		t.Fatalf("ClaimTalosRow(%s) = %v, %v; want it claimed", key.Tag(), ok, err)
	}
	return row.Claim()
}

// breakTalos breaks the wanted row at key by an integrity failure.
func (c *cliTest) breakTalos(t *testing.T, key fleet.TalosKey) {
	t.Helper()
	f := fleet.TalosFailure{Claim: c.claim(t, key), Cause: "recorded hash mismatch", Integrity: true, At: cliT0}
	if row, err := c.repo.RecordTalosFailure(t.Context(), f); err != nil || row.State != fleet.CacheBroken {
		t.Fatalf("RecordTalosFailure = %+v, %v; want a broken row", row, err)
	}
}

// readyTalos makes the wanted row at key ready, with every asset recorded.
func (c *cliTest) readyTalos(t *testing.T, key fleet.TalosKey) {
	t.Helper()
	assets := fleet.TalosAssets{}
	for _, p := range fleet.TalosParts() {
		assets[p] = fleet.TalosAsset{Size: 10, SHA256: recordedHash}
	}
	if ok, err := c.repo.MarkTalosReady(t.Context(), c.claim(t, key), assets, cliT0); err != nil || !ok {
		t.Fatalf("MarkTalosReady = %v, %v; want the row ready", ok, err)
	}
}

func TestMachineApprove(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "machine", "approve", cliMAC(1), "lab/"+hostCP, "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, append(machineKeys(), "from_state")...)
	for key, want := range map[string]any{
		"id": m.ID, "state": "assigned", "from_state": "discovered", "cluster": labName, "host": hostCP,
		"talos_schematic": plainSchematic, "talos_version": labTalos,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	stored := c.get(t, m.ID)
	if stored.State != fleet.StateAssigned || stored.HostName == nil || *stored.HostName != hostCP {
		t.Errorf("stored = %+v, want assigned to %s", stored, hostCP)
	}
	if row := c.talosRow(t, talosKey(stored)); row.State != fleet.CacheWanted {
		t.Errorf("Talos row state = %s, want wanted", row.State)
	}
	if actor, _ := c.audit(t, "transition.approve"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
	c.storeAbsent(t)
}

func TestMachineApprove_Text(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "machine", "approve", cliUUID(1), "lab/"+hostCP)

	for _, want := range []string{m.ID, "discovered", "assigned", "lab/" + hostCP} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestMachineAdopt(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	out := c.mustRun(t, "machine", "adopt", cliMAC(1), "lab/"+hostCP, "--json")

	got := jsonObject(t, out)
	if got["state"] != "installed" || got["from_state"] != "discovered" || got["host"] != hostCP {
		t.Errorf("output = %v, want installed in %s, from discovered", got, hostCP)
	}
	stored := c.get(t, m.ID)
	if _, ok, err := c.repo.Talos(t.Context(), talosKey(stored)); err != nil || ok {
		t.Errorf("Talos row exists = %v, %v; want none: an adopted machine installs nothing", ok, err)
	}
	if actor, _ := c.audit(t, "transition.adopt"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestMachineAdoptByUUID(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	const uuid = "4C4C4544-0042-3510-8052-B4C04F4E4D77"

	out := c.mustRun(t, "machine", "adopt", "--uuid", uuid, "--arch", "arm64", "lab/"+hostWrk, "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, append(machineKeys(), "from_state")...)
	for key, want := range map[string]any{
		"uuid": strings.ToLower(uuid), "arch": "arm64", "state": "installed", "from_state": "none", "cluster": labName, "host": hostWrk,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	m, err := c.repo.ResolveMachine(t.Context(), uuid)
	if err != nil {
		t.Fatalf("ResolveMachine(%s): %v", uuid, err)
	}
	if m.IdentityObserved || len(m.MACs) != 0 {
		t.Errorf("machine = %+v, want an unobserved identity and no MACs", m)
	}
	if actor, _ := c.audit(t, "transition.adopt_by_uuid"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestMachineBind_Refusals(t *testing.T) {
	tests := []struct {
		name string
		// run prepares the fixture and returns the arguments of the refused command.
		run  func(t *testing.T, c *cliTest) []string
		want string
	}{
		{"host bound", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.assign(t, c.machine(t, 2, fleet.ArchAMD64), hostCP)
			c.machine(t, 1, fleet.ArchAMD64)
			return []string{"approve", cliMAC(1), "lab/" + hostCP}
		}, "already bound"},
		{"host missing", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.machine(t, 1, fleet.ArchAMD64)
			return []string{"approve", cliMAC(1), "lab/nope-01"}
		}, "not in the approved pair"},
		{"cluster missing", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.machine(t, 1, fleet.ArchAMD64)
			return []string{"approve", cliMAC(1), "nope/" + hostCP}
		}, "no cluster named nope"},
		{"junk uuid", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.record(t, fleet.Identity{UUID: zeroUUID, MAC: cliMAC(1), Serial: "S", Product: "P"}, fleet.ArchAMD64)
			return []string{"approve", cliMAC(1), "lab/" + hostCP}
		}, "junk"},
		{"already bound machine", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
			return []string{"approve", cliMAC(1), "lab/" + hostWrk}
		}, "assigned"},
		{"adopt by a UUID a machine holds", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.machine(t, 1, fleet.ArchAMD64)
			return []string{"adopt", "--uuid", cliUUID(1), "--arch", "amd64", "lab/" + hostCP}
		}, "already has a record"},
		{"adopt by a junk UUID", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			return []string{"adopt", "--uuid", zeroUUID, "--arch", "amd64", "lab/" + hostCP}
		}, "junk"},
		{"rebind a machine that is not orphaned", func(t *testing.T, c *cliTest) []string {
			t.Helper()
			c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
			return []string{"rebind", cliMAC(1), "lab/" + hostWrk}
		}, "assigned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			c.labApproved(t)
			args := append([]string{"machine"}, tt.run(t, c)...)

			_, err := c.run(t, args...)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("hephaestus %s: error = %v, want one saying %q", strings.Join(args, " "), err, tt.want)
			}
			if exitCode(err) != exitError {
				t.Errorf("exit code = %d, want %d", exitCode(err), exitError)
			}
		})
	}
}

func TestMachineRebind(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"its own former host", hostCP},
		{"another unbound host", hostWrk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLITest(t)
			c.labApproved(t)
			m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
			c.trigger(t, m, fleet.TriggerHostLeft)

			out := c.mustRun(t, "machine", "rebind", cliMAC(1), "lab/"+tt.host, "--json")

			got := jsonObject(t, out)
			if got["state"] != "assigned" || got["from_state"] != "orphaned" || got["host"] != tt.host {
				t.Errorf("output = %v, want assigned to %s, from orphaned", got, tt.host)
			}
			stored := c.get(t, m.ID)
			if stored.HostName == nil || *stored.HostName != tt.host || stored.State != fleet.StateAssigned {
				t.Errorf("stored = %+v, want assigned to %s", stored, tt.host)
			}
			if row := c.talosRow(t, talosKey(stored)); row.State != fleet.CacheWanted {
				t.Errorf("Talos row state = %s, want wanted", row.State)
			}
			if actor, _ := c.audit(t, "transition.rebind"); actor != fleet.ActorCLI {
				t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
			}
		})
	}
}

func TestMachineTransitions(t *testing.T) {
	discovered := func(t *testing.T, c *cliTest) fleet.Machine {
		t.Helper()
		return c.machine(t, 1, fleet.ArchAMD64)
	}
	assigned := func(t *testing.T, c *cliTest) fleet.Machine {
		t.Helper()
		c.labApproved(t)
		return c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
	}
	installing := func(t *testing.T, c *cliTest) fleet.Machine {
		t.Helper()
		return c.install(t, assigned(t, c))
	}
	orphaned := func(t *testing.T, c *cliTest) fleet.Machine {
		t.Helper()
		return c.trigger(t, assigned(t, c), fleet.TriggerHostLeft)
	}
	via := func(setup func(*testing.T, *cliTest) fleet.Machine, trigger fleet.Trigger) func(*testing.T, *cliTest) fleet.Machine {
		return func(t *testing.T, c *cliTest) fleet.Machine {
			t.Helper()
			return c.trigger(t, setup(t, c), trigger)
		}
	}
	tests := []struct {
		command string
		setup   func(*testing.T, *cliTest) fleet.Machine
		action  string
		from    fleet.State
		to      fleet.State
		// unbound is true when the command releases the machine's host.
		unbound bool
	}{
		{"mark-installed", installing, "transition.mark_installed", fleet.StateInstalling, fleet.StateInstalled, false},
		{"abort", installing, "transition.abort_install", fleet.StateInstalling, fleet.StateAssigned, false},
		{"unassign", assigned, "transition.unassign", fleet.StateAssigned, fleet.StateDiscovered, true},
		{"unbind", orphaned, "transition.unbind", fleet.StateOrphaned, fleet.StateDiscovered, true},
		{"cordon", discovered, "transition.cordon", fleet.StateDiscovered, fleet.StateCordoned, false},
		{"uncordon", via(discovered, fleet.TriggerCordon), "transition.uncordon", fleet.StateCordoned, fleet.StateDiscovered, false},
		{"local-only", discovered, "transition.mark_local_only", fleet.StateDiscovered, fleet.StateLocalOnly, false},
		{"unmark-local-only", via(discovered, fleet.TriggerMarkLocalOnly), "transition.unmark_local_only", fleet.StateLocalOnly, fleet.StateDiscovered, false},
		{"reject", discovered, "transition.reject", fleet.StateDiscovered, fleet.StateRejected, false},
		{"unreject", via(discovered, fleet.TriggerReject), "transition.unreject", fleet.StateRejected, fleet.StateDiscovered, false},
		{"forget", via(discovered, fleet.TriggerReject), "transition.forget", fleet.StateRejected, fleet.StateNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			c := newCLITest(t)
			m := tt.setup(t, c)

			out := c.mustRun(t, "machine", tt.command, cliMAC(1), "--json")

			got := jsonObject(t, out)
			wantKeys(t, got, append(machineKeys(), "from_state")...)
			wantTo := string(tt.to)
			if tt.to == fleet.StateNone {
				wantTo = "none"
			}
			if got["id"] != m.ID || got["from_state"] != string(tt.from) || got["state"] != wantTo {
				t.Errorf("output = %v, want machine %s from %s to %s", got, m.ID, tt.from, wantTo)
			}
			if actor, _ := c.audit(t, tt.action); actor != fleet.ActorCLI {
				t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
			}
			if tt.to == fleet.StateNone {
				if _, err := c.repo.Get(t.Context(), m.ID); !errors.Is(err, fleet.ErrNotFound) {
					t.Errorf("Get after forget = %v, want ErrNotFound", err)
				}
				return
			}
			stored := c.get(t, m.ID)
			if stored.State != tt.to {
				t.Errorf("stored state = %s, want %s", stored.State, tt.to)
			}
			if tt.unbound && (stored.ClusterID != nil || stored.HostName != nil) {
				t.Errorf("stored binding = %v/%v, want none", stored.ClusterID, stored.HostName)
			}
		})
	}
}

func TestMachineTransitions_Refused(t *testing.T) {
	c := newCLITest(t)
	m := c.machine(t, 1, fleet.ArchAMD64)

	for _, command := range []string{"abort", "mark-installed", "unassign", "unbind", "uncordon", "unmark-local-only", "unreject"} {
		_, err := c.run(t, "machine", command, cliMAC(1))
		if err == nil || exitCode(err) != exitError {
			t.Errorf("machine %s of a discovered machine: error = %v, exit %d; want a refusal, exit %d", command, err, exitCode(err), exitError)
		}
	}
	if got := c.get(t, m.ID); got.State != fleet.StateDiscovered {
		t.Errorf("state = %s, want it left discovered", got.State)
	}
	_, err := c.run(t, "machine", "cordon", "ff:ff:ff:00:00:01")
	if err == nil || !strings.Contains(err.Error(), "no machine matches") {
		t.Errorf("unknown machine: error = %v, want one saying no machine matches", err)
	}
}

func TestMachineReinstallNeedsFlag(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
	key := talosKey(m)
	c.breakTalos(t, key)
	c.install(t, m)
	c.trigger(t, m, fleet.TriggerMarkInstalled)

	for _, args := range [][]string{
		{"machine", "reinstall", cliMAC(1)},
		{"machine", "reinstall", cliMAC(1), "--topf-reset-full-done=false"},
	} {
		_, err := c.run(t, args...)
		wantUsage(t, err, args...)
	}
	if got := c.get(t, m.ID); got.State != fleet.StateInstalled {
		t.Fatalf("state = %s after the refused reinstalls, want installed", got.State)
	}

	out := c.mustRun(t, "machine", "reinstall", cliMAC(1), "--topf-reset-full-done", "--json")

	got := jsonObject(t, out)
	if got["state"] != "assigned" || got["from_state"] != "installed" {
		t.Errorf("output = %v, want installed to assigned", got)
	}
	if row := c.talosRow(t, key); row.State != fleet.CacheWanted || row.Attempts != 0 {
		t.Errorf("Talos row = %s with %d attempts, want wanted and 0: the reinstall re-arms it", row.State, row.Attempts)
	}
	if actor, _ := c.audit(t, "transition.reinstall"); actor != fleet.ActorCLI {
		t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
	}
}

func TestMachineReinstall_HelpGivesTheRunbook(t *testing.T) {
	c := newCLITest(t)

	out := c.mustRun(t, "machine", "reinstall", "--help")

	for _, want := range []string{"topf reset", "--full=false", "META", "power-cycle"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	if abort := c.mustRun(t, "machine", "abort", "--help"); !strings.Contains(abort, "full reset") {
		t.Errorf("abort help lacks the full-reset warning:\n%s", abort)
	}
}

// The recorded-but-not-received runbook, first branch: the machine is still installing, so abort frees it and the
// next boot mints a new ticket.
func TestMachine_RecordedNotReceivedInstalling(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.install(t, c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP))
	if _, ok, err := c.repo.LiveTicket(t.Context(), m.ID); err != nil || ok {
		t.Fatalf("LiveTicket = %v, %v; want none: the delivery consumed it", ok, err)
	}

	out := c.mustRun(t, "machine", "abort", cliMAC(1), "--json")

	if got := jsonObject(t, out); got["state"] != "assigned" || got["from_state"] != "installing" {
		t.Errorf("output = %v, want installing to assigned", got)
	}
	token, tk := c.mint(t, m)
	if token == "" || tk.MachineID != m.ID {
		t.Errorf("MintTicket after the abort = %q, %+v; want a new ticket for the machine", token, tk)
	}
}

// The recorded-but-not-received runbook, second branch: one network boot moved the machine to install-unconfirmed,
// abort refuses it, and a full reset followed by reinstall frees it.
func TestMachine_RecordedNotReceivedInstallUnconfirmed(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.install(t, c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP))
	_, _, err := c.repo.RecordBoot(t.Context(), fleet.BootRecord{
		Identity: fleet.Identity{UUID: cliUUID(1), MAC: cliMAC(1), Serial: "S", Product: "P"}, Arch: fleet.ArchAMD64,
		MachineID: m.ID, Trigger: fleet.TriggerNextBoot, Event: fleet.BootEvent{Row: 6, Action: "exit"}, At: cliT0,
	})
	if err != nil {
		t.Fatalf("RecordBoot: %v", err)
	}
	if got := c.get(t, m.ID); got.State != fleet.StateInstallUnconfirmed {
		t.Fatalf("state = %s, want install-unconfirmed", got.State)
	}

	_, err = c.run(t, "machine", "abort", cliMAC(1))

	if err == nil || exitCode(err) != exitError {
		t.Fatalf("abort: error = %v, exit %d; want a refusal, exit %d", err, exitCode(err), exitError)
	}
	for _, want := range []string{"install-unconfirmed", "topf reset", "machine reinstall --topf-reset-full-done"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort refusal %q lacks %q", err, want)
		}
	}
	if got := c.get(t, m.ID); got.State != fleet.StateInstallUnconfirmed {
		t.Errorf("state = %s after the refused abort, want install-unconfirmed", got.State)
	}

	out := c.mustRun(t, "machine", "reinstall", cliMAC(1), "--topf-reset-full-done", "--json")

	if got := jsonObject(t, out); got["state"] != "assigned" || got["from_state"] != "install-unconfirmed" {
		t.Errorf("output = %v, want install-unconfirmed to assigned", got)
	}
	if token, _ := c.mint(t, m); token == "" {
		t.Error("MintTicket after the reinstall returned no token")
	}
}

func TestMachineFetch(t *testing.T) {
	t.Run("a broken row is wanted again with its attempts reset", func(t *testing.T) {
		c := newCLITest(t)
		c.labApproved(t)
		m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
		key := talosKey(m)
		c.breakTalos(t, key)
		if row := c.talosRow(t, key); row.State != fleet.CacheBroken || row.Attempts == 0 {
			t.Fatalf("row = %s with %d attempts, want broken with attempts", row.State, row.Attempts)
		}

		out := c.mustRun(t, "machine", "fetch", cliMAC(1), "--json")

		got := jsonObject(t, out)
		wantKeys(t, got, "machine_id", "schematic", "version", "arch", "wanted", "reset_hash", "state", "attempts", "reason")
		for k, want := range map[string]any{
			"machine_id": m.ID, "schematic": key.Schematic, "version": key.Version, "arch": "amd64", "wanted": true,
			"reset_hash": false, "state": "wanted", "attempts": float64(0),
		} {
			if got[k] != want {
				t.Errorf("%s = %v, want %v", k, got[k], want)
			}
		}
		if row := c.talosRow(t, key); row.State != fleet.CacheWanted || row.Attempts != 0 {
			t.Errorf("row = %s with %d attempts, want wanted with 0", row.State, row.Attempts)
		}
		if actor, detail := c.audit(t, "talos.fetch_requested"); actor != fleet.ActorCLI || !strings.Contains(detail, key.Schematic) {
			t.Errorf("audit = %q, %q; want actor cli and a detail naming the schematic", actor, detail)
		}
	})

	t.Run("an absent row is created with the approved host row's body", func(t *testing.T) {
		c := newCLITest(t)
		c.importLab(t)
		host := labHost(hostCP, labConfig(hostCP, 1), "c1", "d1")
		host.SchematicBody = []byte("customization: {}\n")
		c.approveLab(t, c.storePending(t, "T1", host))
		m := c.machine(t, 1, fleet.ArchAMD64)
		if _, err := c.clusters.AdoptMachine(t.Context(), clusters.MachineRequest{
			MachineID: m.ID, Cluster: labName, Host: hostCP, Actor: fleet.ActorCLI, At: cliT0,
		}); err != nil {
			t.Fatalf("AdoptMachine: %v", err)
		}
		key := talosKey(c.get(t, m.ID))
		if _, ok, err := c.repo.Talos(t.Context(), key); err != nil || ok {
			t.Fatalf("Talos row exists = %v, %v; want none before the fetch", ok, err)
		}

		out := c.mustRun(t, "machine", "fetch", cliUUID(1), "--json")

		if got := jsonObject(t, out); got["wanted"] != true || got["state"] != "wanted" {
			t.Errorf("output = %v, want wanted", got)
		}
		if row := c.talosRow(t, key); row.State != fleet.CacheWanted || !bytes.Equal(row.Body, host.SchematicBody) {
			t.Errorf("row = %s with body %q, want wanted with %q", row.State, row.Body, host.SchematicBody)
		}
	})

	t.Run("a machine whose host left the approved pair has nothing to fetch", func(t *testing.T) {
		c := newCLITest(t)
		c.labApproved(t)
		m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
		c.approveLab(t, c.storePending(t, "T2", labHost(hostWrk, labConfig(hostWrk, 2), "c3", "d3")))
		if got := c.get(t, m.ID); got.State != fleet.StateOrphaned || got.HostName == nil {
			t.Fatalf("machine = %+v, want it orphaned and still naming its former host", got)
		}

		out := c.mustRun(t, "machine", "fetch", cliMAC(1), "--json")

		if got := jsonObject(t, out); got["wanted"] != false || got["reason"] != "machine is not bound to a host" {
			t.Errorf("output = %v, want nothing wanted: a host the approved pair lacks is nothing to fetch, not a failure", got)
		}
	})

	t.Run("a machine without a host has nothing to fetch", func(t *testing.T) {
		c := newCLITest(t)
		c.labApproved(t)
		c.machine(t, 1, fleet.ArchAMD64)

		for _, args := range [][]string{
			{"machine", "fetch", cliMAC(1), "--json"},
			{"machine", "fetch", cliMAC(1), "--reset-hash", "--json"},
		} {
			out := c.mustRun(t, args...)

			got := jsonObject(t, out)
			if got["wanted"] != false || got["reason"] != "machine is not bound to a host" {
				t.Errorf("hephaestus %s: output = %v, want nothing wanted, because the machine is not bound to a host",
					strings.Join(args, " "), got)
			}
		}
		if rows, err := c.repo.TalosRows(t.Context()); err != nil || len(rows) != 0 {
			t.Errorf("Talos rows = %+v, %v; want none", rows, err)
		}
		text := c.mustRun(t, "machine", "fetch", cliMAC(1))
		if !strings.Contains(text, "nothing to fetch") {
			t.Errorf("text output %q lacks %q", text, "nothing to fetch")
		}
	})
}

func TestMachineFetchResetHash(t *testing.T) {
	t.Run("a ready row is wanted again with its assets cleared", func(t *testing.T) {
		c := newCLITest(t)
		c.labApproved(t)
		m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
		key := talosKey(m)
		c.readyTalos(t, key)
		if row := c.talosRow(t, key); row.State != fleet.CacheReady || !row.Assets.Complete() {
			t.Fatalf("row = %s with assets %v, want ready and complete", row.State, row.Assets)
		}

		out := c.mustRun(t, "machine", "fetch", cliMAC(1), "--reset-hash", "--json")

		if got := jsonObject(t, out); got["wanted"] != true || got["reset_hash"] != true || got["state"] != "wanted" {
			t.Errorf("output = %v, want wanted with the hash reset", got)
		}
		if row := c.talosRow(t, key); row.State != fleet.CacheWanted || len(row.Assets) != 0 {
			t.Errorf("row = %s with assets %v, want wanted with none", row.State, row.Assets)
		}
		if actor, _ := c.audit(t, "talos.hash_reset"); actor != fleet.ActorCLI {
			t.Errorf("audit actor = %q, want %q", actor, fleet.ActorCLI)
		}
	})

	t.Run("a broken row too, and an absent row is created", func(t *testing.T) {
		c := newCLITest(t)
		c.labApproved(t)
		m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
		key := talosKey(m)
		c.breakTalos(t, key)

		c.mustRun(t, "machine", "fetch", cliMAC(1), "--reset-hash")

		if row := c.talosRow(t, key); row.State != fleet.CacheWanted || row.Attempts != 0 {
			t.Errorf("row = %s with %d attempts, want wanted with 0", row.State, row.Attempts)
		}
		adopted := c.machine(t, 2, fleet.ArchARM64)
		if _, err := c.clusters.AdoptMachine(t.Context(), clusters.MachineRequest{
			MachineID: adopted.ID, Cluster: labName, Host: hostWrk, Actor: fleet.ActorCLI, At: cliT0,
		}); err != nil {
			t.Fatalf("AdoptMachine: %v", err)
		}

		out := c.mustRun(t, "machine", "fetch", cliMAC(2), "--reset-hash", "--json")

		if got := jsonObject(t, out); got["wanted"] != true || got["reset_hash"] != false {
			t.Errorf("output = %v, want wanted, with no hash to reset", got)
		}
		if row := c.talosRow(t, talosKey(c.get(t, adopted.ID))); row.State != fleet.CacheWanted {
			t.Errorf("row state = %s, want wanted", row.State)
		}
	})
}

func TestMachineShow_PendingTicket(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
	c.now = cliT0.Add(time.Minute)
	token, tk := c.mint(t, m)

	out := c.mustRun(t, "machine", "show", cliMAC(1), "--json")

	got := jsonObject(t, out)
	wantKeys(t, got, append(machineKeys(), "talos", "ticket")...)
	ticket, _ := got["ticket"].(map[string]any)
	wantKeys(t, ticket, "minted_at", "expires_at", "config_requested", "config_requested_at")
	if ticket["minted_at"] != formatTime(tk.MintedAt) || ticket["config_requested"] != false || ticket["config_requested_at"] != "" {
		t.Errorf("ticket = %v, want minted at %s and not requested", ticket, formatTime(tk.MintedAt))
	}
	talos, _ := got["talos"].(map[string]any)
	wantKeys(t, talos, "state", "attempts", "last_error", "assets_recorded")
	if talos["state"] != "wanted" || got["cluster"] != labName || got["host"] != hostCP {
		t.Errorf("show = %v, want the wanted Talos row and the binding to lab/%s", got, hostCP)
	}
	text := c.mustRun(t, "machine", "show", cliMAC(1))
	for _, want := range []string{"config requested: no", formatTime(tk.MintedAt), "lab/" + hostCP, "wanted"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
	if err := c.repo.MarkTicketRequested(t.Context(), tk.ID, cliT0); err != nil {
		t.Fatalf("MarkTicketRequested: %v", err)
	}
	if text := c.mustRun(t, "machine", "show", cliMAC(1)); !strings.Contains(text, "config requested: yes") {
		t.Errorf("text output lacks %q after the request:\n%s", "config requested: yes", text)
	}
	for _, secret := range []string{token, tk.TokenSHA256, tk.ID} {
		if strings.Contains(out, secret) || strings.Contains(text, secret) {
			t.Errorf("output prints %q, a ticket secret", secret)
		}
	}
}

func TestMachineShow_NoTicketNoTalos(t *testing.T) {
	c := newCLITest(t)
	c.machine(t, 1, fleet.ArchARM64)

	out := c.mustRun(t, "machine", "show", cliMAC(1), "--json")

	got := jsonObject(t, out)
	if got["ticket"] != nil || got["talos"] != nil || got["state"] != "discovered" || got["arch"] != "arm64" {
		t.Errorf("show = %v, want a discovered arm64 machine with no ticket and no Talos row", got)
	}
	if text := c.mustRun(t, "machine", "show", cliMAC(1)); !strings.Contains(text, "ticket: none") {
		t.Errorf("text output lacks %q:\n%s", "ticket: none", text)
	}
}

func TestMachineList(t *testing.T) {
	c := newCLITest(t)
	if out := c.mustRun(t, "machine", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list JSON = %q, want []", out)
	}
	if out := c.mustRun(t, "machine", "list"); !strings.Contains(out, "no machines") {
		t.Errorf("empty list text = %q, want a note", out)
	}
	c.labApproved(t)
	first := c.assign(t, c.machine(t, 1, fleet.ArchAMD64), hostCP)
	second := c.machine(t, 2, fleet.ArchARM64)

	rows := jsonRows(t, c.mustRun(t, "machine", "list", "--json"))

	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", rows)
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		wantKeys(t, row, machineKeys()...)
		id, _ := row["id"].(string)
		byID[id] = row
	}
	if row := byID[first.ID]; row["state"] != "assigned" || row["cluster"] != labName || row["host"] != hostCP {
		t.Errorf("row of %s = %v, want the assigned machine in lab/%s", first.ID, row, hostCP)
	}
	if macs, _ := byID[second.ID]["macs"].([]any); len(macs) != 1 || macs[0] != cliMAC(2) || byID[second.ID]["state"] != "discovered" {
		t.Errorf("row of %s = %v, want the discovered machine with its MAC", second.ID, byID[second.ID])
	}
	text := c.mustRun(t, "machine", "list")
	for _, want := range []string{"STATE", first.ID, "lab/" + hostCP, cliMAC(2), "arm64"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
}

func TestMachine_Usage(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	m := c.machine(t, 1, fleet.ArchAMD64)
	mac, ref := cliMAC(1), "lab/"+hostCP
	for _, args := range [][]string{
		{"machine", "approve", mac},
		{"machine", "approve", mac, "lab"},
		{"machine", "approve", mac, "lab/"},
		{"machine", "approve", mac, "/" + hostCP},
		{"machine", "approve", mac, "lab/" + hostCP + "/extra"},
		{"machine", "approve", mac, ref, "extra"},
		{"machine", "adopt", "--uuid", cliUUID(9), ref},
		{"machine", "adopt", "--arch", "amd64", ref},
		{"machine", "adopt", "--uuid", cliUUID(9), "--arch", "mips", ref},
		{"machine", "adopt", "--uuid", cliUUID(9), "--arch", "", ref},
		{"machine", "adopt", "--uuid", "", "--arch", "amd64", ref},
		{"machine", "adopt", "--uuid", "not-a-uuid", "--arch", "amd64", ref},
		{"machine", "adopt", "--uuid", cliUUID(9), "--arch", "amd64", mac, ref},
		{"machine", "adopt", mac, ref, "--arch", "amd64"},
		{"machine", "adopt", mac},
		{"machine", "rebind", mac},
		{"machine", "rebind", ref},
		{"machine", "abort"},
		{"machine", "abort", mac, "extra"},
		{"machine", "show"},
		{"machine", "fetch"},
		{"machine", "fetch", mac, "extra"},
		{"machine", "list", "extra"},
		{"machine", "bogus"},
		{"machine", "reinstall", mac},
	} {
		_, err := c.run(t, args...)
		wantUsage(t, err, args...)
	}
	if got := c.get(t, m.ID); got.State != fleet.StateDiscovered {
		t.Errorf("state = %s, want the usage errors to change nothing", got.State)
	}
}

func TestMachine_NeverOpensStores(t *testing.T) {
	c := newCLITest(t)
	c.labApproved(t)
	c.machine(t, 1, fleet.ArchAMD64)

	for _, args := range [][]string{
		{"machine", "list"}, {"machine", "show", cliMAC(1)}, {"machine", "approve", cliMAC(1), "lab/" + hostCP},
		{"machine", "fetch", cliMAC(1), "--json"}, {"machine", "unassign", cliMAC(1)},
	} {
		c.mustRun(t, args...)
		c.storeAbsent(t)
	}
}
