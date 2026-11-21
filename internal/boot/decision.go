package boot

import (
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// BootNextTTL is how long after it was set a boot-next flag may still fire.
const BootNextTTL = 24 * time.Hour

// Action is what a /boot answer does for the machine.
type Action string

// The actions a Decision can take.
const (
	// ActionChainTLS re-chains the machine to /boot over TLS.
	ActionChainTLS Action = "chain_tls"
	// ActionHold keeps the machine in the 30-second hold loop.
	ActionHold Action = "hold"
	// ActionExit hands the machine back to its firmware to boot local disk.
	ActionExit Action = "exit"
	// ActionUtility boots a utility OS.
	ActionUtility Action = "utility"
	// ActionInstall boots the Talos installer.
	ActionInstall Action = "install"
)

// Hold reasons. They are echoed to the machine's console by the hold script.
const (
	reasonConflict    = "identity conflict: this UUID was seen with another serial/product"
	reasonCordoned    = "machine is cordoned"
	reasonRejected    = "machine is rejected"
	reasonOrphaned    = "machine is orphaned"
	reasonUnknown     = "machine state is unknown"
	reasonAwaiting    = "awaiting approval"
	reasonAssets      = "boot assets not ready"
	reasonDrift       = "configuration drift: re-approval required"
	reasonJunkInstall = "junk SMBIOS UUID: installs are refused"
)

// Request is what a /boot request says about the machine. It is the sanitised
// form of the request: the fields are compared, never echoed into a script.
type Request struct {
	UUID, MAC, Serial, Product string
	Arch                       fleet.Arch
	// Platform is the firmware the machine booted with, or "" when the request
	// named none or one the catalog does not recognise.
	Platform catalog.Platform
}

// UtilityFacts are what the boot-next utility is in the catalog and the store,
// for the machine's architecture and firmware.
type UtilityFacts struct {
	// InCatalog is whether the catalog has the entry at all.
	InCatalog bool
	// Supported is whether the entry boots on the machine's architecture and a
	// recognised firmware.
	Supported bool
	// Ready is whether the entry's cache row is ready at the catalog's pin and
	// its layers pass the size check.
	Ready bool
}

// Input is everything Decide looks at.
type Input struct {
	// TLS is whether the request was served by a TLS route, directly or through
	// the reverse proxy that terminated the TLS.
	TLS bool
	// Known is whether Machine is a recorded machine.
	Known bool
	// Machine is the recorded machine; it is read only when Known.
	Machine fleet.Machine
	// Request is what the request said about the machine.
	Request Request
	// UnknownAction is the unknown_action setting.
	UnknownAction fleet.UnknownAction
	// Now is the time of the decision; BootNext's age is measured against it.
	Now time.Time
	// BootNext is the one-shot utility target and BootNextSetAt when it was set,
	// or "" when none is set.
	BootNext      string
	BootNextSetAt time.Time
	// PlatformKnown is whether Request.Platform is a recognised firmware.
	PlatformKnown bool
	// Utility is what BootNext is in the catalog and the store; it is read only
	// when BootNext is set.
	Utility UtilityFacts
	// AssetsReady and Drift are read only for an assigned machine: whether its
	// Talos row is ready and its content intact in the store, and whether the
	// approved render of its host has drifted.
	AssetsReady, Drift bool
}

// Decision is the answer to a /boot request. Row is the design section 5.2
// row that matched. The caller applies Trigger, MarkJunk, Alert,
// BootNextClear and WantUtility, mints a ticket when NeedsTicket is set, and
// renders the script with Render, or with the entry's recipe for ActionUtility.
type Decision struct {
	Row    int
	Action Action
	// Report wraps the script in the discovery report.
	Report bool
	// Reason is the console text of a hold.
	Reason string
	// Trigger is the lifecycle transition to record, or "" for none.
	Trigger fleet.Trigger
	// MarkJunk marks the machine's UUID junk; Alert raises an operator alert.
	MarkJunk, Alert bool
	// BootNextClear is why the caller compare-and-clears the one-shot utility
	// target, or "" to leave it.
	BootNextClear fleet.ClearReason
	// WantUtility asks the caller to want the BootNext utility's content fetched.
	WantUtility bool
	// NeedsTicket asks the caller to mint a config ticket (row 10 only).
	NeedsTicket bool
	// Utility is the utility OS to boot (row 3 only).
	Utility string
}

// Decide applies the ten rows of design section 5.2 in order; the first match
// wins.
func Decide(in Input) Decision {
	m := in.Machine
	switch {
	case !in.TLS:
		return Decision{Row: 1, Action: ActionChainTLS}
	case in.Known && m.IdentityObserved && (m.Serial != in.Request.Serial || m.Product != in.Request.Product):
		// A machine whose identity was never observed (one adopted by its UUID)
		// takes the request's serial and product as its first report instead.
		// A repeat on an already-junk record holds without a second alert.
		return Decision{Row: 2, Action: ActionHold, Reason: reasonConflict, MarkJunk: !m.Junk, Alert: !m.Junk}
	case in.Known && in.BootNext != "":
		return bootNextDecision(in)
	case !in.Known:
		return unknownDecision(in, fleet.TriggerFirstContact)
	}
	return knownDecision(in)
}

// bootNextDecision is row 3 for a recorded machine with a boot-next flag: a
// stale, unknown or unsupported flag is cleared and rows 4 to 10 decide the
// boot; a ready one boots its utility; one not ready is kept and wanted, and
// rows 4 to 10 decide the boot.
func bootNextDecision(in Input) Decision {
	var why fleet.ClearReason
	var want bool
	switch {
	case in.Now.Sub(in.BootNextSetAt) > BootNextTTL:
		why = fleet.ClearExpired
	case !in.Utility.InCatalog:
		why = fleet.ClearUnknownEntry
	case in.PlatformKnown && !in.Utility.Supported:
		// An unrecognised platform is never unsupported, only not ready.
		why = fleet.ClearUnsupported
	case in.PlatformKnown && in.Utility.Ready:
		return Decision{Row: 3, Action: ActionUtility, Utility: in.BootNext, BootNextClear: fleet.ClearServed}
	default:
		want = true
	}
	d := knownDecision(in)
	d.BootNextClear, d.WantUtility = why, want
	return d
}

// knownDecision is rows 4 to 10 for a recorded machine.
func knownDecision(in Input) Decision {
	m := in.Machine
	switch m.State {
	case fleet.StateCordoned:
		switch m.Prior {
		case fleet.StateInstalled, fleet.StateInstallUnconfirmed, fleet.StateInstalling, fleet.StateLocalOnly:
			return Decision{Row: 4, Action: ActionExit}
		case fleet.StateNone, fleet.StateDiscovered, fleet.StateAssigned, fleet.StateRejected,
			fleet.StateOrphaned, fleet.StateCordoned:
		}
		return Decision{Row: 5, Action: ActionHold, Reason: reasonCordoned}
	case fleet.StateRejected:
		return Decision{Row: 5, Action: ActionHold, Reason: reasonRejected}
	case fleet.StateOrphaned:
		return Decision{Row: 5, Action: ActionHold, Reason: reasonOrphaned}
	case fleet.StateLocalOnly, fleet.StateInstallUnconfirmed, fleet.StateInstalled:
		return Decision{Row: 6, Action: ActionExit}
	case fleet.StateInstalling:
		return Decision{Row: 6, Action: ActionExit, Trigger: fleet.TriggerNextBoot}
	case fleet.StateDiscovered:
		return unknownDecision(in, "")
	case fleet.StateAssigned:
		return assignedDecision(in)
	case fleet.StateNone:
	}
	// A recorded machine always has a state; hold rather than guess.
	return Decision{Row: 5, Action: ActionHold, Reason: reasonUnknown}
}

// assignedDecision is rows 8 to 10.
func assignedDecision(in Input) Decision {
	switch {
	case !in.AssetsReady:
		return Decision{Row: 8, Action: ActionHold, Reason: reasonAssets}
	case in.Drift:
		return Decision{Row: 9, Action: ActionHold, Reason: reasonDrift}
	case in.Machine.Junk:
		return Decision{Row: 10, Action: ActionHold, Reason: reasonJunkInstall}
	}
	return Decision{Row: 10, Action: ActionInstall, NeedsTicket: true}
}

// unknownDecision is row 7: report, then hold or exit per unknown_action.
func unknownDecision(in Input, trigger fleet.Trigger) Decision {
	if in.UnknownAction == fleet.UnknownActionHold {
		return Decision{Row: 7, Action: ActionHold, Report: true, Reason: reasonAwaiting, Trigger: trigger}
	}
	return Decision{Row: 7, Action: ActionExit, Report: true, Trigger: trigger}
}
