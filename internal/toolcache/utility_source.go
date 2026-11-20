package toolcache

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// utilitySource claims utility_cache rows. Each row it claims is a fetchJob,
// which is both the row's run and its settler.
type utilitySource struct{ f *Fetcher }

// claim returns a priority 1 row for claimUrgent and a priority 0 row for
// claimRest, so the Talos rows of an assigned machine come between the two,
// and nothing for any other class.
func (s utilitySource) claim(ctx context.Context, class claimClass, at time.Time) (claimed, bool, error) {
	var priority int
	switch class {
	case claimUrgent:
		priority = 1
	case claimRest:
		priority = 0
	case claimAssigned, claimOther:
		return claimed{}, false, nil
	}
	row, ok, err := s.f.cfg.Repo.ClaimFetchAtPriority(ctx, priority, at)
	if err != nil || !ok {
		return claimed{}, false, err
	}
	job := &fetchJob{f: s.f, row: row}
	return claimed{
		name:  fmt.Sprintf("%s/%s", row.Entry, row.Arch),
		event: FetchEvent{Source: SourceUtility, Key: row.Key(), Pin: row.Pin, From: claimedFrom(row.NextAttemptAt), To: fleet.CacheFetching, Attempts: row.Attempts},
		run:   job,
		row:   job,
	}, true, nil
}

// claimedFrom is the state a claimed row was in, from its next_attempt_at; the
// claim returns the row already fetching. In both tables a failure sets
// next_attempt_at and every way back to wanted clears it except the resets of
// fetching rows, so a set value means failed; a row either gave back after a
// failed claim is reported as failed too.
func claimedFrom(nextAttemptAt time.Time) fleet.CacheState {
	if nextAttemptAt.IsZero() {
		return fleet.CacheWanted
	}
	return fleet.CacheFailed
}

// record stores o and emits its event. A failure that could not be stored
// carries its cause along, for the error handler.
func (j *fetchJob) record(ctx context.Context, o outcome) error {
	f := j.f
	if o.deferred {
		moved, err := j.giveBack(ctx, f.now())
		if err != nil {
			return err
		}
		if moved {
			o.ev.To = fleet.CacheWanted
			f.events(ctx, o.ev)
		}
		return nil
	}
	if o.ready {
		moved, err := f.cfg.Repo.MarkReady(ctx, j.row.Key(), j.row.Pin, f.now())
		if err != nil {
			return err
		}
		if moved {
			o.ev.To = fleet.CacheReady
			f.events(ctx, o.ev)
		}
		return nil
	}
	row, err := f.cfg.Repo.RecordFetchFailure(ctx, fleet.FetchFailure{Key: j.row.Key(), Pin: j.row.Pin, Cause: o.cause, Integrity: o.integrity, At: o.at})
	if err != nil {
		return fmt.Errorf("%w (the fetch failed: %s)", err, o.cause)
	}
	if row.Pin == j.row.Pin && (row.State == fleet.CacheFailed || row.State == fleet.CacheBroken) {
		o.ev.To, o.ev.Attempts = row.State, row.Attempts
		f.events(ctx, o.ev)
	}
	return nil
}

// giveBack moves the row from fetching back to wanted. The row may have been
// repinned or settled since it was claimed; the reset then moves nothing.
func (j *fetchJob) giveBack(ctx context.Context, at time.Time) (bool, error) {
	return j.f.cfg.Repo.ResetFetchingRow(ctx, j.row.Key(), j.row.Pin, at)
}

// countFailure adds one to hephaestus.utility.fetch.failures under class.
func (j *fetchJob) countFailure(ctx context.Context, class string) {
	j.f.failures.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class)))
}
