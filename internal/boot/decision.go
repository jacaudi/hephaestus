package boot

import "go.jacaudi.dev/hephaestus/internal/fleet"

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
	// BootNext is the one-shot utility target, or "" when none is set.
	BootNext string
	// UtilityReady, AssetsReady and Drift are owned by M2 and M3: whether the
	// BootNext utility is cached, whether the machine's boot assets are cached,
	// and whether the machine's rendered configuration has drifted.
	UtilityReady, AssetsReady, Drift bool
}

// Decision is the answer to a /boot request. Row is the design section 5.2
// row that matched. The caller applies Trigger, MarkJunk, Alert and
// ClearBootNext, mints a ticket when NeedsTicket is set, and renders the
// script with Render.
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
	// ClearBootNext clears the one-shot utility target.
	ClearBootNext bool
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
	case in.Known && (m.Serial != in.Request.Serial || m.Product != in.Request.Product):
		// A repeat on an already-junk record holds without a second alert.
		return Decision{Row: 2, Action: ActionHold, Reason: reasonConflict, MarkJunk: !m.Junk, Alert: !m.Junk}
	case in.BootNext != "" && in.UtilityReady:
		return Decision{Row: 3, Action: ActionUtility, Utility: in.BootNext, ClearBootNext: true}
	case !in.Known:
		return unknownDecision(in, fleet.TriggerFirstContact)
	}
	return knownDecision(in)
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
