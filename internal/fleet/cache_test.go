package fleet

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour},
		{9, time.Hour},
		{-3, time.Minute},
		{1000, time.Hour},
	}
	for _, tc := range tests {
		if got := Backoff(tc.attempts); got != tc.want {
			t.Errorf("Backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}
