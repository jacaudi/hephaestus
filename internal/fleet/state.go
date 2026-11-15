package fleet

import "fmt"

// State is where a machine stands in its lifecycle. The zero value, StateNone,
// means there is no record.
type State string

// The lifecycle states of design section 5.2.
const (
	// StateNone is no record: the From of first contact and of adopt-by-UUID,
	// and the To of forget. It is never stored.
	StateNone State = ""
	// StateDiscovered is a machine that has been seen but not yet placed.
	StateDiscovered State = "discovered"
	// StateLocalOnly is a junk-UUID or adopted non-Talos machine; it always boots local disk.
	StateLocalOnly State = "local-only"
	// StateAssigned is a machine approved into a host, waiting to install.
	StateAssigned State = "assigned"
	// StateInstalling is a machine that has been served its config.
	StateInstalling State = "installing"
	// StateInstallUnconfirmed is a machine that booted again after being served its config.
	StateInstallUnconfirmed State = "install-unconfirmed"
	// StateInstalled is a machine an admin confirmed installed, or adopted as installed.
	StateInstalled State = "installed"
	// StateRejected is a machine an admin turned away.
	StateRejected State = "rejected"
	// StateOrphaned is a machine whose host left the cluster definition.
	StateOrphaned State = "orphaned"
	// StateCordoned wraps a remembered prior state.
	StateCordoned State = "cordoned"
)

// UnmarshalText accepts only the nine stored state names, so a corrupt row or
// request body cannot smuggle in a state the machine does not know.
func (s *State) UnmarshalText(text []byte) error {
	switch v := State(text); v {
	case StateDiscovered, StateLocalOnly, StateAssigned, StateInstalling, StateInstallUnconfirmed,
		StateInstalled, StateRejected, StateOrphaned, StateCordoned:
		*s = v
		return nil
	case StateNone: // not a stored name; falls through to the error
	}
	return fmt.Errorf("unknown state %q", text)
}

// Bound reports whether the machine is bound to a host: assigned, installing,
// install-unconfirmed or installed.
func (s State) Bound() bool {
	switch s {
	case StateAssigned, StateInstalling, StateInstallUnconfirmed, StateInstalled:
		return true
	case StateNone, StateDiscovered, StateLocalOnly, StateRejected, StateOrphaned, StateCordoned:
		return false
	}
	return false
}

// UnknownAction is what /boot does with a machine it has no record of, or
// that is still discovered.
type UnknownAction string

// The unknown_action settings of design section 5.2.
const (
	// UnknownActionHold keeps the machine in the hold loop.
	UnknownActionHold UnknownAction = "hold"
	// UnknownActionLocal boots the machine's local disk.
	UnknownActionLocal UnknownAction = "local"
)

// UnmarshalText accepts only "hold" and "local".
func (a *UnknownAction) UnmarshalText(text []byte) error {
	switch v := UnknownAction(text); v {
	case UnknownActionHold, UnknownActionLocal:
		*a = v
		return nil
	}
	return fmt.Errorf("unknown_action %q must be %q or %q", text, UnknownActionHold, UnknownActionLocal)
}

// Arch is a machine's CPU architecture as the boot chain names it.
type Arch string

// The supported architectures.
const (
	// ArchAMD64 is x86-64.
	ArchAMD64 Arch = "amd64"
	// ArchARM64 is 64-bit ARM.
	ArchARM64 Arch = "arm64"
)

// ArchFromBuildArch maps the iPXE ${buildarch} value to an Arch: "arm64" is
// arm64 and anything else is amd64, as booter does.
func ArchFromBuildArch(s string) Arch {
	if s == "arm64" {
		return ArchARM64
	}
	return ArchAMD64
}
