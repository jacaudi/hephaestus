package admin

import (
	"fmt"
	"sync/atomic"
)

// State is the service lifecycle state that health probes report.
type State int

const (
	// Starting means the service is initializing and not yet serving.
	Starting State = iota
	// Ready means the service accepts traffic.
	Ready
	// Draining means the service is shutting down and rejects new traffic.
	Draining
	// Stopped means the service has stopped.
	Stopped
)

// String returns the state's name.
func (s State) String() string {
	switch s {
	case Starting:
		return "starting"
	case Ready:
		return "ready"
	case Draining:
		return "draining"
	case Stopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// MarshalText encodes the state as its name, so every encoding carries the
// string form.
func (s State) MarshalText() ([]byte, error) {
	if s < Starting || s > Stopped {
		return nil, fmt.Errorf("invalid state %d", int(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText decodes a state name and rejects unknown names.
func (s *State) UnmarshalText(text []byte) error {
	for c := range Stopped + 1 {
		if c.String() == string(text) {
			*s = c
			return nil
		}
	}
	return fmt.Errorf("unknown state %q", text)
}

// Lifecycle holds the current State. The zero value is Starting and it is
// safe for concurrent use.
type Lifecycle struct {
	state atomic.Int64
}

// Set records the current state.
func (l *Lifecycle) Set(s State) { l.state.Store(int64(s)) }

// Current returns the current state.
func (l *Lifecycle) Current() State { return State(l.state.Load()) }
