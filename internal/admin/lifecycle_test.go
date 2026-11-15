package admin

import "testing"

func TestState_TextRoundTrip(t *testing.T) {
	for _, s := range []State{Starting, Ready, Draining, Stopped} {
		t.Run(s.String(), func(t *testing.T) {
			text, err := s.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText(%v): %v", s, err)
			}
			var got State
			if err := got.UnmarshalText(text); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", text, err)
			}
			if got != s {
				t.Errorf("round trip = %v, want %v", got, s)
			}
		})
	}

	t.Run("unknown name", func(t *testing.T) {
		var s State
		if err := s.UnmarshalText([]byte("bogus")); err == nil {
			t.Error("UnmarshalText(bogus) = nil error, want error")
		}
	})
	t.Run("out of range", func(t *testing.T) {
		if _, err := State(9).MarshalText(); err == nil {
			t.Error("MarshalText(State(9)) = nil error, want error")
		}
	})
}
