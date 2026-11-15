package tlsacme

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
)

// Account is the stored ACME account of one directory: its private key
// (sealed) and the registration the server returned (JSON).
type Account struct {
	DirectoryURL string
	KeySealed    []byte
	Registration []byte
	CreatedAt    time.Time
}

// StoredCertificate is the certificate of one domain as stored: the ACME
// directory it came from, the chain and the private key sealed, and the
// validity window in clear so it can be inspected without the data key.
type StoredCertificate struct {
	Domain, DirectoryURL           string
	ChainSealed, KeySealed         []byte
	NotBefore, NotAfter, UpdatedAt time.Time
}

// accountRow is the acme_accounts table. Registration is TEXT holding JSON.
type accountRow struct {
	bun.BaseModel `bun:"table:acme_accounts"`

	DirectoryURL string    `bun:"directory_url,pk"`
	KeySealed    []byte    `bun:"key_sealed"`
	Registration string    `bun:"registration"`
	CreatedAt    time.Time `bun:"created_at"`
}

// certificateRow is the tls_certificates table.
type certificateRow struct {
	bun.BaseModel `bun:"table:tls_certificates"`

	Domain       string    `bun:"domain,pk"`
	DirectoryURL string    `bun:"directory_url"`
	ChainSealed  []byte    `bun:"chain_sealed"`
	KeySealed    []byte    `bun:"key_sealed"`
	NotBefore    time.Time `bun:"not_before"`
	NotAfter     time.Time `bun:"not_after"`
	UpdatedAt    time.Time `bun:"updated_at"`
}

// Repository stores the ACME account and the certificate.
type Repository struct {
	bdb *bun.DB
}

// NewRepository returns a Repository over d.
func NewRepository(d *db.DB) *Repository { return &Repository{bdb: d.Bun()} }

// SaveAccount stores a, replacing the account of the same directory.
func (r *Repository) SaveAccount(ctx context.Context, a Account) error {
	row := accountRow{
		DirectoryURL: a.DirectoryURL,
		KeySealed:    a.KeySealed,
		Registration: string(a.Registration),
		CreatedAt:    a.CreatedAt.UTC(),
	}
	_, err := r.bdb.NewInsert().Model(&row).
		On("CONFLICT (directory_url) DO UPDATE").
		Set("key_sealed = EXCLUDED.key_sealed").
		Set("registration = EXCLUDED.registration").
		Set("created_at = EXCLUDED.created_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("tlsacme: save account: %w", err)
	}
	return nil
}

// LoadAccount returns the account of directoryURL; ok is false when there is none.
func (r *Repository) LoadAccount(ctx context.Context, directoryURL string) (Account, bool, error) {
	var row accountRow
	err := r.bdb.NewSelect().Model(&row).Where("directory_url = ?", directoryURL).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("tlsacme: load account: %w", err)
	}
	return Account{
		DirectoryURL: row.DirectoryURL,
		KeySealed:    row.KeySealed,
		Registration: []byte(row.Registration),
		CreatedAt:    row.CreatedAt.UTC(),
	}, true, nil
}

// SaveCertificate stores c, replacing the certificate of the same domain.
func (r *Repository) SaveCertificate(ctx context.Context, c StoredCertificate) error {
	row := certificateRow{
		Domain:       c.Domain,
		DirectoryURL: c.DirectoryURL,
		ChainSealed:  c.ChainSealed,
		KeySealed:    c.KeySealed,
		NotBefore:    c.NotBefore.UTC(),
		NotAfter:     c.NotAfter.UTC(),
		UpdatedAt:    c.UpdatedAt.UTC(),
	}
	_, err := r.bdb.NewInsert().Model(&row).
		On("CONFLICT (domain) DO UPDATE").
		Set("directory_url = EXCLUDED.directory_url").
		Set("chain_sealed = EXCLUDED.chain_sealed").
		Set("key_sealed = EXCLUDED.key_sealed").
		Set("not_before = EXCLUDED.not_before").
		Set("not_after = EXCLUDED.not_after").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("tlsacme: save certificate: %w", err)
	}
	return nil
}

// LoadCertificate returns the certificate of domain; ok is false when there is none.
func (r *Repository) LoadCertificate(ctx context.Context, domain string) (StoredCertificate, bool, error) {
	var row certificateRow
	err := r.bdb.NewSelect().Model(&row).Where("domain = ?", domain).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredCertificate{}, false, nil
	}
	if err != nil {
		return StoredCertificate{}, false, fmt.Errorf("tlsacme: load certificate: %w", err)
	}
	return StoredCertificate{
		Domain:       row.Domain,
		DirectoryURL: row.DirectoryURL,
		ChainSealed:  row.ChainSealed,
		KeySealed:    row.KeySealed,
		NotBefore:    row.NotBefore.UTC(),
		NotAfter:     row.NotAfter.UTC(),
		UpdatedAt:    row.UpdatedAt.UTC(),
	}, true, nil
}
