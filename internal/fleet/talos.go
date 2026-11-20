package fleet

import (
	"time"

	"github.com/uptrace/bun"
)

// TalosKey identifies a talos_cache row: a schematic at a Talos version for one
// architecture.
type TalosKey struct {
	Schematic, Version string
	Arch               Arch
}

// Tag is the store tag the key's assets are pushed under:
// "talos/<schematic>-<arch>:<version>".
func (k TalosKey) Tag() string {
	return "talos/" + k.Schematic + "-" + string(k.Arch) + ":" + k.Version
}

// TalosPart is one of the three assets a Talos boot needs. Its value is also
// the title of its layer in the store.
type TalosPart string

// The parts of a Talos boot asset set.
const (
	// TalosKernel is the kernel image.
	TalosKernel TalosPart = "kernel"
	// TalosInitramfs is the initramfs.
	TalosInitramfs TalosPart = "initramfs"
	// TalosCmdline is the kernel command line.
	TalosCmdline TalosPart = "cmdline"
)

// TalosParts returns the three parts in the order they are fetched and layered:
// kernel, initramfs, cmdline.
func TalosParts() [3]TalosPart { return [3]TalosPart{TalosKernel, TalosInitramfs, TalosCmdline} }

// IsSHA256Hex reports whether s is 64 lower-case hex digits: the form of a
// recorded sha256 and of a schematic ID.
func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TalosAsset is the size and sha256 recorded at an asset's first complete
// fetch; later fetches are checked against it.
type TalosAsset struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// TalosAssets are the recorded assets of a row, by part, stored as the JSON
// column assets. A row that has never been fetched has none.
type TalosAssets map[TalosPart]TalosAsset

// Complete reports whether a records every part with a sha256, as a ready row
// must.
func (a TalosAssets) Complete() bool {
	for _, p := range TalosParts() {
		if !IsSHA256Hex(a[p].SHA256) {
			return false
		}
	}
	return true
}

// TalosRow is a stored talos_cache row: the bun model of the table. Body is the
// schematic's canonical body, nil for a literal schematic ID.
type TalosRow struct {
	bun.BaseModel `bun:"table:talos_cache"`

	Schematic     string      `bun:"schematic,pk"`
	Version       string      `bun:"version,pk"`
	Arch          Arch        `bun:"arch,pk"`
	State         CacheState  `bun:"state"`
	Body          []byte      `bun:"body,nullzero"`
	Attempts      int         `bun:"attempts"`
	NextAttemptAt time.Time   `bun:"next_attempt_at,nullzero"`
	BytesDone     int64       `bun:"bytes_done"`
	BytesTotal    int64       `bun:"bytes_total"`
	LastError     string      `bun:"last_error"`
	Assets        TalosAssets `bun:"assets"`
	// ClaimGen counts the row's claims; ClaimTalos adds one.
	ClaimGen  int64     `bun:"claim_gen"`
	UpdatedAt time.Time `bun:"updated_at"`
}

// Key returns the row's key.
func (r TalosRow) Key() TalosKey {
	return TalosKey{Schematic: r.Schematic, Version: r.Version, Arch: r.Arch}
}

// TalosClaim is one claim of a talos_cache row: its key and the claim's
// generation. The writes of a fetch name its claim and land only while the row
// is fetching under it, so once the row is reset or claimed again the old fetch
// can no longer settle it.
type TalosClaim struct {
	Key TalosKey
	Gen int64
}

// Claim returns the claim of the row as ClaimTalos returned it.
func (r TalosRow) Claim() TalosClaim { return TalosClaim{Key: r.Key(), Gen: r.ClaimGen} }

// TalosWant names the row a transaction wants fetched. Body, when set, is the
// schematic body a row without one takes. Rearm moves a failed or broken row
// back to wanted; without it only an absent row is created.
type TalosWant struct {
	Key   TalosKey
	Body  []byte
	Rearm bool
}

// TalosClass is which rows ClaimTalos looks at.
type TalosClass int

// The claim classes, in the order the fetcher prefers them.
const (
	// TalosAssigned claims the rows whose key is the Talos tuple and arch of a
	// machine in assigned.
	TalosAssigned TalosClass = iota
	// TalosOther claims every other row.
	TalosOther
)

// TalosFailure is a failed fetch under Claim. Integrity marks a failure no
// retry can fix, which breaks the row at once.
type TalosFailure struct {
	Claim     TalosClaim
	Cause     string
	Integrity bool
	At        time.Time
}
