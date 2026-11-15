package fleet

import (
	"encoding/json"
	"testing"
)

// storedStates are the nine names a State may carry in storage.
func storedStates() []State {
	return []State{
		StateDiscovered, StateLocalOnly, StateAssigned, StateInstalling,
		StateInstallUnconfirmed, StateInstalled, StateRejected, StateOrphaned, StateCordoned,
	}
}

func TestState_UnmarshalText(t *testing.T) {
	for _, want := range storedStates() {
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", want, err)
		}
		var got State
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("json.Unmarshal(%s): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("round trip of %q = %q", want, got)
		}
	}

	for _, in := range []string{"", "bogus", "Discovered"} {
		got := StateInstalled
		if err := got.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("UnmarshalText(%q) = nil error, want an error", in)
		}
		if got != StateInstalled {
			t.Errorf("UnmarshalText(%q) changed the target to %q", in, got)
		}
	}
}

func TestState_Bound(t *testing.T) {
	bound := map[State]bool{
		StateAssigned: true, StateInstalling: true, StateInstallUnconfirmed: true, StateInstalled: true,
	}

	for _, s := range append(storedStates(), StateNone) {
		if got, want := s.Bound(), bound[s]; got != want {
			t.Errorf("%q.Bound() = %v, want %v", s, got, want)
		}
	}
}

func TestUnknownAction_UnmarshalText(t *testing.T) {
	tests := []struct {
		in      string
		want    UnknownAction
		wantErr bool
	}{
		{in: "hold", want: UnknownActionHold},
		{in: "local", want: UnknownActionLocal},
		{in: "exit", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tt := range tests {
		var got UnknownAction
		err := got.UnmarshalText([]byte(tt.in))
		if (err != nil) != tt.wantErr {
			t.Errorf("UnmarshalText(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("UnmarshalText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestArchFromBuildArch(t *testing.T) {
	tests := []struct {
		in   string
		want Arch
	}{
		{in: "arm64", want: ArchARM64},
		{in: "x86_64", want: ArchAMD64},
		{in: "i386", want: ArchAMD64},
		{in: "", want: ArchAMD64},
	}

	for _, tt := range tests {
		if got := ArchFromBuildArch(tt.in); got != tt.want {
			t.Errorf("ArchFromBuildArch(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
