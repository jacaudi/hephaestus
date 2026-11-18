package fleet

import (
	"time"

	"github.com/uptrace/bun"
)

// CacheState is where a utility_cache row is in the fetch lifecycle of design
// section 5.4.
type CacheState string

// The utility_cache states.
const (
	// CacheWanted waits for the fetcher to claim it.
	CacheWanted CacheState = "wanted"
	// CacheFetching is being downloaded.
	CacheFetching CacheState = "fetching"
	// CacheReady is in the content store and can be served.
	CacheReady CacheState = "ready"
	// CacheFailed failed transiently and is retried when next_attempt_at is due.
	CacheFailed CacheState = "failed"
	// CacheBroken failed for good; only an admin action or a repin re-arms it.
	CacheBroken CacheState = "broken"
)

// ClearReason is why a machine's boot-next flag was cleared.
type ClearReason string

// The reasons of a boot_next.cleared audit event.
const (
	// ClearServed means the entry's script was issued.
	ClearServed ClearReason = "served"
	// ClearCLI means an admin cleared it.
	ClearCLI ClearReason = "cli"
	// ClearUnsupported means the machine's platform cannot boot the entry.
	ClearUnsupported ClearReason = "unsupported"
	// ClearUnknownEntry means the catalog no longer has the entry.
	ClearUnknownEntry ClearReason = "unknown_entry"
	// ClearExpired means the flag outlived its time to live.
	ClearExpired ClearReason = "expired"
	// ClearBroken means the entry's cache row became broken.
	ClearBroken ClearReason = "broken"
)

const (
	// MaxFetchAttempts is the failure that makes a row broken.
	MaxFetchAttempts = 10
	// MaxCauseBytes is the longest failure cause a row stores; a longer one is cut
	// on a rune boundary.
	MaxCauseBytes = 1 << 10
	// BackoffBase is the wait after the first failure; each failure doubles it.
	BackoffBase = time.Minute
	// BackoffCap is the longest wait between attempts.
	BackoffCap = time.Hour

	// maxBackoffShift keeps BackoffBase << shift from overflowing; the cap is
	// reached long before it.
	maxBackoffShift = 20
)

// Backoff is how long a row waits after its attempts-th failure: BackoffBase,
// doubling, capped at BackoffCap.
func Backoff(attempts int) time.Duration {
	shift := min(max(attempts-1, 0), maxBackoffShift)
	return min(BackoffBase<<shift, BackoffCap)
}

// CacheKey identifies a utility_cache row: a catalog entry for one architecture.
type CacheKey struct {
	Entry string
	Arch  Arch
}

// FetchWant names the row and the catalog pin a boot or an admin wants fetched.
type FetchWant struct {
	Key CacheKey
	Pin string
}

// CacheRow is a stored utility_cache row: the bun model of the table.
type CacheRow struct {
	bun.BaseModel `bun:"table:utility_cache"`

	Entry         string     `bun:"entry,pk"`
	Arch          Arch       `bun:"arch,pk"`
	Pin           string     `bun:"pin"`
	State         CacheState `bun:"state"`
	Attempts      int        `bun:"attempts"`
	NextAttemptAt time.Time  `bun:"next_attempt_at,nullzero"`
	Priority      int        `bun:"priority"`
	BytesDone     int64      `bun:"bytes_done"`
	BytesTotal    int64      `bun:"bytes_total"`
	LastError     string     `bun:"last_error"`
	UpdatedAt     time.Time  `bun:"updated_at"`
}

// Key returns the row's key.
func (c CacheRow) Key() CacheKey { return CacheKey{Entry: c.Entry, Arch: c.Arch} }

// ClearBootNextRequest compare-and-clears one machine's boot-next flag: only
// the flag that names Entry and was set at SetAt is cleared.
type ClearBootNextRequest struct {
	MachineID, Entry string
	SetAt            time.Time
	Reason           ClearReason
	Actor            string
	At               time.Time
}

// FetchProgress is the fetcher's progress on the object it is fetching for the
// row at Key and Pin: Done of Total bytes.
type FetchProgress struct {
	Key         CacheKey
	Pin         string
	Done, Total int64
}

// FetchFailure is a failed fetch of the row at Key and Pin. Integrity marks a
// failure no retry can fix, which breaks the row at once.
type FetchFailure struct {
	Key       CacheKey
	Pin       string
	Cause     string
	Integrity bool
	At        time.Time
}

// FetchRequest is what RequestFetch found: the row as stored, the last_error
// an admin's request cleared and whether it re-armed a failed or broken row.
type FetchRequest struct {
	Row           CacheRow
	PreviousError string
	Rearmed       bool
}
