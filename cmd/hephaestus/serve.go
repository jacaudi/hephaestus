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

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
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
HEPHAESTUS_SHUTDOWN_GRACE, HEPHAESTUS_LOG_LEVEL and HEPHAESTUS_LOG_FORMAT (or
--log-format).

The process stops on SIGINT or SIGTERM after draining in-flight requests.`,
		Example: `  HEPHAESTUS_FQDN=boot.example.com \
  HEPHAESTUS_PROVISIONING_INTERFACE=vlan20 \
  HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5 \
  HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24 \
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
		client: toolcache.NewHTTPClient("hephaestus/"+buildVersion(), nil),
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
	// client is the upstream client the fetcher downloads utility content with.
	client *http.Client
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
	if err := dbh.Migrate(ctx); err != nil {
		return err
	}

	content, moved, err := store.Open(ctx, filepath.Join(cfg.DataDir, "store"), time.Now)
	if err != nil {
		return err
	}
	if moved != "" {
		logger.ErrorContext(ctx, "store was unusable; moved aside", "path", moved)
	}

	repo := fleet.NewRepository(dbh)

	// Bring the cache rows, the store and the spool into agreement with the
	// catalog before anything serves or fetches (design section 5.7).
	rc := toolcache.ReconcileConfig{
		Repo: repo, Store: content, Catalog: deps.catalog, SpoolDir: filepath.Join(cfg.DataDir, "spool"), At: time.Now(),
	}
	fetcher, err := prepareUtility(ctx, logger, metrics, rc, deps.client)
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
	)...)
	registry.RunOnce(ctx)

	handlers, err := boot.NewHandlers(boot.Config{
		Repo: repo, Store: content, Catalog: deps.catalog,
		FQDN: cfg.FQDN, Address: cfg.Address, Subnet: cfg.Subnet, Files: deps.files,
	}, boot.WithMeter(metrics.Meter()), boot.WithErrorHandler(func(ctx context.Context, err error) {
		// A lost race with the CLI is expected now and then; lost content is not.
		level := slog.LevelError
		if errors.Is(err, boot.ErrBootNextRace) {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level, "boot error", "error", err)
	}))
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

// prepareUtility runs design section 5.7's steps 2 to 5: it creates the spool
// directory, reconciles the cache rows, the store and the spool with the
// catalog, and collects the garbage of tags that are no longer current, logging
// what each changed. It returns the fetcher over the same rows, store and
// spool, not yet started.
func prepareUtility(ctx context.Context, logger *slog.Logger, metrics *telemetry.Metrics, rc toolcache.ReconcileConfig, client *http.Client) (*toolcache.Fetcher, error) {
	if err := os.MkdirAll(rc.SpoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	rep, err := toolcache.Reconcile(ctx, rc)
	if err != nil {
		return nil, fmt.Errorf("reconcile utility cache: %w", err)
	}
	logger.InfoContext(ctx, "utility reconciled",
		"interrupted", rep.Interrupted, "deleted", rep.Deleted, "repinned", rep.Repinned,
		"requeued", rep.Requeued, "restored", rep.Restored, "spool_removed", rep.SpoolRemoved)
	untagged, err := toolcache.CollectGarbage(ctx, rc.Store, rc.Catalog)
	if err != nil {
		return nil, fmt.Errorf("collect utility garbage: %w", err)
	}
	logger.InfoContext(ctx, "utility garbage collected", "untagged", untagged)

	return toolcache.NewFetcher(
		toolcache.FetcherConfig{Repo: rc.Repo, Store: rc.Store, Catalog: rc.Catalog, Client: client, SpoolDir: rc.SpoolDir},
		toolcache.WithMeter(metrics.Meter()),
		toolcache.WithEvents(func(ctx context.Context, ev toolcache.FetchEvent) { logFetchEvent(ctx, logger, ev) }),
		toolcache.WithErrorHandler(func(ctx context.Context, err error) {
			logger.WarnContext(ctx, "utility fetch loop error", "error", err)
		}),
	)
}

// logFetchEvent logs one state change of a utility cache row at Info. The
// event's error has the userinfo, query and fragment of any URL removed already.
func logFetchEvent(ctx context.Context, logger *slog.Logger, ev toolcache.FetchEvent) {
	var cause string
	if ev.Err != nil {
		cause = ev.Err.Error()
	}
	logger.InfoContext(ctx, "utility fetch",
		"entry", ev.Key.Entry, "arch", string(ev.Key.Arch), "pin", ev.Pin,
		"from", string(ev.From), "to", string(ev.To), "attempts", ev.Attempts,
		"class", ev.Class, "error", cause, "bytes", ev.Bytes, "duration_ms", ev.Duration.Milliseconds())
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
	// fetcher downloads the utility content that cache rows ask for.
	fetcher *toolcache.Fetcher

	adminSrv, httpSrv *http.Server
	// httpsSrv is the boot HTTPS server in file mode and proxySrv the one behind
	// the TLS-terminating proxy in proxy mode; the other is nil.
	httpsSrv, proxySrv *http.Server
}

// run starts every component, marks the service ready and serves until ctx is
// done or a component fails. It then shuts down in this order: proxyDHCP;
// the boot servers (draining requests); TFTP, the checks and the certificate
// loader; the admin server, which stays up the longest so probes and metrics
// see the drain. The two server drains share one ShutdownGrace deadline, set
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
