package fleet

import (
	"testing"
	"time"
)

func TestTicket_LiveAndValid(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	expires := t0.Add(TicketTTL)
	tests := []struct {
		name      string
		ticket    Ticket
		at        time.Time
		wantLive  bool
		wantValid bool
	}{
		{"unconsumed unrevoked unexpired", Ticket{ExpiresAt: expires}, t0, true, true},
		{"consumed", Ticket{ExpiresAt: expires, ConsumedAt: t0}, t0, false, false},
		{"revoked", Ticket{ExpiresAt: expires, RevokedAt: t0}, t0, false, false},
		{"consumed and revoked", Ticket{ExpiresAt: expires, ConsumedAt: t0, RevokedAt: t0}, t0, false, false},
		{"expired is still live", Ticket{ExpiresAt: expires}, expires.Add(time.Second), true, false},
		{"valid until the instant it expires", Ticket{ExpiresAt: expires}, expires, true, false},
		{"valid a moment before it expires", Ticket{ExpiresAt: expires}, expires.Add(-time.Nanosecond), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ticket.Live(); got != tt.wantLive {
				t.Errorf("Live() = %t, want %t", got, tt.wantLive)
			}
			if got := tt.ticket.Valid(tt.at); got != tt.wantValid {
				t.Errorf("Valid(%v) = %t, want %t", tt.at, got, tt.wantValid)
			}
		})
	}
}
