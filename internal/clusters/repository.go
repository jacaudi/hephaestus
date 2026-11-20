package clusters

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

var (
	// ErrNotFound is returned when no cluster, pair or secret version has the name or id.
	ErrNotFound = errors.New("clusters: not found")
	// ErrExists is returned by Import when a cluster already has the name.
	ErrExists = errors.New("clusters: a cluster with this name exists")
	// ErrClusterInUse is returned by Remove while a machine is bound to the cluster.
	ErrClusterInUse = errors.New("clusters: machines are bound to this cluster")
	// ErrStale is returned by an approval of a pair that is no longer the pending pair.
	ErrStale = errors.New("clusters: the pair is no longer pending")
)

const (
	actionImported = "cluster.imported"
	actionSet      = "cluster.set"
	actionRemoved  = "cluster.removed"

	tableClusters = "clusters"
	tableVersions = "secret_versions"
	tableRenders  = "host_renders"

	// The sealed columns; each is part of its value's additional data.
	columnCredSealed  = "credential_sealed"
	columnFetchError  = "fetch_error_sealed"
	columnFailedError = "failed_error_sealed"
	columnLastCall    = "last_call_error_sealed"
	columnValues      = "values_sealed"
	columnRedacted    = "redacted_sealed"
	columnDriftRedact = "drift_redacted_sealed"

	// The columns findCluster looks a cluster up by.
	columnID   = "id"
	columnName = "name"

	interruptedMessage = "interrupted"
	maxNameLength      = 63
)

// Cluster is a stored cluster: the bun model of the clusters table. Nullable text and times are the zero value
// when NULL.
type Cluster struct {
	bun.BaseModel `bun:"table:clusters"`

	ID            string `bun:"id,pk"`
	Name          string `bun:"name"`
	RepoURL       string `bun:"repo_url"`
	Ref           string `bun:"ref"`
	Subpath       string `bun:"subpath"`
	Username      string `bun:"username"`
	CredentialRef string `bun:"credential_ref"`
	// Revision counts the changes of the source; work on a tree is stored only while it still holds the value
	// the work began at.
	Revision     int64     `bun:"revision"`
	LastFetchAt  time.Time `bun:"last_fetch_at,nullzero"`
	LastHeadTree string    `bun:"last_head_tree,nullzero"`
	// ProcessingTree and ProcessingRevision are the processing mark: both are set, or neither. The revision is a
	// pointer because revision 0, a fresh cluster's, is a real mark, which nullzero would write as NULL.
	ProcessingTree     string    `bun:"processing_tree,nullzero"`
	ProcessingRevision *int64    `bun:"processing_revision"`
	FailedTree         string    `bun:"failed_tree,nullzero"`
	ApprovedPairID     string    `bun:"approved_pair_id,nullzero"`
	PendingPairID      string    `bun:"pending_pair_id,nullzero"`
	RefreshRequested   bool      `bun:"refresh_requested"`
	RefreshIncomplete  bool      `bun:"refresh_incomplete"`
	LastRefreshAt      time.Time `bun:"last_refresh_at,nullzero"`
	LastDailyAt        time.Time `bun:"last_daily_at,nullzero"`
	Attempts           int       `bun:"attempts"`
	NextAttemptAt      time.Time `bun:"next_attempt_at,nullzero"`
	RateLimitedUntil   time.Time `bun:"rate_limited_until,nullzero"`
	// CreatedAt is when the cluster was imported; UpdatedAt when its definition, a sealed credential included,
	// last changed.
	CreatedAt time.Time `bun:"created_at"`
	UpdatedAt time.Time `bun:"updated_at"`

	// The sealed columns; Errors and Credential open them.
	CredentialSealed    []byte `bun:"credential_sealed,nullzero"`
	FetchErrorSealed    []byte `bun:"fetch_error_sealed,nullzero"`
	FailedErrorSealed   []byte `bun:"failed_error_sealed,nullzero"`
	LastCallErrorSealed []byte `bun:"last_call_error_sealed,nullzero"`
}

// utc puts the cluster's times in UTC.
func (c Cluster) utc() Cluster {
	c.LastFetchAt, c.LastRefreshAt, c.LastDailyAt = c.LastFetchAt.UTC(), c.LastRefreshAt.UTC(), c.LastDailyAt.UTC()
	c.NextAttemptAt, c.RateLimitedUntil = c.NextAttemptAt.UTC(), c.RateLimitedUntil.UTC()
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c
}

// Pair is a stored pair of a cluster: a tree and the secret version it was rendered with. A pair is pending until
// an admin approves it.
type Pair struct {
	bun.BaseModel `bun:"table:cluster_pairs"`

	ID              string    `bun:"id,pk"`
	ClusterID       string    `bun:"cluster_id"`
	TreeHash        string    `bun:"tree_hash"`
	SecretVersionID string    `bun:"secret_version_id"`
	TalosVersion    string    `bun:"talos_version"`
	CreatedAt       time.Time `bun:"created_at"`
	ApprovedAt      time.Time `bun:"approved_at,nullzero"`
	ApprovedBy      string    `bun:"approved_by,nullzero"`
}

func (p Pair) utc() Pair {
	p.CreatedAt, p.ApprovedAt = p.CreatedAt.UTC(), p.ApprovedAt.UTC()
	return p
}

// versionRow is the secret_versions table; values_sealed is the JSON of the value set, sealed.
type versionRow struct {
	bun.BaseModel `bun:"table:secret_versions"`

	ID           string    `bun:"id,pk"`
	ClusterID    string    `bun:"cluster_id"`
	ValuesSealed []byte    `bun:"values_sealed"`
	CreatedAt    time.Time `bun:"created_at"`
}

// hostRenderRow is the host_renders table.
type hostRenderRow struct {
	bun.BaseModel `bun:"table:host_renders"`

	PairID              string `bun:"pair_id,pk"`
	HostName            string `bun:"host_name,pk"`
	Role                string `bun:"role"`
	IP                  string `bun:"ip"`
	ConfigHash          string `bun:"config_hash"`
	DisplayHash         string `bun:"display_hash"`
	RedactedSealed      []byte `bun:"redacted_sealed"`
	SchematicID         string `bun:"schematic_id"`
	SchematicBody       []byte `bun:"schematic_body,nullzero"`
	TalosVersion        string `bun:"talos_version"`
	Drift               bool   `bun:"drift"`
	DriftConfigHash     string `bun:"drift_config_hash,nullzero"`
	DriftDisplayHash    string `bun:"drift_display_hash,nullzero"`
	DriftRedactedSealed []byte `bun:"drift_redacted_sealed,nullzero"`
}

// nameDetail is the detail of a cluster.imported and a cluster.removed audit event.
type nameDetail struct {
	Name string `json:"name"`
}

// setDetail is the detail of a cluster.set audit event: the fields the set changed.
type setDetail struct {
	Name    string   `json:"name"`
	Changed []string `json:"changed,omitempty"`
}

// stamp is how every time is written: UTC, to the microsecond Postgres keeps.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// Repository stores clusters, their pairs, secret versions and renders. Every value that can hold secret
// content is sealed with the data key before it is written.
type Repository struct {
	bdb *bun.DB
	key *datakey.Key
}

// NewRepository returns a Repository over d that seals with key.
func NewRepository(d *db.DB, key *datakey.Key) *Repository {
	return &Repository{bdb: d.Bun(), key: key}
}

// ImportRequest defines a cluster to import. Name is a DNS label, RepoURL an https URL without userinfo, and
// CredentialRef empty or a plain ref+op:// reference.
type ImportRequest struct {
	Name, RepoURL, Ref, Subpath, Username, CredentialRef string
	At                                                   time.Time
}

// Import stores a new cluster at revision 0 and audits it as cluster.imported. A name some cluster holds is
// ErrExists; an invalid name, URL or credential reference is an error that quotes nothing a URL could carry.
func (r *Repository) Import(ctx context.Context, req ImportRequest) (Cluster, error) {
	if err := validName(req.Name); err != nil {
		return Cluster{}, err
	}
	if err := validRepoURL(req.RepoURL); err != nil {
		return Cluster{}, err
	}
	if err := validCredentialRef(req.CredentialRef); err != nil {
		return Cluster{}, err
	}

	at := stamp(req.At)
	c := Cluster{
		ID: rand.Text(), Name: req.Name, RepoURL: req.RepoURL, Ref: req.Ref, Subpath: req.Subpath,
		Username: req.Username, CredentialRef: req.CredentialRef, CreatedAt: at, UpdatedAt: at,
	}
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&c).Exec(ctx); err != nil {
			if db.IsUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrExists, req.Name)
			}
			return fmt.Errorf("insert cluster: %w", err)
		}
		return fleet.InsertAudit(ctx, tx, fleet.AuditEvent{
			At: at, Actor: fleet.ActorCLI, Action: actionImported, Detail: nameDetail{Name: c.Name},
		})
	})
	if err != nil {
		return Cluster{}, fmt.Errorf("clusters: import %s: %w", req.Name, err)
	}
	return c, nil
}

// SetRequest changes the fields of cluster Name whose pointer is non-nil.
type SetRequest struct {
	Name                                           string
	RepoURL, Ref, Subpath, Username, CredentialRef *string
	At                                             time.Time
}

// setField is one field of the definition: where it is stored, the value a SetRequest gives it, and what a change
// of it invalidates.
type setField struct {
	column     string
	dst        *string
	val        *string
	source     bool
	credential bool
}

// validateSet refuses the repo URL and the credential reference a set would store when they are not valid.
func validateSet(req SetRequest) error {
	if req.RepoURL != nil {
		if err := validRepoURL(*req.RepoURL); err != nil {
			return err
		}
	}
	if req.CredentialRef != nil {
		return validCredentialRef(*req.CredentialRef)
	}
	return nil
}

// applySet writes the fields req changes onto c. It returns the changed columns, whether any of them is the source of
// the trees (repo URL, ref, path) and whether the credential reference is one.
func applySet(c *Cluster, req SetRequest) (changed []string, source, credential bool) {
	for _, f := range []setField{
		{column: "repo_url", dst: &c.RepoURL, val: req.RepoURL, source: true},
		{column: "ref", dst: &c.Ref, val: req.Ref, source: true},
		{column: "subpath", dst: &c.Subpath, val: req.Subpath, source: true},
		{column: "username", dst: &c.Username, val: req.Username},
		{column: "credential_ref", dst: &c.CredentialRef, val: req.CredentialRef, credential: true},
	} {
		if f.val == nil || *f.val == *f.dst {
			continue
		}
		*f.dst = *f.val
		changed = append(changed, f.column)
		source, credential = source || f.source, credential || f.credential
	}
	return changed, source, credential
}

// Set changes the fields of req and adds one to the revision, which clears the processing mark: work marked at the
// old revision can no longer be stored, so the mark has no owner. A changed repo URL, ref or path also clears the
// pending pair (deleting it with its renders and version), the failed tree and the last head tree, so the new
// source's head is processed even if it is the same tree; the approved pair is kept. A changed credential
// reference clears the sealed credential. It audits cluster.set. An unknown name is ErrNotFound.
func (r *Repository) Set(ctx context.Context, req SetRequest) (Cluster, error) {
	if err := validateSet(req); err != nil {
		return Cluster{}, err
	}

	at := stamp(req.At)
	var out Cluster
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnName, req.Name, true)
		if err != nil {
			return err
		}
		changed, source, credential := applySet(&c, req)

		q := tx.NewUpdate().Model((*Cluster)(nil)).
			Set("repo_url = ?", c.RepoURL).Set("ref = ?", c.Ref).Set("subpath = ?", c.Subpath).
			Set("username = ?", c.Username).Set("credential_ref = ?", c.CredentialRef).
			Set("revision = ?", c.Revision+1).Set("updated_at = ?", at).
			Where("id = ?", c.ID).Where("revision = ?", c.Revision)
		if credential {
			q = q.Set("credential_sealed = NULL")
		}
		q = clearMark(q)
		if source {
			q = q.Set("pending_pair_id = NULL").Set("failed_tree = NULL").Set("failed_error_sealed = NULL").Set("last_head_tree = NULL")
		}
		if err := updateOne(ctx, q, db.ErrConflict); err != nil {
			return err
		}
		if source && c.PendingPairID != "" {
			if err := deletePair(ctx, tx, c.PendingPairID); err != nil {
				return err
			}
		}
		ev := fleet.AuditEvent{At: at, Actor: fleet.ActorCLI, Action: actionSet, Detail: setDetail{Name: c.Name, Changed: changed}}
		if err := fleet.InsertAudit(ctx, tx, ev); err != nil {
			return err
		}
		out, err = findCluster(ctx, tx, columnID, c.ID, false)
		return err
	})
	if err != nil {
		return Cluster{}, fmt.Errorf("clusters: set %s: %w", req.Name, err)
	}
	return out, nil
}

// Remove deletes the cluster with its pairs, versions and renders, and audits it as cluster.removed by actor. It
// is ErrClusterInUse while a machine is bound to the cluster, and ErrNotFound for an unknown name.
func (r *Repository) Remove(ctx context.Context, name, actor string, at time.Time) error {
	at = stamp(at)
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnName, name, true)
		if err != nil {
			return err
		}
		bound, err := tx.NewSelect().Model((*fleet.Machine)(nil)).Where("cluster_id = ?", c.ID).Exists(ctx)
		if err != nil {
			return fmt.Errorf("check machines bound to %s: %w", name, err)
		}
		if bound {
			return ErrClusterInUse
		}
		// The pairs, versions and renders go with the row (ON DELETE CASCADE).
		if _, err := tx.NewDelete().Model((*Cluster)(nil)).Where("id = ?", c.ID).Exec(ctx); err != nil {
			return fmt.Errorf("delete cluster: %w", err)
		}
		return fleet.InsertAudit(ctx, tx, fleet.AuditEvent{At: at, Actor: actor, Action: actionRemoved, Detail: nameDetail{Name: name}})
	})
	if err != nil {
		return fmt.Errorf("clusters: remove %s: %w", name, err)
	}
	return nil
}

// Get returns the cluster with the name, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, name string) (Cluster, error) {
	c, err := findCluster(ctx, r.bdb, columnName, name, false)
	if err != nil {
		return Cluster{}, fmt.Errorf("clusters: get %s: %w", name, err)
	}
	return c, nil
}

// List returns every cluster, ordered by name.
func (r *Repository) List(ctx context.Context) ([]Cluster, error) {
	var out []Cluster
	if err := r.bdb.NewSelect().Model(&out).Order("name").Scan(ctx); err != nil {
		return nil, fmt.Errorf("clusters: list: %w", err)
	}
	for i := range out {
		out[i] = out[i].utc()
	}
	return out, nil
}

// RequestRefresh asks for a refresh of the cluster: the request flag is set and the backoff and rate limit are
// cleared, so serve acts at its next pick-up. An unknown name is ErrNotFound.
func (r *Repository) RequestRefresh(ctx context.Context, name string, _ time.Time) error {
	q := r.bdb.NewUpdate().Model((*Cluster)(nil)).
		Set("refresh_requested = ?", true).Set("attempts = 0").Set("next_attempt_at = NULL").Set("rate_limited_until = NULL").
		Where("name = ?", name)
	if err := updateOne(ctx, q, ErrNotFound); err != nil {
		return fmt.Errorf("clusters: request refresh of %s: %w", name, err)
	}
	return nil
}

// Trigger is what started a piece of work on a tree.
type Trigger string

// The triggers of work on a tree.
const (
	// TriggerHead is a new head tree.
	TriggerHead Trigger = "head"
	// TriggerRefresh is a refresh request; marking it consumes the request flag.
	TriggerRefresh Trigger = "refresh"
	// TriggerDaily is the daily timer; marking it stamps last_daily_at.
	TriggerDaily Trigger = "daily"
	// TriggerContinue is the continuation of work an earlier call left incomplete.
	TriggerContinue Trigger = "continue"
	// TriggerCredential is the first resolution of a credential.
	TriggerCredential Trigger = "credential" //nolint:gosec // G101: the name of a trigger, not a credential
)

// MarkRequest records Tree as processing on cluster ClusterID, which must still be at Revision.
type MarkRequest struct {
	ClusterID, Tree string
	Revision        int64
	Trigger         Trigger
	At              time.Time
}

// MarkProcessing records, in one write, the tree as processing at the revision and the consumption of the
// trigger: a refresh clears the request flag, the daily timer stamps last_daily_at, and the other triggers
// consume nothing. It writes only while the cluster is at the revision and carries no mark, and reports whether
// it did.
func (r *Repository) MarkProcessing(ctx context.Context, req MarkRequest) (bool, error) {
	if req.Tree == "" {
		return false, errors.New("clusters: mark processing: no tree")
	}
	q := r.bdb.NewUpdate().Model((*Cluster)(nil)).
		Set("processing_tree = ?", req.Tree).Set("processing_revision = ?", req.Revision).
		Where("id = ?", req.ClusterID).Where("revision = ?", req.Revision).Where("processing_tree IS NULL")
	switch req.Trigger {
	case TriggerRefresh:
		q = q.Set("refresh_requested = ?", false)
	case TriggerDaily:
		q = q.Set("last_daily_at = ?", stamp(req.At))
	case TriggerHead, TriggerContinue, TriggerCredential:
	default:
		return false, fmt.Errorf("clusters: mark processing: unknown trigger %q", req.Trigger)
	}
	ok, err := applied(ctx, q)
	if err != nil {
		return false, fmt.Errorf("clusters: mark %s processing: %w", req.Tree, err)
	}
	return ok, nil
}

// ClearProcessing clears the processing mark without recording the tree as processed, so the next poll processes
// it again: the graceful shutdown of work in flight.
func (r *Repository) ClearProcessing(ctx context.Context, clusterID string) error {
	q := clearMark(r.bdb.NewUpdate().Model((*Cluster)(nil)).Where("id = ?", clusterID))
	if err := updateOne(ctx, q, ErrNotFound); err != nil {
		return fmt.Errorf("clusters: clear processing mark of %s: %w", clusterID, err)
	}
	return nil
}

// ConvertInterrupted turns the processing mark of every cluster that still has one, left by a process that died
// mid-work, into the failed tree with the error "interrupted", records the tree as the last head tree processed
// and clears the mark. It returns how many clusters it converted.
func (r *Repository) ConvertInterrupted(ctx context.Context, _ time.Time) (int, error) {
	var n int
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		n = 0
		var marked []Cluster
		sel := tx.NewSelect().Model(&marked).Where("processing_tree IS NOT NULL").Order("id")
		if tx.Dialect().Name() == dialect.PG {
			sel = sel.For("UPDATE")
		}
		if err := sel.Scan(ctx); err != nil {
			return fmt.Errorf("read marked clusters: %w", err)
		}
		for i := range marked {
			c := &marked[i]
			q := clearMark(tx.NewUpdate().Model((*Cluster)(nil)).
				Set("failed_tree = ?", c.ProcessingTree).
				Set("failed_error_sealed = ?", r.seal(clusterField(c.ID, columnFailedError), interruptedMessage)).
				Set("last_head_tree = ?", c.ProcessingTree).
				Where("id = ?", c.ID).Where("processing_tree = ?", c.ProcessingTree))
			if err := updateOne(ctx, q, db.ErrConflict); err != nil {
				return fmt.Errorf("convert the mark of %s: %w", c.Name, err)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("clusters: convert interrupted work: %w", err)
	}
	return n, nil
}

// markKey names the processing mark a result is stored under.
type markKey struct {
	ClusterID, Tree string
	Revision        int64
}

// underMark runs fn in a transaction that holds the cluster row, when the cluster is still at the revision with
// the tree marked processing, and reports whether it ran. A cluster that is gone, a changed revision and a
// cleared or different mark all mean the work is stale: fn does not run and nothing is written.
func (r *Repository) underMark(ctx context.Context, k markKey, fn func(ctx context.Context, tx bun.Tx, c Cluster) error) (bool, error) {
	var ran bool
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		ran = false
		c, err := findCluster(ctx, tx, columnID, k.ClusterID, true)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if c.Revision != k.Revision || c.ProcessingRevision == nil || *c.ProcessingRevision != k.Revision || c.ProcessingTree != k.Tree {
			return nil
		}
		ran = true
		return fn(ctx, tx, c)
	})
	if err != nil {
		return false, err
	}
	return ran, nil
}

// forMark limits q to the cluster row that still carries the mark.
func forMark(q *bun.UpdateQuery, k markKey) *bun.UpdateQuery {
	return q.Where("id = ?", k.ClusterID).Where("revision = ?", k.Revision).Where("processing_tree = ?", k.Tree)
}

// clearMark sets the processing mark of q's rows to none.
func clearMark(q *bun.UpdateQuery) *bun.UpdateQuery {
	return q.Set("processing_tree = NULL").Set("processing_revision = NULL")
}

// StorePendingRequest is the render of Tree, to store as the cluster's pending pair: the resolved Values and the
// Renders of its hosts. Refresh is true when a refresh produced it rather than a head tree.
type StorePendingRequest struct {
	ClusterID, Tree string
	Revision        int64
	Values          map[string]string
	TalosVersion    string
	Renders         []render.HostRender
	Refresh         bool
	At              time.Time
}

// StorePending stores the secret version, the pair and the renders in one transaction, deletes the older pending
// pair (its renders, then the pair, then its version), makes the new pair the pending one and clears the
// processing mark. It stores only while the cluster is at Revision with Tree marked processing; otherwise it
// writes nothing and reports stored false, as it does for a cluster that has been removed. A head tree also
// becomes the last head tree processed and clears the failed tree. A Refresh leaves the last head tree alone,
// clears the failed tree only when it is Tree, and sets last_refresh_at to At and refresh_incomplete to false.
func (r *Repository) StorePending(ctx context.Context, req StorePendingRequest) (pairID string, stored bool, err error) {
	at := stamp(req.At)
	key := markKey{ClusterID: req.ClusterID, Tree: req.Tree, Revision: req.Revision}
	stored, err = r.underMark(ctx, key, func(ctx context.Context, tx bun.Tx, c Cluster) error {
		id, err := r.insertPair(ctx, tx, req, at)
		if err != nil {
			return err
		}
		pairID = id
		if c.PendingPairID != "" {
			if err := deletePair(ctx, tx, c.PendingPairID); err != nil {
				return err
			}
		}
		q := clearMark(tx.NewUpdate().Model((*Cluster)(nil)).Set("pending_pair_id = ?", id))
		if req.Refresh {
			q = q.Set("last_refresh_at = ?", at).Set("refresh_incomplete = ?", false)
		} else {
			q = q.Set("last_head_tree = ?", req.Tree)
		}
		if !req.Refresh || c.FailedTree == req.Tree {
			q = q.Set("failed_tree = NULL").Set("failed_error_sealed = NULL")
		}
		return updateOne(ctx, forMark(q, key), db.ErrConflict)
	})
	if err != nil {
		return "", false, fmt.Errorf("clusters: store pending pair of %s: %w", req.Tree, err)
	}
	if !stored {
		return "", false, nil
	}
	return pairID, true, nil
}

// insertPair inserts the version, then the pair that references it, then the renders, and returns the pair id.
func (r *Repository) insertPair(ctx context.Context, q bun.IDB, req StorePendingRequest, at time.Time) (string, error) {
	values, err := json.Marshal(req.Values)
	if err != nil {
		return "", fmt.Errorf("encode secret values: %w", err)
	}
	versionID, pairID := rand.Text(), rand.Text()
	version := versionRow{
		ID: versionID, ClusterID: req.ClusterID, CreatedAt: at,
		ValuesSealed: r.key.SealField(datakey.Field{Table: tableVersions, Row: versionID, Column: columnValues}, values),
	}
	if _, err := q.NewInsert().Model(&version).Exec(ctx); err != nil {
		return "", fmt.Errorf("insert secret version: %w", err)
	}
	pair := Pair{ID: pairID, ClusterID: req.ClusterID, TreeHash: req.Tree, SecretVersionID: versionID, TalosVersion: req.TalosVersion, CreatedAt: at}
	if _, err := q.NewInsert().Model(&pair).Exec(ctx); err != nil {
		return "", fmt.Errorf("insert pair: %w", err)
	}
	if len(req.Renders) == 0 {
		return pairID, nil
	}
	rows := make([]hostRenderRow, len(req.Renders))
	for i := range req.Renders {
		h := &req.Renders[i]
		rows[i] = hostRenderRow{
			PairID: pairID, HostName: h.Host, Role: h.Role, IP: h.IP, ConfigHash: h.ConfigHash, DisplayHash: h.DisplayHash,
			RedactedSealed: r.key.SealField(renderField(pairID, h.Host, columnRedacted), h.Redacted),
			SchematicID:    h.SchematicID, SchematicBody: h.SchematicBody, TalosVersion: h.TalosVersion,
		}
	}
	if _, err := q.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return "", fmt.Errorf("insert host renders: %w", err)
	}
	return pairID, nil
}

// deletePair deletes a pair as the lock order requires: its renders, then the pair, then its secret version. A
// pair that is already gone leaves nothing to delete.
func deletePair(ctx context.Context, q bun.IDB, pairID string) error {
	var versionID string
	err := q.NewSelect().Model((*Pair)(nil)).Column("secret_version_id").Where("id = ?", pairID).Scan(ctx, &versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pair %s: %w", pairID, err)
	}
	if _, err := q.NewDelete().Model((*hostRenderRow)(nil)).Where("pair_id = ?", pairID).Exec(ctx); err != nil {
		return fmt.Errorf("delete renders of pair %s: %w", pairID, err)
	}
	if _, err := q.NewDelete().Model((*Pair)(nil)).Where("id = ?", pairID).Exec(ctx); err != nil {
		return fmt.Errorf("delete pair %s: %w", pairID, err)
	}
	if _, err := q.NewDelete().Model((*versionRow)(nil)).Where("id = ?", versionID).Exec(ctx); err != nil {
		return fmt.Errorf("delete secret version %s: %w", versionID, err)
	}
	return nil
}

// StoreFailedRequest records that Tree could not be turned into a pair, with the Error text that may quote a
// secret value.
type StoreFailedRequest struct {
	ClusterID, Tree string
	Revision        int64
	Error           string
	At              time.Time
}

// StoreFailed records the tree as the failed tree with its error sealed, records it as the last head tree
// processed and clears the processing mark, in one transaction, while the cluster is at the revision with the tree
// marked processing. Otherwise it writes nothing and reports false.
func (r *Repository) StoreFailed(ctx context.Context, req StoreFailedRequest) (bool, error) {
	key := markKey{ClusterID: req.ClusterID, Tree: req.Tree, Revision: req.Revision}
	stored, err := r.underMark(ctx, key, func(ctx context.Context, tx bun.Tx, c Cluster) error {
		q := clearMark(tx.NewUpdate().Model((*Cluster)(nil)).
			Set("failed_tree = ?", req.Tree).
			Set("failed_error_sealed = ?", r.seal(clusterField(c.ID, columnFailedError), req.Error)).
			Set("last_head_tree = ?", req.Tree))
		return updateOne(ctx, forMark(q, key), db.ErrConflict)
	})
	if err != nil {
		return false, fmt.Errorf("clusters: store failed tree %s: %w", req.Tree, err)
	}
	return stored, nil
}

// RefreshRecord is the outcome of a refresh that stored no pair.
type RefreshRecord struct {
	ClusterID, Tree string
	Revision        int64
	// Incomplete is true when a reference failed and the refresh could not finish.
	Incomplete bool
	At         time.Time
}

// RecordRefresh records a refresh that stores no pair and clears the processing mark, while the cluster is at the
// revision with the tree marked processing; otherwise it writes nothing and reports false. An incomplete refresh
// sets refresh_incomplete. A complete one sets last_refresh_at to At, clears refresh_incomplete and resets the
// attempts.
func (r *Repository) RecordRefresh(ctx context.Context, rec RefreshRecord) (bool, error) {
	key := markKey{ClusterID: rec.ClusterID, Tree: rec.Tree, Revision: rec.Revision}
	at := stamp(rec.At)
	recorded, err := r.underMark(ctx, key, func(ctx context.Context, tx bun.Tx, _ Cluster) error {
		q := clearMark(tx.NewUpdate().Model((*Cluster)(nil)))
		if rec.Incomplete {
			q = q.Set("refresh_incomplete = ?", true)
		} else {
			q = resetAttempts(q.Set("last_refresh_at = ?", at).Set("refresh_incomplete = ?", false))
		}
		return updateOne(ctx, forMark(q, key), db.ErrConflict)
	})
	if err != nil {
		return false, fmt.Errorf("clusters: record refresh of %s: %w", rec.Tree, err)
	}
	return recorded, nil
}

// ClusterErrors are the cluster's stored error texts, opened; each is "" when there is none. Each can quote a
// secret value: show them to an admin, never log them.
type ClusterErrors struct{ Fetch, Failed, LastCall string }

// Errors opens the cluster's three sealed error texts. A text that does not open is an error matching
// datakey.ErrDecrypt.
func (r *Repository) Errors(ctx context.Context, clusterID string) (ClusterErrors, error) {
	var c Cluster
	err := r.bdb.NewSelect().Model(&c).Column(columnFetchError, columnFailedError, columnLastCall).Where("id = ?", clusterID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ClusterErrors{}, fmt.Errorf("clusters: errors of %s: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return ClusterErrors{}, fmt.Errorf("clusters: read errors of %s: %w", clusterID, err)
	}
	var out ClusterErrors
	for _, f := range []struct {
		dst    *string
		column string
		sealed []byte
	}{
		{&out.Fetch, columnFetchError, c.FetchErrorSealed},
		{&out.Failed, columnFailedError, c.FailedErrorSealed},
		{&out.LastCall, columnLastCall, c.LastCallErrorSealed},
	} {
		if f.sealed == nil {
			continue
		}
		text, err := r.key.OpenField(clusterField(clusterID, f.column), f.sealed)
		if err != nil {
			return ClusterErrors{}, fmt.Errorf("clusters: open %s of %s: %w", f.column, clusterID, err)
		}
		*f.dst = string(text)
	}
	return out, nil
}

// RecordFetch records a fetch at at: its time, and its error sealed, or no error when fetchErr is nil.
func (r *Repository) RecordFetch(ctx context.Context, clusterID string, fetchErr error, at time.Time) error {
	q := r.bdb.NewUpdate().Model((*Cluster)(nil)).Set("last_fetch_at = ?", stamp(at)).Where("id = ?", clusterID)
	if fetchErr == nil {
		q = q.Set("fetch_error_sealed = NULL")
	} else {
		q = q.Set("fetch_error_sealed = ?", r.seal(clusterField(clusterID, columnFetchError), fetchErr.Error()))
	}
	if err := updateOne(ctx, q, ErrNotFound); err != nil {
		return fmt.Errorf("clusters: record fetch of %s: %w", clusterID, err)
	}
	return nil
}

// WholeCallFailure is a call that failed as a whole: transport, token or client creation. Tree is the tree whose
// mark the work held, "" when it held none. Trigger is what started the work. NextDaily is when the next daily
// trigger runs; a rate-limited failure waits for it. Error can quote a secret value.
type WholeCallFailure struct {
	ClusterID, Tree string
	Trigger         Trigger
	RateLimited     bool
	NextDaily       time.Time
	Error           string
	At              time.Time
}

// RecordWholeCallFailure counts the failure, schedules the next attempt at At plus fleet.Backoff of the new count
// and stores the error sealed. A rate-limited failure also holds every retry until NextDaily. A processing mark on
// Tree is cleared without recording the tree as processed. A refresh, daily or continue trigger sets the refresh
// request again, so the work is retried after the backoff.
func (r *Repository) RecordWholeCallFailure(ctx context.Context, f WholeCallFailure) error {
	if f.RateLimited && f.NextDaily.IsZero() {
		return errors.New("clusters: record whole-call failure: a rate limit needs the next daily time")
	}
	at := stamp(f.At)
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnID, f.ClusterID, true)
		if err != nil {
			return err
		}
		attempts := c.Attempts + 1
		q := tx.NewUpdate().Model((*Cluster)(nil)).
			Set("attempts = ?", attempts).Set("next_attempt_at = ?", at.Add(fleet.Backoff(attempts))).
			Set("last_call_error_sealed = ?", r.seal(clusterField(c.ID, columnLastCall), f.Error)).
			Where("id = ?", c.ID).Where("attempts = ?", c.Attempts)
		if f.RateLimited {
			q = q.Set("rate_limited_until = ?", stamp(f.NextDaily))
		}
		if f.Tree != "" && c.ProcessingTree == f.Tree {
			q = clearMark(q)
		}
		switch f.Trigger {
		case TriggerRefresh, TriggerDaily, TriggerContinue:
			q = q.Set("refresh_requested = ?", true)
		case TriggerHead, TriggerCredential:
		default:
			return fmt.Errorf("unknown trigger %q", f.Trigger)
		}
		return updateOne(ctx, q, db.ErrConflict)
	})
	if err != nil {
		return fmt.Errorf("clusters: record whole-call failure of %s: %w", f.ClusterID, err)
	}
	return nil
}

// ResetAttempts clears the attempt count, the next attempt time and the last call error.
func (r *Repository) ResetAttempts(ctx context.Context, clusterID string) error {
	q := resetAttempts(r.bdb.NewUpdate().Model((*Cluster)(nil)).Where("id = ?", clusterID))
	if err := updateOne(ctx, q, ErrNotFound); err != nil {
		return fmt.Errorf("clusters: reset attempts of %s: %w", clusterID, err)
	}
	return nil
}

// resetAttempts sets the backoff state of q's rows to none.
func resetAttempts(q *bun.UpdateQuery) *bun.UpdateQuery {
	return q.Set("attempts = 0").Set("next_attempt_at = NULL").Set("last_call_error_sealed = NULL")
}

// SetCredential seals the resolved read credential on the cluster. An unknown cluster is ErrNotFound.
func (r *Repository) SetCredential(ctx context.Context, clusterID string, value secret.Secret, at time.Time) error {
	q := r.bdb.NewUpdate().Model((*Cluster)(nil)).
		Set("credential_sealed = ?", r.seal(clusterField(clusterID, columnCredSealed), value.Expose())).
		Set("updated_at = ?", stamp(at)).Where("id = ?", clusterID)
	if err := updateOne(ctx, q, ErrNotFound); err != nil {
		return fmt.Errorf("clusters: set credential of %s: %w", clusterID, err)
	}
	return nil
}

// Credential opens the cluster's sealed read credential; ok is false when none is stored. An unknown cluster is
// ErrNotFound.
func (r *Repository) Credential(ctx context.Context, clusterID string) (value secret.Secret, ok bool, err error) {
	var c Cluster
	err = r.bdb.NewSelect().Model(&c).Column(columnCredSealed).Where("id = ?", clusterID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("clusters: credential of %s: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return "", false, fmt.Errorf("clusters: read credential of %s: %w", clusterID, err)
	}
	if c.CredentialSealed == nil {
		return "", false, nil
	}
	plain, err := r.key.OpenField(clusterField(clusterID, columnCredSealed), c.CredentialSealed)
	if err != nil {
		return "", false, fmt.Errorf("clusters: open credential of %s: %w", clusterID, err)
	}
	return secret.Secret(plain), true, nil
}

// Pair returns the pair with the id, or ErrNotFound.
func (r *Repository) Pair(ctx context.Context, id string) (Pair, error) {
	var p Pair
	err := r.bdb.NewSelect().Model(&p).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Pair{}, fmt.Errorf("clusters: pair %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Pair{}, fmt.Errorf("clusters: read pair %s: %w", id, err)
	}
	return p.utc(), nil
}

// Values opens the sealed value set of a secret version: reference to resolved value. An unknown version is
// ErrNotFound; a row that does not open, or does not hold a value set, is an error matching datakey.ErrDecrypt.
func (r *Repository) Values(ctx context.Context, versionID string) (map[string]string, error) {
	var v versionRow
	err := r.bdb.NewSelect().Model(&v).Where("id = ?", versionID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("clusters: secret version %s: %w", versionID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("clusters: read secret version %s: %w", versionID, err)
	}
	plain, err := r.key.OpenField(datakey.Field{Table: tableVersions, Row: versionID, Column: columnValues}, v.ValuesSealed)
	if err != nil {
		return nil, fmt.Errorf("clusters: open secret version %s: %w", versionID, err)
	}
	var values map[string]string
	if err := json.Unmarshal(plain, &values); err != nil {
		// The decoder's text can quote a byte of the value set, so it is dropped.
		return nil, fmt.Errorf("clusters: secret version %s is not a value set: %w", versionID, datakey.ErrDecrypt)
	}
	return values, nil
}

// HostRow is the stored render of one host of a pair, with its redacted renders opened. Drift is true when the
// host's running config differs from the render, and the Drift fields then describe the running config.
type HostRow struct {
	PairID, Host, Role, IP, ConfigHash, DisplayHash string
	Redacted                                        []byte
	SchematicID                                     string
	SchematicBody                                   []byte
	TalosVersion                                    string
	Drift                                           bool
	DriftConfigHash, DriftDisplayHash               string
	DriftRedacted                                   []byte
}

// HostRows returns the renders of a pair's hosts, ordered by host. A row that does not open is an error matching
// datakey.ErrDecrypt.
func (r *Repository) HostRows(ctx context.Context, pairID string) ([]HostRow, error) {
	var rows []hostRenderRow
	if err := r.bdb.NewSelect().Model(&rows).Where("pair_id = ?", pairID).Order("host_name").Scan(ctx); err != nil {
		return nil, fmt.Errorf("clusters: read renders of pair %s: %w", pairID, err)
	}
	out := make([]HostRow, len(rows))
	for i := range rows {
		row := &rows[i]
		redacted, err := r.key.OpenField(renderField(pairID, row.HostName, columnRedacted), row.RedactedSealed)
		if err != nil {
			return nil, fmt.Errorf("clusters: open render of %s/%s: %w", pairID, row.HostName, err)
		}
		out[i] = HostRow{
			PairID: pairID, Host: row.HostName, Role: row.Role, IP: row.IP, ConfigHash: row.ConfigHash, DisplayHash: row.DisplayHash,
			Redacted: redacted, SchematicID: row.SchematicID, SchematicBody: row.SchematicBody, TalosVersion: row.TalosVersion,
			Drift: row.Drift, DriftConfigHash: row.DriftConfigHash, DriftDisplayHash: row.DriftDisplayHash,
		}
		if row.DriftRedactedSealed != nil {
			out[i].DriftRedacted, err = r.key.OpenField(renderField(pairID, row.HostName, columnDriftRedact), row.DriftRedactedSealed)
			if err != nil {
				return nil, fmt.Errorf("clusters: open drift render of %s/%s: %w", pairID, row.HostName, err)
			}
		}
	}
	return out, nil
}

// DriftRender is the render of a host's running config: its hashes and its redacted text.
type DriftRender struct {
	ConfigHash, DisplayHash string
	Redacted                []byte
}

// SetDriftRequest records Render as the drift of Host in a pair, found by reading the host's render whose config
// hash was ReadHash.
type SetDriftRequest struct {
	PairID, Host, ReadHash string
	Render                 DriftRender
}

// SetDrift marks the host drifted, with the drift render's hashes and its sealed redacted text, while the host's
// render still has the config hash the caller read. It reports whether it wrote; a different pair, an unknown host
// or a stale read hash writes nothing.
func (r *Repository) SetDrift(ctx context.Context, req SetDriftRequest) (bool, error) {
	q := r.bdb.NewUpdate().Model((*hostRenderRow)(nil)).
		Set("drift = ?", true).Set("drift_config_hash = ?", req.Render.ConfigHash).Set("drift_display_hash = ?", req.Render.DisplayHash).
		Set("drift_redacted_sealed = ?", r.key.SealField(renderField(req.PairID, req.Host, columnDriftRedact), req.Render.Redacted)).
		Where("pair_id = ?", req.PairID).Where("host_name = ?", req.Host).Where("config_hash = ?", req.ReadHash)
	ok, err := applied(ctx, q)
	if err != nil {
		return false, fmt.Errorf("clusters: set drift of %s/%s: %w", req.PairID, req.Host, err)
	}
	return ok, nil
}

// seal seals text for the field.
func (r *Repository) seal(f datakey.Field, text string) []byte {
	return r.key.SealField(f, []byte(text))
}

func clusterField(id, column string) datakey.Field {
	return datakey.Field{Table: tableClusters, Row: id, Column: column}
}

// renderField is a sealed column of a host render, whose row id is "<pair id>/<host>".
func renderField(pairID, host, column string) datakey.Field {
	return datakey.Field{Table: tableRenders, Row: pairID + "/" + host, Column: column}
}

// findCluster reads the cluster whose column is value; with lock it also holds the row (FOR UPDATE on Postgres)
// until the caller's transaction ends, as every transaction of a cluster takes its row first. SQLite has one
// writer, so there is nothing to hold. An unknown cluster is ErrNotFound.
func findCluster(ctx context.Context, q bun.IDB, column, value string, lock bool) (Cluster, error) {
	var c Cluster
	sel := q.NewSelect().Model(&c).Where("? = ?", bun.Ident(column), value)
	if lock && q.Dialect().Name() == dialect.PG {
		sel = sel.For("UPDATE")
	}
	err := sel.Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("%w: cluster %s", ErrNotFound, value)
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("read cluster %s: %w", value, err)
	}
	return c.utc(), nil
}

// applied runs q and reports whether it changed a row.
func applied(ctx context.Context, q *bun.UpdateQuery) (bool, error) {
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// updateOne runs q and returns none when it matched no row.
func updateOne(ctx context.Context, q *bun.UpdateQuery, none error) error {
	ok, err := applied(ctx, q)
	if err != nil {
		return err
	}
	if !ok {
		return none
	}
	return nil
}

// validName refuses a name that is not a DNS label: 1 to 63 lowercase letters, digits and '-', not starting or
// ending with '-'. A name has no '/', so <cluster>/<host> splits at its only '/'.
func validName(name string) error {
	if name == "" || len(name) > maxNameLength {
		return fmt.Errorf("cluster name %q: must be 1 to %d characters, got %d", name, maxNameLength, len(name))
	}
	for i := range len(name) {
		if !isNameByte(name[i], i > 0 && i < len(name)-1) {
			return fmt.Errorf("cluster name %q: must be a DNS label of lowercase letters, digits and '-', not starting or ending with '-'", name)
		}
	}
	return nil
}

// isNameByte reports whether c can be in a cluster name: a lowercase letter or a digit, or '-' when inner.
func isNameByte(c byte, inner bool) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && inner
}

// validRepoURL refuses a URL that is not https, has no host, or carries userinfo. Its errors never quote the
// URL, which can hold a password.
func validRepoURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("repo URL: not a valid URL")
	case u.Scheme != "https":
		return errors.New("repo URL: must be an https URL")
	case u.Host == "":
		return errors.New("repo URL: must name a host")
	case u.User != nil:
		return errors.New("repo URL: must not carry a user or password; give them as the username and the credential")
	}
	return nil
}

// validCredentialRef refuses a reference that is neither empty nor a plain ref+op:// reference. It does not quote
// the value, which may be a pasted token.
func validCredentialRef(ref string) error {
	if ref != "" && !render.IsReference(ref) {
		return errors.New("credential reference: must be empty or ref+op://VAULT/ITEM/[SECTION/]FIELD")
	}
	return nil
}
