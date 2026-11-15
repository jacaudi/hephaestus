package tlsacme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

const (
	userAgent = "hephaestus"
	// accountSaveTimeout bounds storing a just-registered account, which must
	// not depend on the Obtain context (see register).
	accountSaveTimeout = 30 * time.Second
)

// LegoConfig configures a LegoIssuer.
type LegoConfig struct {
	// DirectoryURL is the ACME directory, e.g. the Let's Encrypt production or staging URL.
	DirectoryURL string
	// CloudflareToken is the API token that edits the domain's DNS records.
	CloudflareToken secret.Secret
	// Repo stores the ACME account.
	Repo *Repository
	// Key seals the ACME account key.
	Key *datakey.Key
}

// Format prints the directory and a redacted token for every verb, so the
// token never reaches a log through fmt.
func (c LegoConfig) Format(f fmt.State, _ rune) {
	if _, err := fmt.Fprintf(f, "{DirectoryURL: %s CloudflareToken: ****}", c.DirectoryURL); err != nil {
		return // fmt.State offers no way to report a write error
	}
}

// LogValue logs the directory and a redacted token.
func (c LegoConfig) LogValue() slog.Value {
	return slog.GroupValue(slog.String("directory_url", c.DirectoryURL), slog.String("cloudflare_token", "****"))
}

// LegoIssuer is the Let's Encrypt Issuer: it registers an ACME account on
// first use and proves control of the domain with the DNS-01 challenge on
// Cloudflare. Its configuration sits in an unexported field, which fmt cannot
// reach, so it redacts itself like LegoConfig.
type LegoIssuer struct {
	cfg LegoConfig
}

// NewLegoIssuer returns a LegoIssuer. It makes no network call.
func NewLegoIssuer(cfg LegoConfig) *LegoIssuer { return &LegoIssuer{cfg: cfg} }

// Format prints the directory and a redacted token for every verb.
func (l LegoIssuer) Format(f fmt.State, verb rune) { l.cfg.Format(f, verb) }

// LogValue logs the directory and a redacted token.
func (l LegoIssuer) LogValue() slog.Value { return l.cfg.LogValue() }

// Load checks that the stored ACME account, if there is one, opens under the
// data key. serve calls it at startup: a wrong data key must stop startup
// here, not silently register a second account on the next renewal.
func (l LegoIssuer) Load(ctx context.Context) error {
	_, err := l.storedUser(ctx)
	return err
}

// Obtain issues a certificate for domain. It returns ctx.Err() before any
// network call when ctx is done; lego itself takes no context, so a call in
// flight cannot be interrupted (Manager abandons it). The account is stored
// only after the server accepted the registration.
func (l LegoIssuer) Obtain(ctx context.Context, domain string) (Certificate, error) {
	if err := ctx.Err(); err != nil {
		return Certificate{}, err
	}
	user, err := l.storedUser(ctx)
	if err != nil {
		return Certificate{}, fmt.Errorf("acme: load account: %w", err)
	}
	if user == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return Certificate{}, fmt.Errorf("acme: generate account key: %w", err)
		}
		user = &acmeUser{key: key}
	}

	cfg := lego.NewConfig(user)
	cfg.CADirURL = l.cfg.DirectoryURL
	cfg.Certificate.KeyType = certcrypto.RSA2048
	cfg.UserAgent = userAgent
	client, err := lego.NewClient(cfg)
	if err != nil {
		return Certificate{}, fmt.Errorf("acme: create client: %w", err)
	}
	if user.reg == nil {
		if err := l.register(ctx, client, user); err != nil {
			return Certificate{}, err
		}
	}

	dns := cloudflare.NewDefaultConfig()
	dns.AuthToken = l.cfg.CloudflareToken.Expose()
	provider, err := cloudflare.NewDNSProviderConfig(dns)
	if err != nil {
		return Certificate{}, fmt.Errorf("acme: cloudflare provider: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(provider); err != nil {
		return Certificate{}, fmt.Errorf("acme: set DNS-01 provider: %w", err)
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{domain}, Bundle: true})
	if err != nil {
		return Certificate{}, fmt.Errorf("acme: obtain certificate: %w", err)
	}
	return Certificate{ChainPEM: res.Certificate, KeyPEM: res.PrivateKey}, nil
}

// register registers user's key with the server and stores the account.
func (l LegoIssuer) register(ctx context.Context, client *lego.Client, user *acmeUser) error {
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return fmt.Errorf("acme: register account: %w", err)
	}
	user.reg = reg

	keyDER, err := x509.MarshalPKCS8PrivateKey(user.key)
	if err != nil {
		return fmt.Errorf("acme: marshal account key: %w", err)
	}
	regJSON, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("acme: marshal registration: %w", err)
	}
	// Manager abandons an Obtain when its context is done, but the server now
	// holds the account: store it regardless, or the next start registers a
	// second one. The detached context gets its own timeout.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountSaveTimeout)
	defer cancel()
	err = l.cfg.Repo.SaveAccount(saveCtx, Account{
		DirectoryURL: l.cfg.DirectoryURL,
		KeySealed:    l.cfg.Key.Seal(keyDER, accountKeyAAD(l.cfg.DirectoryURL)),
		Registration: regJSON,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("acme: store account: %w", err)
	}
	return nil
}

// storedUser returns the stored account of the directory, or nil when there
// is none. An account that does not open under the data key is an error.
func (l LegoIssuer) storedUser(ctx context.Context) (*acmeUser, error) {
	dir := l.cfg.DirectoryURL
	row, ok, err := l.cfg.Repo.LoadAccount(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil //nolint:nilnil // no stored account is not an error; the caller registers one
	}
	keyDER, err := l.cfg.Key.Open(row.KeySealed, accountKeyAAD(dir))
	if err != nil {
		return nil, fmt.Errorf("stored ACME account for %s cannot be decrypted with the configured data key: %w", dir, err)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse stored ACME account key for %s: %w", dir, err)
	}
	var reg registration.Resource
	if err := json.Unmarshal(row.Registration, &reg); err != nil {
		return nil, fmt.Errorf("parse stored ACME registration for %s: %w", dir, err)
	}
	return &acmeUser{key: key, reg: &reg}, nil
}

// acmeUser is the ACME account lego acts for: its key and, once registered,
// the registration.
type acmeUser struct {
	key crypto.PrivateKey
	reg *registration.Resource
}

func (*acmeUser) GetEmail() string                          { return "" }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// accountKeyAAD binds the sealed account key to its row.
func accountKeyAAD(directoryURL string) []byte { return []byte("acme_accounts/key/" + directoryURL) }
