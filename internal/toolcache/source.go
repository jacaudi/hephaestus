package toolcache

import (
	"context"
	"time"
)

// claimClass is one step of the fetch claim order. A worker asks every source
// for a row of the first class, then of the next, and runs the first row
// claimed.
type claimClass int

const (
	// claimUrgent is a utility row at priority 1.
	claimUrgent claimClass = iota
	// claimAssigned is a Talos row an assigned machine wants.
	claimAssigned
	// claimRest is a utility row at priority 0.
	claimRest
	// claimOther is any other Talos row.
	claimOther
)

// fetchRun is the download side of one claimed row.
type fetchRun interface {
	// run takes the row to the point where its content is in the store. It
	// returns ctx's error, wrapped, when ctx ends it.
	run(ctx context.Context) error
	// received is how many bytes this run received.
	received() int64
	// discard removes the row's spool files. What it cannot remove goes to the
	// error handler.
	discard(ctx context.Context)
	// close releases what the run holds, whichever way it ended.
	close(ctx context.Context)
}

// settler is the row side of one claimed row. It uses only the repository, so
// it stays valid after the run is closed.
type settler interface {
	// record stores o: a ready row is marked ready, a deferred one is given
	// back, and any other is a failure. It emits o.ev once the row moved.
	record(ctx context.Context, o outcome) error
	// giveBack moves the row from fetching back to wanted and reports whether it
	// moved.
	giveBack(ctx context.Context, at time.Time) (bool, error)
	// countFailure adds one to the source's failures counter.
	countFailure(ctx context.Context, class string)
}

// claimed is one row a source moved to fetching.
type claimed struct {
	// name says which row it is in error text: "<entry>/<arch>" for a utility.
	name string
	// event is the claim's state change, to fetching.
	event FetchEvent
	run   fetchRun
	row   settler
}

// source is one kind of cache row the fetcher claims.
type source interface {
	// claim moves the next row of class to fetching and returns it. It reports
	// false when the source has none of that class eligible at at.
	claim(ctx context.Context, class claimClass, at time.Time) (claimed, bool, error)
}

// outcome is how one claimed row ended: ready, given back, or a failure to record.
type outcome struct {
	ready bool
	// deferred is a row to give back to wanted, with no attempt and no failure,
	// because the spool volume has no room for it beside the downloads in flight.
	deferred bool
	// cause is the failure as printable text; "" when ready or deferred.
	cause string
	// integrity is a failure no retry can fix.
	integrity bool
	// at is when the failure happened.
	at time.Time
	// ev is the event to emit once the outcome is stored, less To and Attempts.
	ev FetchEvent
}

// heldOutcome is an outcome the database refused, with the row it settles.
// retried is set once the next poll has tried it again.
type heldOutcome struct {
	outcome

	name    string
	row     settler
	retried bool
	// cause is the latest error that kept the outcome from being stored.
	cause error
}
