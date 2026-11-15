package fleet

import (
	"errors"
	"fmt"
)

// Trigger is an event that may move a machine to another lifecycle state.
type Trigger string

// The triggers of design section 5.2's transition table.
const (
	// TriggerFirstContact is a machine's first /boot or /discover.
	TriggerFirstContact Trigger = "first_contact"
	// TriggerApprove approves a discovered machine into an unbound host.
	TriggerApprove Trigger = "approve"
	// TriggerAdopt adopts a discovered machine into a host as installed.
	TriggerAdopt Trigger = "adopt"
	// TriggerAdoptByUUID adopts a machine into a host by a typed UUID, with no prior record.
	TriggerAdoptByUUID Trigger = "adopt_by_uuid"
	// TriggerMarkLocalOnly marks a machine to always boot local disk.
	TriggerMarkLocalOnly Trigger = "mark_local_only"
	// TriggerUnmarkLocalOnly returns a local-only machine to discovered.
	TriggerUnmarkLocalOnly Trigger = "unmark_local_only"
	// TriggerReject turns a discovered machine away.
	TriggerReject Trigger = "reject"
	// TriggerUnreject returns a rejected machine to discovered.
	TriggerUnreject Trigger = "unreject"
	// TriggerForget removes a rejected, discovered or local-only machine.
	TriggerForget Trigger = "forget"
	// TriggerUnassign returns an assigned machine to discovered.
	TriggerUnassign Trigger = "unassign"
	// TriggerConfigServed records that /config was fully written with a 200.
	TriggerConfigServed Trigger = "config_served"
	// TriggerNextBoot records the next /boot of an installing machine.
	TriggerNextBoot Trigger = "next_boot"
	// TriggerAbortInstall is an admin giving up on an install in progress.
	TriggerAbortInstall Trigger = "abort_install"
	// TriggerMarkInstalled is an admin confirming an install.
	TriggerMarkInstalled Trigger = "mark_installed"
	// TriggerReinstall is an admin reinstalling an installed machine.
	TriggerReinstall Trigger = "reinstall"
	// TriggerHostLeft records that a machine's host left the cluster definition.
	TriggerHostLeft Trigger = "host_left"
	// TriggerUnbind is an admin releasing an orphaned machine.
	TriggerUnbind Trigger = "unbind"
	// TriggerRebind is an admin binding an orphaned machine to another unbound host.
	TriggerRebind Trigger = "rebind"
	// TriggerCordon is an admin cordoning a machine.
	TriggerCordon Trigger = "cordon"
	// TriggerUncordon is an admin lifting a cordon.
	TriggerUncordon Trigger = "uncordon"
)

// Lifecycle is a machine's state plus the states it remembers.
type Lifecycle struct {
	State State
	// Prior is the state a cordoned machine returns to; empty otherwise.
	Prior State
	// OrphanedFrom is the bound state an orphaned machine had, kept while it
	// is orphaned or cordoned over orphaned; empty otherwise.
	OrphanedFrom State
}

// Guard carries the facts about the world that a transition depends on. The
// caller computes them from storage and the cluster definition; Next never
// does I/O.
type Guard struct {
	// Junk is true when the machine's UUID is junk.
	Junk bool
	// HostExists is true when the target host is in nodes[].
	HostExists bool
	// HostUnbound is true when no other machine is bound to the target host.
	HostUnbound bool
	// ResetConfirmed is true when the admin confirmed the machine was reset.
	ResetConfirmed bool
}

// Reasons a transition is refused. They are the machine-readable part of a
// TransitionError.
const (
	reasonJunkUUID        = "junk_uuid"
	reasonHostNotFound    = "host_not_found"
	reasonHostBound       = "host_bound"
	reasonResetNotConfirm = "reset_not_confirmed"
	reasonNotBound        = "not_bound"
	reasonAlreadyCordoned = "already_cordoned"
	reasonNotCordoned     = "not_cordoned"
	reasonNotAllowed      = "not_allowed"
)

// ErrInvalidTransition is the sentinel every TransitionError unwraps to.
var ErrInvalidTransition = errors.New("invalid transition")

// TransitionError reports a refused transition: the lifecycle it was tried
// from, the trigger, and a snake_case Reason.
type TransitionError struct {
	From    Lifecycle
	Trigger Trigger
	Reason  string
}

// Error formats as "<trigger> from <state or none>: <reason>".
func (e *TransitionError) Error() string {
	from := string(e.From.State)
	if e.From.State == StateNone {
		from = "none"
	}
	return fmt.Sprintf("%s from %s: %s", e.Trigger, from, e.Reason)
}

// Unwrap returns ErrInvalidTransition.
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

// refuse returns the input lifecycle unchanged with a TransitionError.
func refuse(l Lifecycle, t Trigger, reason string) (Lifecycle, error) {
	return l, &TransitionError{From: l, Trigger: t, Reason: reason}
}

// Next applies trigger t to lifecycle l under guard g and returns the new
// lifecycle, implementing design section 5.2's transition table. A refused
// transition returns l unchanged and a *TransitionError. Next is pure.
func Next(l Lifecycle, t Trigger, g Guard) (Lifecycle, error) {
	switch t {
	case TriggerFirstContact, TriggerMarkLocalOnly, TriggerUnmarkLocalOnly, TriggerReject, TriggerUnreject,
		TriggerForget, TriggerUnassign, TriggerConfigServed, TriggerNextBoot, TriggerAbortInstall,
		TriggerMarkInstalled, TriggerUnbind:
		return move(l, t)
	case TriggerApprove:
		return bind(l, t, g, StateDiscovered, StateAssigned)
	case TriggerAdopt:
		return bind(l, t, g, StateDiscovered, StateInstalled)
	case TriggerAdoptByUUID:
		return bind(l, t, g, StateNone, StateInstalled)
	case TriggerRebind:
		return rebind(l, g)
	case TriggerReinstall:
		return reinstall(l, g)
	case TriggerHostLeft:
		return hostLeft(l)
	case TriggerCordon:
		return cordon(l)
	case TriggerUncordon:
		return uncordon(l)
	}
	return refuse(l, t, reasonNotAllowed)
}

// edge is one row of the transition table whose target is fixed and whose
// only precondition is the From state.
type edge struct {
	trigger Trigger
	from    State
	to      State
}

// move applies the unguarded, fixed-target rows of the table. The target
// lifecycle carries no remembered states.
func move(l Lifecycle, t Trigger) (Lifecycle, error) {
	for _, e := range [...]edge{
		{TriggerFirstContact, StateNone, StateDiscovered},
		{TriggerMarkLocalOnly, StateDiscovered, StateLocalOnly},
		{TriggerUnmarkLocalOnly, StateLocalOnly, StateDiscovered},
		{TriggerReject, StateDiscovered, StateRejected},
		{TriggerUnreject, StateRejected, StateDiscovered},
		{TriggerForget, StateRejected, StateNone},
		{TriggerForget, StateDiscovered, StateNone},
		{TriggerForget, StateLocalOnly, StateNone},
		{TriggerUnassign, StateAssigned, StateDiscovered},
		{TriggerConfigServed, StateAssigned, StateInstalling},
		{TriggerNextBoot, StateInstalling, StateInstallUnconfirmed},
		{TriggerAbortInstall, StateInstalling, StateAssigned},
		{TriggerMarkInstalled, StateInstalling, StateInstalled},
		{TriggerMarkInstalled, StateInstallUnconfirmed, StateInstalled},
		{TriggerUnbind, StateOrphaned, StateDiscovered},
	} {
		if e.trigger == t && e.from == l.State {
			return Lifecycle{State: e.to}, nil
		}
	}
	return refuse(l, t, reasonNotAllowed)
}

// hostGuard returns the reason g refuses a bind to a host, or "" when the host
// exists and is unbound. A missing host wins over a bound one.
func hostGuard(g Guard) string {
	switch {
	case !g.HostExists:
		return reasonHostNotFound
	case !g.HostUnbound:
		return reasonHostBound
	}
	return ""
}

// bind moves a machine from the from state to the to state, which puts it into
// a host. The UUID must not be junk and the host must exist and be unbound.
func bind(l Lifecycle, t Trigger, g Guard, from, to State) (Lifecycle, error) {
	if l.State != from {
		return refuse(l, t, reasonNotAllowed)
	}
	if g.Junk {
		return refuse(l, t, reasonJunkUUID)
	}
	if reason := hostGuard(g); reason != "" {
		return refuse(l, t, reason)
	}
	return Lifecycle{State: to}, nil
}

// rebind binds an orphaned machine to another unbound host. It lands installed
// when the machine was installed or install-unconfirmed before orphaning and
// assigned otherwise. The UUID is the machine's own, so Junk is not consulted.
func rebind(l Lifecycle, g Guard) (Lifecycle, error) {
	if l.State != StateOrphaned || !l.OrphanedFrom.Bound() {
		return refuse(l, TriggerRebind, reasonNotAllowed)
	}
	if reason := hostGuard(g); reason != "" {
		return refuse(l, TriggerRebind, reason)
	}
	if l.OrphanedFrom == StateInstalled || l.OrphanedFrom == StateInstallUnconfirmed {
		return Lifecycle{State: StateInstalled}, nil
	}
	return Lifecycle{State: StateAssigned}, nil
}

// reinstall sends an installed or install-unconfirmed machine back to assigned
// once the admin has confirmed it was reset.
func reinstall(l Lifecycle, g Guard) (Lifecycle, error) {
	if l.State != StateInstalled && l.State != StateInstallUnconfirmed {
		return refuse(l, TriggerReinstall, reasonNotAllowed)
	}
	if !g.ResetConfirmed {
		return refuse(l, TriggerReinstall, reasonResetNotConfirm)
	}
	return Lifecycle{State: StateAssigned}, nil
}

// hostLeft orphans a bound machine, remembering its state. A cordon wrapper is
// kept, with prior state orphaned.
func hostLeft(l Lifecycle) (Lifecycle, error) {
	if l.State == StateCordoned && l.Prior.Bound() {
		return Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: l.Prior}, nil
	}
	if l.State.Bound() {
		return Lifecycle{State: StateOrphaned, OrphanedFrom: l.State}, nil
	}
	return refuse(l, TriggerHostLeft, reasonNotBound)
}

// cordon wraps any stored, non-cordoned state, remembering it and any
// orphaned-from state.
func cordon(l Lifecycle) (Lifecycle, error) {
	switch l.State {
	case StateCordoned:
		return refuse(l, TriggerCordon, reasonAlreadyCordoned)
	case StateNone:
		return refuse(l, TriggerCordon, reasonNotAllowed)
	case StateDiscovered, StateLocalOnly, StateAssigned, StateInstalling, StateInstallUnconfirmed,
		StateInstalled, StateRejected, StateOrphaned:
		return Lifecycle{State: StateCordoned, Prior: l.State, OrphanedFrom: l.OrphanedFrom}, nil
	}
	return refuse(l, TriggerCordon, reasonNotAllowed)
}

// uncordon restores the state a cordoned machine remembers.
func uncordon(l Lifecycle) (Lifecycle, error) {
	if l.State != StateCordoned {
		return refuse(l, TriggerUncordon, reasonNotCordoned)
	}
	if l.Prior == StateNone {
		return refuse(l, TriggerUncordon, reasonNotAllowed)
	}
	return Lifecycle{State: l.Prior, OrphanedFrom: l.OrphanedFrom}, nil
}
