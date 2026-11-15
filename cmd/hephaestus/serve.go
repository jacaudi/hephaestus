package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	legolog "github.com/go-acme/lego/v4/log"
	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
	"go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/dhcp"
	"go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/tftp"
	"go.jacaudi.dev/hephaestus/internal/tlsacme"
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
		Long: `Run proxyDHCP, TFTP, the boot HTTP and HTTPS listeners and the admin listener.

Configuration comes from the environment. Required:
  HEPHAESTUS_FQDN                    name the boot HTTPS certificate covers
  HEPHAESTUS_PROVISIONING_INTERFACE  interface proxyDHCP listens on
  HEPHAESTUS_PROVISIONING_ADDRESS    this host's address on the provisioning subnet
  HEPHAESTUS_PROVISIONING_SUBNET     provisioning subnet, as a CIDR prefix
  HEPHAESTUS_DATA_KEY                base64 32-byte key that seals stored secrets
                                     (or HEPHAESTUS_DATA_KEY_FILE)
  HEPHAESTUS_CLOUDFLARE_TOKEN        Cloudflare token for the DNS-01 challenge
                                     (or HEPHAESTUS_CLOUDFLARE_TOKEN_FILE)

Optional: HEPHAESTUS_ADMIN_ADDRESS, HEPHAESTUS_DATA_DIR, HEPHAESTUS_DATABASE_URL,
HEPHAESTUS_ACME_DIRECTORY_URL, HEPHAESTUS_SHUTDOWN_GRACE, HEPHAESTUS_LOG_LEVEL and
HEPHAESTUS_LOG_FORMAT (or --log-format).

The process stops on SIGINT or SIGTERM after draining in-flight requests.`,
		Example: `  HEPHAESTUS_FQDN=boot.example.com \
  HEPHAESTUS_PROVISIONING_INTERFACE=vlan20 \
  HEPHAESTUS_PROVISIONING_ADDRESS=10.10.20.5 \
  HEPHAESTUS_PROVISIONING_SUBNET=10.10.20.0/24 \
  HEPHAESTUS_DATA_KEY_FILE=/run/secrets/data_key \
  HEPHAESTUS_CLOUDFLARE_TOKEN_FILE=/run/secrets/cloudflare_token \
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
			legolog.Logger = slog.NewLogLogger(logger.Handler(), slog.LevelInfo)

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
	lns, err := listen(ctx,
		netip.AddrPortFrom(cfg.Address, bootHTTPPort).String(),
		netip.AddrPortFrom(cfg.Address, bootHTTPSPort).String(),
		cfg.AdminAddr.String(),
	)
	if err != nil {
		return err
	}
	return runServe(ctx, cfg, logger, serveDeps{
		newIssuer: legoIssuerFactory(cfg),
		httpLn:    lns[0], httpsLn: lns[1], adminLn: lns[2],
		udp:   true,
		files: files,
	})
}

// fatalLogged logs err at Error level and returns it marked as logged, so main
// exits 1 without printing it a second time.
func fatalLogged(ctx context.Context, logger *slog.Logger, err error) error {
	logger.ErrorContext(ctx, "serve failed", "error", err)
	return loggedError{err}
}

// legoIssuerFactory returns the serveDeps.newIssuer that issues through Let's
// Encrypt (or the configured ACME directory) with DNS-01 on Cloudflare.
func legoIssuerFactory(cfg serveConfig) func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error) {
	return func(ctx context.Context, repo *tlsacme.Repository, key *datakey.Key) (tlsacme.Issuer, error) {
		iss := tlsacme.NewLegoIssuer(tlsacme.LegoConfig{
			DirectoryURL:    cfg.ACMEDirectoryURL,
			CloudflareToken: cfg.CloudflareToken,
			Repo:            repo,
			Key:             key,
		})
		if err := iss.Load(ctx); err != nil {
			return nil, err
		}
		return iss, nil
	}
}

// serveDeps are the parts of runServe a test replaces.
type serveDeps struct {
	// newIssuer builds the certificate issuer; an error stops startup.
	newIssuer func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error)
	// httpLn, httpsLn and adminLn are the boot HTTP, boot HTTPS and admin
	// listeners. runServe owns them and closes them on return.
	httpLn, httpsLn, adminLn net.Listener
	// udp starts proxyDHCP and TFTP. Tests set it false: both bind fixed
	// privileged UDP ports.
	udp bool
	// files are the iPXE binaries, by the names TFTP and /tftp serve them under.
	files map[string][]byte
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

// closeAll closes every listener and returns the failures. A listener that is
// already closed (http.Server closes the one it serves) is not a failure.
func closeAll(lns ...net.Listener) error {
	var errs []error
	for _, ln := range lns {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runServe runs the service until ctx is done or a component fails, then
// shuts down in order. It returns nil after a clean stop.
func runServe(ctx context.Context, cfg serveConfig, logger *slog.Logger, deps serveDeps) (err error) {
	defer func() { err = errors.Join(err, closeAll(deps.httpLn, deps.httpsLn, deps.adminLn)) }()

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

	repo := fleet.NewRepository(dbh)
	acmeRepo := tlsacme.NewRepository(dbh)
	iss, err := deps.newIssuer(ctx, acmeRepo, cfg.DataKey)
	if err != nil {
		return fmt.Errorf("build certificate issuer: %w", err)
	}
	mgr, err := tlsacme.NewManager(acmeRepo, cfg.DataKey, iss, cfg.FQDN,
		tlsacme.WithDirectoryURL(cfg.ACMEDirectoryURL),
		tlsacme.WithMeter(metrics.Meter()),
		tlsacme.WithErrorHandler(func(ctx context.Context, err error) {
			logger.WarnContext(ctx, "certificate renewal failed", "error", err)
		}),
	)
	if err != nil {
		return err
	}
	if err := mgr.Load(ctx); err != nil {
		return err
	}

	registry := admin.NewRegistry(slices.Concat(
		dbh.Checks(),
		[]admin.Check{admin.DiskCheck("disk:data_dir", cfg.DataDir, minFreeDisk)},
		mgr.Checks(),
	)...)
	registry.RunOnce(ctx)

	handlers, err := boot.NewHandlers(repo, cfg.FQDN, cfg.Subnet, deps.files, boot.WithMeter(metrics.Meter()))
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "starting",
		"fqdn", cfg.FQDN,
		"address", cfg.Address.String(),
		"subnet", cfg.Subnet.String(),
		"admin_address", cfg.AdminAddr.String(),
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
	httpsHandler := telemetry.WideEvents(metrics.Instrument(handlers.TLS(), "boot-https", deps.httpsLn.Addr().String()), logger, slog.LevelInfo)

	httpsSrv := newServer(deps.httpsLn, httpsHandler, time.Minute, logger)
	httpsSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: mgr.GetCertificate}
	p := &plane{
		cfg: cfg, deps: deps, logger: logger, life: life, registry: registry, mgr: mgr,
		adminSrv: newServer(deps.adminLn, adminHandler, 10*time.Second, logger),
		httpSrv:  newServer(deps.httpLn, httpHandler, time.Minute, logger),
		httpsSrv: httpsSrv,
	}
	return p.run(ctx)
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
	mgr      *tlsacme.Manager

	adminSrv, httpSrv, httpsSrv *http.Server
}

// run starts every component, marks the service ready and serves until ctx is
// done or a component fails. It then shuts down in this order: proxyDHCP;
// the boot servers (draining requests); TFTP, the checks and the certificate
// manager; the admin server, which stays up the longest so probes and
// metrics see the drain. It returns the failure that stopped the service, if
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
	web.start("boot http server", func() error { return serveErr(p.httpSrv.Serve(p.deps.httpLn)) })
	web.start("boot https server", func() error { return serveErr(p.httpsSrv.ServeTLS(p.deps.httpsLn, "", "")) })
	work.start("health checks", func() error { p.registry.Run(workCtx); return nil })
	work.start("certificate manager", func() error { return p.mgr.Run(workCtx) })
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

	webErr := errors.Join(shutdownAll(drainCtx, p.httpSrv, p.httpsSrv), web.wait())

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
