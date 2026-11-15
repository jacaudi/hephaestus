package admin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestRegistry_RunStopsOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	var runs atomic.Int32
	twice := make(chan struct{})
	reg := NewRegistry(Check{
		Name:     "tick",
		Interval: 10 * time.Millisecond,
		Run: func(context.Context) Result {
			if runs.Add(1) == 2 {
				close(twice)
			}
			return Result{Status: StatusPass}
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Run(ctx)
	}()

	select {
	case <-twice:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not run twice")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
