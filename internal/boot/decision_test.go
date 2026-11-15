package boot

import (
	"fmt"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	conflictReason = "identity conflict: this UUID was seen with another serial/product"
	awaitingReason = "awaiting approval"
)

// known is a TLS request from a recorded machine in state s whose serial and
// product match the request. UnknownAction is local.
func known(s fleet.State, mods ...func(*Input)) Input {
	in := Input{
		TLS:           true,
		Known:         true,
		Machine:       fleet.Machine{State: s, Serial: "S1", Product: "P1"},
		Request:       Request{Serial: "S1", Product: "P1"},
		UnknownAction: fleet.UnknownActionLocal,
	}
	for _, mod := range mods {
		mod(&in)
	}
	return in
}

func withSerial(v string) func(*Input)  { return func(in *Input) { in.Request.Serial = v } }
func withProduct(v string) func(*Input) { return func(in *Input) { in.Request.Product = v } }
func withPrior(s fleet.State) func(*Input) {
	return func(in *Input) { in.Machine.Prior = s }
}
func withJunk(in *Input)         { in.Machine.Junk = true }
func withAssetsReady(in *Input)  { in.AssetsReady = true }
func withDrift(in *Input)        { in.Drift = true }
func withUtilityReady(in *Input) { in.UtilityReady = true }
func withBootNext(in *Input)     { in.BootNext = "memtest" }
func withHold(in *Input)         { in.UnknownAction = fleet.UnknownActionHold }

func TestDecide(t *testing.T) {
	conflict := Decision{Row: 2, Action: ActionHold, Reason: conflictReason, MarkJunk: true, Alert: true}

	cases := []struct {
		name string
		in   Input
		want Decision
	}{
		{"plain HTTP", known(fleet.StateInstalled, func(in *Input) { in.TLS = false }, withSerial("S2")),
			Decision{Row: 1, Action: ActionChainTLS}},
		{"conflict discovered", known(fleet.StateDiscovered, withSerial("S2")), conflict},
		{"conflict bound", known(fleet.StateAssigned, withProduct("P2")), conflict},
		{"conflict repeat", known(fleet.StateDiscovered, withJunk, withSerial("S2")),
			Decision{Row: 2, Action: ActionHold, Reason: conflictReason}},
		{"conflict beats boot-next", known(fleet.StateInstalled, withSerial("S2"), withBootNext, withUtilityReady), conflict},
		{"boot-next ready", known(fleet.StateInstalled, withBootNext, withUtilityReady),
			Decision{Row: 3, Action: ActionUtility, Utility: "memtest", ClearBootNext: true}},
		{"boot-next not ready", known(fleet.StateInstalled, withBootNext), Decision{Row: 6, Action: ActionExit}},
		{"rejected", known(fleet.StateRejected), Decision{Row: 5, Action: ActionHold, Reason: "machine is rejected"}},
		{"orphaned", known(fleet.StateOrphaned), Decision{Row: 5, Action: ActionHold, Reason: "machine is orphaned"}},
		{"local-only", known(fleet.StateLocalOnly), Decision{Row: 6, Action: ActionExit}},
		{"install-unconfirmed", known(fleet.StateInstallUnconfirmed), Decision{Row: 6, Action: ActionExit}},
		{"installed", known(fleet.StateInstalled), Decision{Row: 6, Action: ActionExit}},
		{"installing", known(fleet.StateInstalling),
			Decision{Row: 6, Action: ActionExit, Trigger: fleet.TriggerNextBoot}},
		{"unknown hold", Input{TLS: true, UnknownAction: fleet.UnknownActionHold},
			Decision{Row: 7, Action: ActionHold, Report: true, Reason: awaitingReason, Trigger: fleet.TriggerFirstContact}},
		{"unknown local", Input{TLS: true, UnknownAction: fleet.UnknownActionLocal},
			Decision{Row: 7, Action: ActionExit, Report: true, Trigger: fleet.TriggerFirstContact}},
		{"discovered hold", known(fleet.StateDiscovered, withHold),
			Decision{Row: 7, Action: ActionHold, Report: true, Reason: awaitingReason}},
		{"discovered local", known(fleet.StateDiscovered), Decision{Row: 7, Action: ActionExit, Report: true}},
		{"assets missing", known(fleet.StateAssigned),
			Decision{Row: 8, Action: ActionHold, Reason: "boot assets not ready"}},
		{"drift", known(fleet.StateAssigned, withAssetsReady, withDrift),
			Decision{Row: 9, Action: ActionHold, Reason: "configuration drift: re-approval required"}},
		{"junk assigned", known(fleet.StateAssigned, withAssetsReady, withJunk),
			Decision{Row: 10, Action: ActionHold, Reason: "junk SMBIOS UUID: installs are refused"}},
		{"install", known(fleet.StateAssigned, withAssetsReady),
			Decision{Row: 10, Action: ActionInstall, NeedsTicket: true}},
	}
	for _, p := range []fleet.State{
		fleet.StateInstalled, fleet.StateInstallUnconfirmed, fleet.StateInstalling, fleet.StateLocalOnly,
	} {
		cases = append(cases, struct {
			name string
			in   Input
			want Decision
		}{fmt.Sprintf("cordoned exits prior %s", p), known(fleet.StateCordoned, withPrior(p)),
			Decision{Row: 4, Action: ActionExit}})
	}
	for _, p := range []fleet.State{
		fleet.StateDiscovered, fleet.StateAssigned, fleet.StateRejected, fleet.StateOrphaned,
	} {
		cases = append(cases, struct {
			name string
			in   Input
			want Decision
		}{fmt.Sprintf("cordoned holds prior %s", p), known(fleet.StateCordoned, withPrior(p)),
			Decision{Row: 5, Action: ActionHold, Reason: "machine is cordoned"}})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.in); got != tc.want {
				t.Errorf("Decide() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
