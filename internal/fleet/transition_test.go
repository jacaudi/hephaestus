package fleet

import (
	"errors"
	"fmt"
	"testing"
)

var (
	guardOK        = Guard{HostExists: true, HostUnbound: true}
	boundStates    = []State{StateAssigned, StateInstalling, StateInstallUnconfirmed, StateInstalled}
	nonCordonedAll = []State{
		StateDiscovered, StateLocalOnly, StateAssigned, StateInstalling,
		StateInstallUnconfirmed, StateInstalled, StateRejected, StateOrphaned,
	}
)

type allowedCase struct {
	from    Lifecycle
	trigger Trigger
	guard   Guard
	want    Lifecycle
}

// allowedCases is design §5.2's transition table, one row per From state.
func allowedCases() []allowedCase {
	cases := []allowedCase{
		{Lifecycle{}, TriggerFirstContact, Guard{}, Lifecycle{State: StateDiscovered}},
		{Lifecycle{State: StateDiscovered}, TriggerApprove, guardOK, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateDiscovered}, TriggerAdopt, guardOK, Lifecycle{State: StateInstalled}},
		{Lifecycle{}, TriggerAdoptByUUID, guardOK, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateDiscovered}, TriggerMarkLocalOnly, Guard{Junk: true}, Lifecycle{State: StateLocalOnly}},
		{Lifecycle{State: StateLocalOnly}, TriggerUnmarkLocalOnly, Guard{}, Lifecycle{State: StateDiscovered}},
		{Lifecycle{State: StateDiscovered}, TriggerReject, Guard{}, Lifecycle{State: StateRejected}},
		{Lifecycle{State: StateRejected}, TriggerUnreject, Guard{}, Lifecycle{State: StateDiscovered}},
		{Lifecycle{State: StateAssigned}, TriggerUnassign, Guard{}, Lifecycle{State: StateDiscovered}},
		{Lifecycle{State: StateAssigned}, TriggerConfigServed, Guard{}, Lifecycle{State: StateInstalling}},
		{Lifecycle{State: StateInstalling}, TriggerNextBoot, Guard{}, Lifecycle{State: StateInstallUnconfirmed}},
		{Lifecycle{State: StateInstalling}, TriggerAbortInstall, Guard{}, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateInstalling}, TriggerMarkInstalled, Guard{}, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateInstallUnconfirmed}, TriggerMarkInstalled, Guard{}, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateInstalled}, TriggerReinstall, Guard{ResetConfirmed: true}, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateInstallUnconfirmed}, TriggerReinstall, Guard{ResetConfirmed: true}, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateCordoned, Prior: StateInstalled}, TriggerHostLeft, Guard{},
			Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: StateInstalled}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerUnbind, Guard{}, Lifecycle{State: StateDiscovered}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerRebind, guardOK, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstallUnconfirmed}, TriggerRebind, guardOK, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalling}, TriggerRebind, guardOK, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateAssigned}, TriggerRebind, guardOK, Lifecycle{State: StateAssigned}},
		{Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerCordon, Guard{},
			Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: StateInstalled}},
		{Lifecycle{State: StateCordoned, Prior: StateInstalled}, TriggerUncordon, Guard{}, Lifecycle{State: StateInstalled}},
		{Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerUncordon, Guard{},
			Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}},
	}

	for _, from := range []State{StateRejected, StateDiscovered, StateLocalOnly} {
		cases = append(cases, allowedCase{Lifecycle{State: from}, TriggerForget, Guard{}, Lifecycle{}})
	}
	for _, x := range boundStates {
		cases = append(cases, allowedCase{Lifecycle{State: x}, TriggerHostLeft, Guard{},
			Lifecycle{State: StateOrphaned, OrphanedFrom: x}})
	}
	for _, x := range nonCordonedAll {
		cases = append(cases, allowedCase{Lifecycle{State: x}, TriggerCordon, Guard{},
			Lifecycle{State: StateCordoned, Prior: x}})
	}

	return cases
}

func TestNext_Allowed(t *testing.T) {
	for _, tt := range allowedCases() {
		name := fmt.Sprintf("%s/%+v", tt.trigger, tt.from)
		t.Run(name, func(t *testing.T) {
			got, err := Next(tt.from, tt.trigger, tt.guard)
			if err != nil {
				t.Fatalf("Next(%+v, %s, %+v): %v", tt.from, tt.trigger, tt.guard, err)
			}
			if got != tt.want {
				t.Errorf("Next(%+v, %s, %+v) = %+v, want %+v", tt.from, tt.trigger, tt.guard, got, tt.want)
			}
		})
	}
}

type rejectedCase struct {
	name    string
	from    Lifecycle
	trigger Trigger
	guard   Guard
	reason  string
}

// hostGuardCases covers the three guard combinations that fail the host checks
// for one action: host_not_found wins whenever the host is missing.
func hostGuardCases(trigger Trigger, from Lifecycle) []rejectedCase {
	return []rejectedCase{
		{string(trigger) + " host missing", from, trigger, Guard{HostExists: false, HostUnbound: true}, "host_not_found"},
		{string(trigger) + " host bound", from, trigger, Guard{HostExists: true, HostUnbound: false}, "host_bound"},
		{string(trigger) + " host missing and bound", from, trigger, Guard{HostExists: false, HostUnbound: false}, "host_not_found"},
	}
}

func rejectedCases() []rejectedCase {
	discovered := Lifecycle{State: StateDiscovered}
	orphanedInstalled := Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}

	cases := []rejectedCase{
		{"approve junk", discovered, TriggerApprove, Guard{Junk: true, HostExists: true, HostUnbound: true}, "junk_uuid"},
		{"adopt junk", discovered, TriggerAdopt, Guard{Junk: true, HostExists: true, HostUnbound: true}, "junk_uuid"},
		{"adopt_by_uuid junk", Lifecycle{}, TriggerAdoptByUUID, Guard{Junk: true, HostExists: true, HostUnbound: true}, "junk_uuid"},
		{"approve junk beats missing host", discovered, TriggerApprove, Guard{Junk: true}, "junk_uuid"},
		{"adopt_by_uuid from discovered", discovered, TriggerAdoptByUUID, guardOK, "not_allowed"},
		{"reinstall without reset confirmation", Lifecycle{State: StateInstalled}, TriggerReinstall, Guard{}, "reset_not_confirmed"},
		{"reinstall from assigned", Lifecycle{State: StateAssigned}, TriggerReinstall, Guard{ResetConfirmed: true}, "not_allowed"},
		{"host_left discovered", discovered, TriggerHostLeft, Guard{}, "not_bound"},
		{"host_left cordoned over discovered", Lifecycle{State: StateCordoned, Prior: StateDiscovered}, TriggerHostLeft, Guard{}, "not_bound"},
		{"host_left cordoned over orphaned", Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerHostLeft, Guard{}, "not_bound"},
		{"host_left orphaned", orphanedInstalled, TriggerHostLeft, Guard{}, "not_bound"},
		{"host_left none", Lifecycle{}, TriggerHostLeft, Guard{}, "not_bound"},
		{"cordon cordoned", Lifecycle{State: StateCordoned, Prior: StateInstalled}, TriggerCordon, Guard{}, "already_cordoned"},
		{"cordon none", Lifecycle{}, TriggerCordon, Guard{}, "not_allowed"},
		{"uncordon discovered", discovered, TriggerUncordon, Guard{}, "not_cordoned"},
		{"uncordon without a prior state", Lifecycle{State: StateCordoned}, TriggerUncordon, Guard{}, "not_allowed"},
		{"approve assigned", Lifecycle{State: StateAssigned}, TriggerApprove, Guard{}, "not_allowed"},
		{"forget assigned", Lifecycle{State: StateAssigned}, TriggerForget, Guard{}, "not_allowed"},
		{"next_boot discovered", discovered, TriggerNextBoot, Guard{}, "not_allowed"},
		{"config_served discovered", discovered, TriggerConfigServed, Guard{}, "not_allowed"},
		{"unbind discovered", discovered, TriggerUnbind, Guard{}, "not_allowed"},
		{"unbind cordoned over orphaned", Lifecycle{State: StateCordoned, Prior: StateOrphaned, OrphanedFrom: StateInstalled}, TriggerUnbind, Guard{}, "not_allowed"},
		{"first_contact discovered", discovered, TriggerFirstContact, Guard{}, "not_allowed"},
		{"rebind discovered", discovered, TriggerRebind, guardOK, "not_allowed"},
		{"rebind without a remembered state", Lifecycle{State: StateOrphaned}, TriggerRebind, guardOK, "not_allowed"},
		{"rebind remembering discovered", Lifecycle{State: StateOrphaned, OrphanedFrom: StateDiscovered}, TriggerRebind, guardOK, "not_allowed"},
		{"unknown trigger", discovered, Trigger("bogus"), guardOK, "not_allowed"},
	}

	cases = append(cases, hostGuardCases(TriggerApprove, discovered)...)
	cases = append(cases, hostGuardCases(TriggerAdopt, discovered)...)
	cases = append(cases, hostGuardCases(TriggerAdoptByUUID, Lifecycle{})...)
	cases = append(cases, hostGuardCases(TriggerRebind, orphanedInstalled)...)

	return cases
}

func TestNext_Rejected(t *testing.T) {
	for _, tt := range rejectedCases() {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Next(tt.from, tt.trigger, tt.guard)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Next(%+v, %s, %+v) error = %v, want ErrInvalidTransition", tt.from, tt.trigger, tt.guard, err)
			}
			te, ok := errors.AsType[*TransitionError](err)
			if !ok {
				t.Fatalf("error %v is not a *TransitionError", err)
			}
			if te.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", te.Reason, tt.reason)
			}
			if te.Trigger != tt.trigger || te.From != tt.from {
				t.Errorf("error carries (%s, %+v), want (%s, %+v)", te.Trigger, te.From, tt.trigger, tt.from)
			}
			if got != tt.from {
				t.Errorf("Next returned %+v on error, want the input %+v unchanged", got, tt.from)
			}
		})
	}
}

// A junk machine is never put into a host, so a rebind of one is refused, and
// before the host is looked at.
func TestNext_RebindJunk(t *testing.T) {
	orphaned := Lifecycle{State: StateOrphaned, OrphanedFrom: StateInstalled}
	for name, g := range map[string]Guard{
		"host free":    {Junk: true, HostExists: true, HostUnbound: true},
		"host missing": {Junk: true, HostUnbound: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Next(orphaned, TriggerRebind, g)
			te, ok := errors.AsType[*TransitionError](err)
			if !ok || te.Reason != reasonJunkUUID {
				t.Fatalf("Next(%+v, rebind, %+v) error = %v, want a %s TransitionError", orphaned, g, err, reasonJunkUUID)
			}
			if got != orphaned {
				t.Errorf("Next returned %+v on error, want the input %+v unchanged", got, orphaned)
			}
		})
	}
}

func TestTransitionError_Message(t *testing.T) {
	tests := []struct {
		name string
		err  *TransitionError
		want string
	}{
		{
			name: "no record",
			err:  &TransitionError{From: Lifecycle{}, Trigger: TriggerCordon, Reason: "not_allowed"},
			want: "cordon from none: not_allowed",
		},
		{
			name: "stored state",
			err:  &TransitionError{From: Lifecycle{State: StateAssigned}, Trigger: TriggerReinstall, Reason: "not_allowed"},
			want: "reinstall from assigned: not_allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
			if !errors.Is(tt.err, ErrInvalidTransition) {
				t.Error("errors.Is(err, ErrInvalidTransition) = false, want true")
			}
		})
	}
}
