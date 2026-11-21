package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/factory"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/secrets"
	"go.jacaudi.dev/hephaestus/internal/store"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
	"go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/dhcp"
	"go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/tftp"
	"go.jacaudi.dev/hephaestus/internal/tlscert"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

const (
	// bootHTTPPort and bootHTTPSPort are fixed: iPXE binaries and the proxyDHCP
	// offer name them.
	bootHTTPPort  = 80
	bootHTTPSPort = 443

	// minFreeDisk is the free fraction of the data dir's disk below which the
	// disk check warns.
	minFreeDisk = 0.10
)

// newServeCmd builds the serve subcommand.
func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the boot plane and the admin listener",
		Long: `Run proxyDHCP, TFTP, the boot HTTP listener, the boot HTTPS listener (file mode) or the
listener a TLS-terminating proxy forwards to (proxy mode), and the admin listener.

Configuration comes from the environment. Required:
  HEPHAESTUS_FQDN                    name the boot HTTPS certificate covers
  HEPHAESTUS_PROVISIONING_INTERFACE  interface proxyDHCP listens on
  HEPHAESTUS_PROVISIONING_ADDRESS    this host's address on the provisioning subnet
  HEPHAESTUS_PROVISIONING_SUBNET     provisioning subnet, as a CIDR prefix
  HEPHAESTUS_DATA_KEY                base64 of the 32-byte key that seals stored secrets, or
  HEPHAESTUS_DATA_KEY_FILE           a file holding it: set exactly one; a key the database
                                     was not sealed with stops serve

The 1Password service account token is HEPHAESTUS_OP_TOKEN, or a file holding it
in HEPHAESTUS_OP_TOKEN_FILE: set at most one. Without it serve still starts and
keeps serving approved configs; a new tree, a refresh and a first credential are
refused until it is set.

TLS comes from one of two sources, chosen by HEPHAESTUS_TLS_MODE:
  file (default)  HEPHAESTUS_TLS_CERT_FILE and HEPHAESTUS_TLS_KEY_FILE name a
                  certificate chain and key that another process (certbot, lego,
                  cert-manager) writes and renews; changes are picked up within
                  a minute
  proxy           a reverse proxy terminates TLS 1.3 and forwards plain HTTP to
                  HEPHAESTUS_PROXY_ADDRESS (IP:port); HEPHAESTUS_PROXY_TRUSTED
                  lists the proxies' CIDR prefixes, comma separated, and the
                  client address is taken from their X-Forwarded-For header

Optional: HEPHAESTUS_ADMIN_ADDRESS, HEPHAESTUS_DATA_DIR, HEPHAESTUS_DATABASE_URL,
HEPHAESTUS_FETCH_WORKERS (utility downloads at once, 1 to 16, default 4),
HEPHAESTUS_SHUTDOWN_GRACE, HEPHAESTUS_LOG_LEVEL and HEPHAESTUS_LOG_FORMAT (or
--log-format).

The process stops on SIGINT or SIGTERM after draining in-flight requests.`,
		Example: `  HEPHAESTUS_FQDN=boot.example.com \
  HEPHAESTUS_PROVISIONING_INTERFACE=vlan20 \
  HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5 \
  HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24 \
  HEPHAESTUS_DATA_KEY_FILE=/run/secrets/hephaestus-data-key \
  HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem \
  HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem \
  hephaestus serve`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// cmd.Flags() includes the root's persistent flags, so --log-format applies.
			// Errors before the logger exists stay plain text on stderr.
			cfg, err := loadConfig(cmd.Flags(), hostInterfaceAddrs)
			if err != nil {
				return err
			}
			logger, err := telemetry.NewLogger(cmd.OutOrStdout(), cfg.LogFormat, cfg.LogLevel, buildVersion())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := listenAndServe(ctx, cfg, logger); err != nil {
				return fatalLogged(ctx, logger, err)
			}
			return nil
		},
	}
}

// listenAndServe opens the boot and admin listeners and runs the service on
// them until ctx is done.
func listenAndServe(ctx context.Context, cfg serveConfig, logger *slog.Logger) error {
	// First, before anything builds a client from the default transport or sends a request.
	counter := secrets.NewCounter(http.DefaultTransport, time.Now)
	setupDefaultTransport(counter)

	files, err := ipxe.Files()
	if err != nil {
		return fmt.Errorf("load iPXE binaries: %w", err)
	}
	roots, err := ipxe.TrustedRoots()
	if err != nil {
		return fmt.Errorf("load iPXE trusted roots: %w", err)
	}
	cat, err := catalog.Default()
	if err != nil {
		return fmt.Errorf("load utility catalog: %w", err)
	}
	// The second boot listener is HTTPS on the provisioning address in file
	// mode, and the proxy's upstream in proxy mode.
	bootTLSAddr := netip.AddrPortFrom(cfg.Address, bootHTTPSPort)
	if cfg.TLSMode == tlsModeProxy {
		bootTLSAddr = cfg.ProxyAddr
	}
	lns, err := listen(ctx,
		netip.AddrPortFrom(cfg.Address, bootHTTPPort).String(),
		bootTLSAddr.String(),
		cfg.AdminAddr.String(),
	)
	if err != nil {
		return err
	}
	deps := serveDeps{
		httpLn: lns[0], adminLn: lns[2], udp: true, files: files, roots: roots, catalog: cat,
		client:  toolcache.NewHTTPClient("hephaestus/"+buildVersion(), nil),
		counter: counter,
	}
	if cfg.TLSMode == tlsModeProxy {
		deps.proxyLn = lns[1]
	} else {
		deps.httpsLn = lns[1]
	}
	return runServe(ctx, cfg, logger, deps)
}

// fatalLogged logs err at Error level and returns it marked as logged, so main
// exits 1 without printing it a second time.
func fatalLogged(ctx context.Context, logger *slog.Logger, err error) error {
	logger.ErrorContext(ctx, "serve failed", "error", err)
	return loggedError{err}
}

// serveDeps are the parts of runServe a test replaces.
type serveDeps struct {
	// httpLn and adminLn are the boot HTTP and admin listeners. httpsLn is the
	// boot HTTPS listener in file mode, and proxyLn the listener a TLS-terminating
	// proxy forwards to in proxy mode: exactly one of the two is set. runServe
	// owns them and closes them on return.
	httpLn, httpsLn, proxyLn, adminLn net.Listener
	// udp starts proxyDHCP and TFTP. Tests set it false: both bind fixed
	// privileged UDP ports.
	udp bool
	// files are the iPXE binaries, by the names TFTP and /tftp serve them under.
	files map[string][]byte
	// roots are the certificates the iPXE binaries trust; a file-mode certificate
	// must chain to one of them.
	roots []*x509.Certificate
	// catalog is the utility catalog boot-next entries are looked up in and the
	// fetcher downloads.
	catalog *catalog.Catalog
	// client is the upstream client the fetcher downloads utility content and
	// Talos assets with, and registers schematics through.
	client *http.Client
	// counter counts the requests to 1Password; http.DefaultClient sends
	// through it.
	counter *secrets.Counter
	// resolver, when set, replaces the 1Password resolver built from the token.
	// Only a test sets it, to hold the clusters service mid-resolve.
	resolver clusters.Resolver
}

// listen opens a TCP listener on each address, in order. On a failure it
// closes the listeners it already opened.
func listen(ctx context.Context, addrs ...string) ([]net.Listener, error) {
	var lc net.ListenConfig
	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("listen on %s: %w", addr, err), closeAll(lns...))
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// closeAll closes every listener that is set and returns the failures. A
// listener that is already closed (http.Server closes the one it serves) is not
// a failure.
func closeAll(lns ...net.Listener) error {
	var errs []error
	for _, ln := range lns {
		if ln == nil {
			continue
		}
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runServe runs the service until ctx is done or a component fails, then
// shuts down in order. It returns nil after a clean stop.
func runServe(ctx context.Context, cfg serveConfig, logger *slog.Logger, deps serveDeps) (err error) {
	defer func() { err = errors.Join(err, closeAll(deps.httpLn, deps.httpsLn, deps.proxyLn, deps.adminLn)) }()

	// Fail closed: without roots the certificate chain check would be skipped.
	if cfg.TLSMode == tlsModeFile && len(deps.roots) == 0 {
		return errors.New("file mode needs the embedded iPXE roots to check the certificate chain against")
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	metrics, err := telemetry.NewMetrics(buildVersion(), commit)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
		defer cancel()
		err = errors.Join(err, metrics.Shutdown(sctx))
	}()

	dbh, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dbh.Close()) }()
	if err := prepareDatabase(ctx, dbh, cfg.DataKey); err != nil {
		return err
	}

	content, err := openStore(ctx, filepath.Join(cfg.DataDir, "store"), logger)
	if err != nil {
		return err
	}

	repo := fleet.NewRepository(dbh)

	// Bring the cache rows, the store and the spool into agreement with the
	// catalog before anything serves or fetches (design section 5.7).
	rc := toolcache.ReconcileConfig{
		Repo: repo, Store: content, Catalog: deps.catalog, SpoolDir: filepath.Join(cfg.DataDir, "spool"), At: time.Now(),
	}
	if err := reconcileCaches(ctx, logger, rc); err != nil {
		return err
	}

	svc, err := startClusters(ctx, clusters.Config{
		Repo: clusters.NewRepository(dbh, cfg.DataKey), Resolver: deps.resolver, Counter: deps.counter,
		GitDir: filepath.Join(cfg.DataDir, "git"), Now: time.Now, OnError: clustersErrorHandler(logger), Meter: metrics.Meter(),
	}, cfg.OPToken)
	if err != nil {
		return err
	}

	fetcher, err := newFetcher(logger, metrics, toolcache.FetcherConfig{
		Repo: rc.Repo, Store: rc.Store, Catalog: rc.Catalog, Client: deps.client, SpoolDir: rc.SpoolDir, Workers: cfg.FetchWorkers,
		Talos: factory.New(factory.Base, deps.client),
	})
	if err != nil {
		return err
	}

	// File mode serves the certificate files; an unusable pair stops startup
	// before anything serves. Proxy mode has no certificate: the proxy holds it.
	var loader *tlscert.Loader
	var tlsChecks []admin.Check
	if cfg.TLSMode == tlsModeFile {
		loader, err = tlscert.NewLoader(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.FQDN,
			tlscert.WithRoots(deps.roots),
			tlscert.WithMeter(metrics.Meter()),
			tlscert.WithErrorHandler(func(err error) {
				logger.WarnContext(ctx, "certificate reload failed", "error", err)
			}),
		)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		tlsChecks = loader.Checks()
	}

	registry := admin.NewRegistry(slices.Concat(
		dbh.Checks(),
		content.Checks(),
		[]admin.Check{admin.DiskCheck("disk:data_dir", cfg.DataDir, minFreeDisk)},
		tlsChecks,
		svc.Checks(),
		[]admin.Check{talosCacheCheck(repo), configPendingCheck(repo, time.Now)},
	)...)
	registry.RunOnce(ctx)

	handlers, err := boot.NewHandlers(boot.Config{
		Repo: repo, Store: content, Catalog: deps.catalog,
		FQDN: cfg.FQDN, Address: cfg.Address, Subnet: cfg.Subnet, Files: deps.files, Configs: svc,
	}, boot.WithMeter(metrics.Meter()), boot.WithErrorHandler(bootErrorHandler(logger)))
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "starting",
		"fqdn", cfg.FQDN,
		"address", cfg.Address.String(),
		"subnet", cfg.Subnet.String(),
		"admin_address", cfg.AdminAddr.String(),
		"tls_mode", cfg.TLSMode,
		"data_dir", cfg.DataDir,
		"database", string(dbh.Dialect()),
	)

	life := &admin.Lifecycle{}
	info := admin.Info{Version: buildVersion(), ReleaseID: commit, Started: time.Now()}
	adminHandler := telemetry.WideEvents(admin.NewHandler(registry, life, metrics.Handler(), info), logger, slog.LevelInfo)
	// Plaintext by design (design §5.10): the embedded chain script carries no
	// domain and chains to http://${proxydhcp/next-server}/start, and the iPXE
	// artifact routes (/tftp/{arch}/snp.efi) are served here too, as well as on
	// HTTPS. Every other boot route redirects to HTTPS. (All binaries, BIOS
	// included, are built with HTTPS support; TLS is not what is missing here.)
	httpHandler := telemetry.WideEvents(metrics.Instrument(handlers.PlainHTTP(), "boot-http", deps.httpLn.Addr().String()), logger, slog.LevelInfo)

	p := &plane{
		cfg: cfg, deps: deps, logger: logger, life: life, registry: registry, loader: loader, fetcher: fetcher,
		clusters: svc, handlers: handlers, repo: repo,
		adminSrv: newServer(deps.adminLn, adminHandler, 10*time.Second, logger),
		httpSrv:  newServer(deps.httpLn, httpHandler, time.Minute, logger),
	}
	if cfg.TLSMode == tlsModeProxy {
		// The proxy terminates TLS and forwards the TLS routes as plain HTTP, so
		// this server does no TLS itself and trusts the client address only from
		// the proxies in cfg.TrustedProxies.
		proxyHandler := telemetry.WideEvents(metrics.Instrument(handlers.Proxied(cfg.TrustedProxies), "boot-proxy", deps.proxyLn.Addr().String()), logger, slog.LevelInfo)
		p.proxySrv = newServer(deps.proxyLn, proxyHandler, time.Minute, logger)
	} else {
		httpsHandler := telemetry.WideEvents(metrics.Instrument(handlers.TLS(), "boot-https", deps.httpsLn.Addr().String()), logger, slog.LevelInfo)
		p.httpsSrv = newServer(deps.httpsLn, httpsHandler, time.Minute, logger)
		// Session tickets are off: Go sends a NewSessionTicket before it reads the
		// client Finished, and iPXE 9261ebe hashes that early ticket into its
		// handshake transcript, so its Finished is wrong and the handshake fails.
		// iPXE gains nothing from resumption.
		p.httpsSrv.TLSConfig = &tls.Config{
			MinVersion:             tls.VersionTLS13,
			GetCertificate:         loader.GetCertificate,
			SessionTicketsDisabled: true,
		}
	}
	return p.run(ctx)
}

// bootErrorHandler logs what boot reports. A lost race with the CLI is expected
// now and then, so it is a Warn; lost content is not, nor is anything else.
func bootErrorHandler(logger *slog.Logger) func(context.Context, error) {
	return func(ctx context.Context, err error) {
		level := slog.LevelError
		if errors.Is(err, boot.ErrBootNextRace) {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level, "boot error", "error", err)
	}
}

// prepareDatabase migrates the database and checks that key opens its canary,
// writing the canary first when the database has none. A key that does not
// open it is an error naming HEPHAESTUS_DATA_KEY.
func prepareDatabase(ctx context.Context, dbh *db.DB, key *datakey.Key) error {
	if err := dbh.Migrate(ctx); err != nil {
		return err
	}
	if err := datakey.EnsureCanary(ctx, dbh.Bun(), key); err != nil {
		if errors.Is(err, datakey.ErrCanary) {
			return fmt.Errorf("check HEPHAESTUS_DATA_KEY: %w", err)
		}
		return err
	}
	return nil
}

// openStore opens the content store at root, its failures going to
// storeErrorHandler. A layout it had to set aside is logged at Error.
func openStore(ctx context.Context, root string, logger *slog.Logger) (*store.Store, error) {
	content, moved, err := store.Open(ctx, root, time.Now, store.WithErrorHandler(storeErrorHandler(logger)))
	if err != nil {
		return nil, err
	}
	if moved != "" {
		logger.ErrorContext(ctx, "store was unusable; moved aside", "path", moved)
	}
	return content, nil
}

// startClusters builds the clusters service over cfg and runs its Startup. The
// resolver is cfg's when set, else the 1Password resolver for token; with no
// token it stays a nil interface, never a nil *secrets.OnePassword, so the
// service fails every cluster closed and secrets:token warns. It also reports
// cfg.Counter's count as the hephaestus.secrets.requests gauge on cfg.Meter.
func startClusters(ctx context.Context, cfg clusters.Config, token secret.Secret) (*clusters.Service, error) {
	if cfg.Resolver == nil && token != "" {
		cfg.Resolver = secrets.NewOnePassword(token, buildVersion())
	}
	svc, err := clusters.NewService(cfg)
	if err != nil {
		return nil, err
	}
	if err := svc.Startup(ctx); err != nil {
		return nil, fmt.Errorf("start clusters: %w", err)
	}
	if err := observeSecretsRequests(cfg.Meter, cfg.Counter); err != nil {
		return nil, err
	}
	return svc, nil
}

// storeErrorHandler logs what the store survives. A filesystem that refuses to
// fsync a directory is a known limit of some volumes, told once: a Warn.
// Anything else is an Error.
func storeErrorHandler(logger *slog.Logger) func(context.Context, error) {
	return func(ctx context.Context, err error) {
		level := slog.LevelError
		if errors.Is(err, store.ErrDirSyncUnsupported) {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level, "store error", "error", err)
	}
}

// clustersErrorHandler logs a failure the clusters service survives at Warn.
// Its text names a stage and a cluster, never a value, a URL or a resolver's,
// renderer's or git's own text. Render calls it from HTTP goroutines; a
// logger is safe for that.
func clustersErrorHandler(logger *slog.Logger) func(context.Context, error) {
	return func(ctx context.Context, err error) {
		logger.WarnContext(ctx, "clusters error", "error", err)
	}
}

// observeSecretsRequests reports the requests counter saw to 1Password in the
// last 24 hours as the gauge hephaestus.secrets.requests.
func observeSecretsRequests(meter metric.Meter, counter *secrets.Counter) error {
	_, err := meter.Int64ObservableGauge("hephaestus.secrets.requests",
		metric.WithDescription("Requests to 1Password in the last 24 hours."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(counter.Last24h()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("secrets requests gauge: %w", err)
	}
	return nil
}

// reconcileCaches runs design section 5.7's steps 2 to 5: it reconciles the
// utility cache rows, the store and the spool (creating the spool directory)
// with the catalog, collects the garbage of tags that are no longer current,
// then reconciles the Talos rows with the store, logging what each changed. A
// reconciliation failure is returned; a spool sweep or a collection failure is
// logged at Warn and startup goes on.
func reconcileCaches(ctx context.Context, logger *slog.Logger, rc toolcache.ReconcileConfig) error {
	rep, err := toolcache.Reconcile(ctx, rc)
	if err != nil {
		return fmt.Errorf("reconcile utility cache: %w", err)
	}
	logger.InfoContext(ctx, "utility reconciled",
		"interrupted", rep.Interrupted, "deleted", rep.Deleted, "repinned", rep.Repinned,
		"requeued", rep.Requeued, "restored", rep.Restored, "spool_removed", rep.SpoolRemoved)
	// Stray spool files are only disk, so a sweep that failed is not worth a stop.
	if rep.SpoolErr != nil {
		logger.WarnContext(ctx, "utility spool sweep failed", "error", rep.SpoolErr)
	}
	// Garbage is only disk, so a failed collection is not worth a stop.
	if untagged, err := toolcache.CollectGarbage(ctx, rc.Store, rc.Catalog); err != nil {
		logger.WarnContext(ctx, "utility garbage collection failed", "untagged", untagged, "error", err)
	} else {
		logger.InfoContext(ctx, "utility garbage collected", "untagged", untagged)
	}

	talos, err := toolcache.ReconcileTalos(ctx, rc)
	if err != nil {
		return fmt.Errorf("reconcile Talos cache: %w", err)
	}
	logger.InfoContext(ctx, "talos reconciled",
		"interrupted", talos.Interrupted, "requeued", talos.Requeued, "restored", talos.Restored)
	return nil
}

// newFetcher returns the fetcher of the utility and Talos rows over fc, not
// yet started, logging every state change and alerting on a broken Talos row.
func newFetcher(logger *slog.Logger, metrics *telemetry.Metrics, fc toolcache.FetcherConfig) (*toolcache.Fetcher, error) {
	alerts, err := metrics.Meter().Int64Counter("hephaestus.alerts", metric.WithDescription("Operator alerts raised, by kind."))
	if err != nil {
		return nil, fmt.Errorf("alerts counter: %w", err)
	}
	return toolcache.NewFetcher(
		fc,
		toolcache.WithMeter(metrics.Meter()),
		toolcache.WithEvents(fetchEventHandler(logger, alerts)),
		toolcache.WithErrorHandler(func(ctx context.Context, err error) {
			logger.WarnContext(ctx, "utility fetch loop error", "error", err)
		}),
	)
}

// fetchEventHandler logs every fetch event and adds a talos_broken alert for
// each Talos row that goes broken (the fleet repository writes its talos.broken
// audit row). Several workers call it at once; the logger and counter are safe
// for that.
func fetchEventHandler(logger *slog.Logger, alerts metric.Int64Counter) func(context.Context, toolcache.FetchEvent) {
	return func(ctx context.Context, ev toolcache.FetchEvent) {
		logFetchEvent(ctx, logger, ev)
		if ev.Source == toolcache.SourceTalos && ev.To == fleet.CacheBroken {
			alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "talos_broken")))
		}
	}
}

// logFetchEvent logs one state change of a cache row at Info, with its source:
// a utility row by entry, arch and pin, a Talos row by schematic, version and
// arch. The event's error has the userinfo, query and fragment of any URL
// removed already.
func logFetchEvent(ctx context.Context, logger *slog.Logger, ev toolcache.FetchEvent) {
	var cause string
	if ev.Err != nil {
		cause = fleet.PrintableText(ev.Err.Error())
	}
	change := []any{
		"source", ev.Source, "from", string(ev.From), "to", string(ev.To), "attempts", ev.Attempts,
		"class", ev.Class, "error", cause, "bytes", ev.Bytes, "duration_ms", ev.Duration.Milliseconds(),
	}
	if ev.Source == toolcache.SourceTalos {
		logger.InfoContext(ctx, "talos fetch", append([]any{
			"schematic", ev.Talos.Schematic, "version", ev.Talos.Version, "arch", string(ev.Talos.Arch),
		}, change...)...)
		return
	}
	logger.InfoContext(ctx, "utility fetch", append([]any{
		"entry", ev.Key.Entry, "arch", string(ev.Key.Arch), "pin", ev.Pin,
	}, change...)...)
}

const (
	// ticketRetention is how long a consumed, revoked or expired ticket is kept.
	// A ticket whose config recording is still being retried is neither
	// consumed nor revoked, and the retries start before it expires and give up
	// an hour after, so pruning never deletes it.
	ticketRetention = 7 * 24 * time.Hour
	// ticketPruneInterval is the time between two prunings.
	ticketPruneInterval = time.Hour
)

// runTicketPruning deletes the tickets consumed, revoked or expired more than
// ticketRetention before now, at once and then every ticketPruneInterval,
// until ctx ends. A failed pruning is logged at Warn and tried again at the
// next interval. It returns nil.
func runTicketPruning(ctx context.Context, repo *fleet.Repository, logger *slog.Logger, now func() time.Time) error {
	tick := time.Tick(ticketPruneInterval)
	for {
		n, err := repo.PruneTickets(ctx, now().Add(-ticketRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			logger.WarnContext(ctx, "ticket pruning failed", "error", err)
		case n > 0:
			logger.InfoContext(ctx, "tickets pruned", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// newServer returns an http.Server for ln with every timeout set. Its
// internal errors (failed TLS handshakes, panics) go to logger at Warn.
func newServer(ln net.Listener, h http.Handler, writeTimeout time.Duration, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// plane is the assembled service, ready to start.
type plane struct {
	cfg      serveConfig
	deps     serveDeps
	logger   *slog.Logger
	life     *admin.Lifecycle
	registry *admin.Registry
	// loader serves and follows the certificate files; nil in proxy mode.
	loader *tlscert.Loader
	// fetcher downloads the utility content and Talos assets that cache rows ask for.
	fetcher *toolcache.Fetcher
	// clusters polls the imported clusters; handlers serves the boot routes and
	// retries the config recordings; repo holds the tickets pruning deletes.
	clusters *clusters.Service
	handlers *boot.Handlers
	repo     *fleet.Repository

	adminSrv, httpSrv *http.Server
	// httpsSrv is the boot HTTPS server in file mode and proxySrv the one behind
	// the TLS-terminating proxy in proxy mode; the other is nil.
	httpsSrv, proxySrv *http.Server
}

// run starts every component, marks the service ready and serves until ctx is
// done or a component fails. It then shuts down in this order: proxyDHCP;
// the boot servers (draining requests); the work stage: TFTP, the checks, the
// fetcher, the clusters service, the config recorder (after the boot servers,
// so a recording that fails during the drain is still reported as abandoned),
// ticket pruning and the certificate loader, all before runServe closes the
// store and the database; the admin server, which stays up the longest so
// probes and metrics see the drain. The two server drains share one ShutdownGrace deadline, set
// when draining starts. It returns the failure that stopped the service, if
// any, joined with the shutdown errors.
func (p *plane) run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	// base outlives ctx: shutdown, not cancellation, decides when each stage stops.
	base := context.WithoutCancel(ctx)
	proxyCtx, stopProxy := context.WithCancel(base)
	defer stopProxy()
	workCtx, stopWork := context.WithCancel(base)
	defer stopWork()

	proxy, web, work, adm := &stage{stop: stop}, &stage{stop: stop}, &stage{stop: stop}, &stage{stop: stop}

	adm.start("admin server", func() error { return serveErr(p.adminSrv.Serve(p.deps.adminLn)) })
	bootSrvs := []*http.Server{p.httpSrv}
	web.start("boot http server", func() error { return serveErr(p.httpSrv.Serve(p.deps.httpLn)) })
	if p.httpsSrv != nil {
		bootSrvs = append(bootSrvs, p.httpsSrv)
		web.start("boot https server", func() error { return serveErr(p.httpsSrv.ServeTLS(p.deps.httpsLn, "", "")) })
	}
	if p.proxySrv != nil {
		bootSrvs = append(bootSrvs, p.proxySrv)
		web.start("boot proxy server", func() error { return serveErr(p.proxySrv.Serve(p.deps.proxyLn)) })
	}
	work.start("health checks", func() error { p.registry.Run(workCtx); return nil })
	work.start("utility fetcher", func() error { return p.fetcher.Run(workCtx) })
	work.start("clusters", func() error { return p.clusters.Run(workCtx) })
	work.start("config recorder", func() error { return p.handlers.RunRecorder(workCtx) })
	work.start("ticket pruning", func() error { return runTicketPruning(workCtx, p.repo, p.logger, time.Now) })
	if p.loader != nil {
		work.start("certificate loader", func() error { return p.loader.Run(workCtx) })
	}
	if p.deps.udp {
		addr := p.cfg.Address.String()
		dhcpProxy := dhcp.NewProxy(addr, bootHTTPPort, p.cfg.Interface, false, p.logger.With("component", "dhcp_proxy"))
		tftpServer := tftp.NewServer(addr, p.cfg.Subnet, p.deps.files, p.logger.With("component", "tftp"))
		proxy.start("proxyDHCP", func() error { return dhcpProxy.Run(proxyCtx) })
		work.start("tftp", func() error { return tftpServer.Run(workCtx) })
	}
	p.life.Set(admin.Ready)

	<-runCtx.Done()
	p.life.Set(admin.Draining)
	p.logger.InfoContext(base, "draining")

	// One deadline for both server drains, set now: a stop timeout equal to
	// ShutdownGrace must not end in SIGKILL because each drain took a full grace.
	drainCtx, cancelDrain := context.WithTimeout(base, p.cfg.ShutdownGrace)
	defer cancelDrain()

	stopProxy()
	proxyErr := proxy.wait()

	webErr := errors.Join(shutdownAll(drainCtx, bootSrvs...), web.wait())

	stopWork()
	workErr := work.wait()

	admErr := errors.Join(shutdownAll(drainCtx, p.adminSrv), adm.wait())

	p.life.Set(admin.Stopped)
	p.logger.InfoContext(base, "stopped")
	return errors.Join(proxyErr, webErr, workErr, admErr)
}

// stage runs components that stop together. A component that returns an
// error wakes the supervisor through stop.
type stage struct {
	stop context.CancelFunc

	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

// start runs run in its own goroutine. Every component here blocks until its
// context is cancelled or its server is shut down, so any error it returns
// is a failure.
func (s *stage) start(name string, run func() error) {
	s.wg.Go(func() {
		if err := run(); err != nil {
			s.mu.Lock()
			s.errs = append(s.errs, fmt.Errorf("%s: %w", name, err))
			s.mu.Unlock()
			s.stop()
		}
	})
}

// wait blocks until every component has returned and joins their errors.
func (s *stage) wait() error {
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.errs...)
}

// serveErr drops http.ErrServerClosed, which Serve returns after Shutdown.
func serveErr(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// shutdownAll shuts the servers down together within ctx. One that does not
// drain in time is closed, which cuts its open connections.
func shutdownAll(ctx context.Context, srvs ...*http.Server) error {
	errs := make([]error, len(srvs))
	var wg sync.WaitGroup
	for i, srv := range srvs {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				errs[i] = errors.Join(fmt.Errorf("shutdown %s: %w", srv.Addr, err), srv.Close())
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
