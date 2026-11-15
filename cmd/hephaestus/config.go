package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

// The values of HEPHAESTUS_TLS_MODE: where the boot HTTPS service gets TLS.
const (
	// tlsModeFile serves a certificate and key that an external process writes
	// and renews, at HEPHAESTUS_TLS_CERT_FILE and HEPHAESTUS_TLS_KEY_FILE.
	tlsModeFile = "file"
	// tlsModeProxy serves plain HTTP to a TLS-terminating reverse proxy.
	tlsModeProxy = "proxy"
)

// dnsLabel is one DNS label: letters, digits and inner hyphens, 1 to 63 bytes.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// rawConfig is the bootstrap configuration as decoded from HEPHAESTUS_*
// environment variables and the --log-format flag, before validation.
type rawConfig struct {
	FQDN         string `mapstructure:"fqdn"`
	Provisioning struct {
		Interface string `mapstructure:"interface"`
		Address   string `mapstructure:"address"`
		Subnet    string `mapstructure:"subnet"`
	} `mapstructure:"provisioning"`
	Admin struct {
		Address string `mapstructure:"address"`
	} `mapstructure:"admin"`
	Data struct {
		Dir string `mapstructure:"dir"`
	} `mapstructure:"data"`
	Database struct {
		URL secret.Secret `mapstructure:"url"`
	} `mapstructure:"database"`
	TLS struct {
		Mode     string `mapstructure:"mode"`
		CertFile string `mapstructure:"cert_file"`
		KeyFile  string `mapstructure:"key_file"`
	} `mapstructure:"tls"`
	Proxy struct {
		Address string   `mapstructure:"address"`
		Trusted []string `mapstructure:"trusted"`
	} `mapstructure:"proxy"`
	ShutdownGrace time.Duration `mapstructure:"shutdown_grace"`
	Log           struct {
		Format string     `mapstructure:"format"`
		Level  slog.Level `mapstructure:"level"`
	} `mapstructure:"log"`
}

// serveConfig is the validated configuration "hephaestus serve" runs with.
type serveConfig struct {
	FQDN, Interface string
	Address         netip.Addr
	Subnet          netip.Prefix
	AdminAddr       netip.AddrPort
	DataDir         string
	DatabaseURL     secret.Secret
	// TLSMode is tlsModeFile or tlsModeProxy. In file mode TLSCertFile and
	// TLSKeyFile are set; in proxy mode ProxyAddr and TrustedProxies are.
	TLSMode                 string
	TLSCertFile, TLSKeyFile string
	ProxyAddr               netip.AddrPort
	TrustedProxies          []netip.Prefix
	ShutdownGrace           time.Duration
	LogFormat               string
	LogLevel                slog.Level
}

// loadConfig reads the bootstrap configuration from the HEPHAESTUS_ environment
// and the --log-format flag in fs (precedence: flag, environment, default),
// then validates it. Every validation failure is returned together.
func loadConfig(fs *pflag.FlagSet, addrsOf interfaceAddrs) (serveConfig, error) {
	// ExperimentalBindStruct makes Unmarshal read every struct key from the
	// environment, even one with no default (go-standards §10).
	v := viper.NewWithOptions(viper.ExperimentalBindStruct())
	if err := v.BindPFlag("log.format", fs.Lookup("log-format")); err != nil {
		return serveConfig{}, fmt.Errorf("bind --log-format: %w", err)
	}
	v.SetEnvPrefix("HEPHAESTUS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetDefault("admin.address", "127.0.0.1:9090")
	v.SetDefault("data.dir", "/var/lib/hephaestus")
	v.SetDefault("tls.mode", tlsModeFile)
	v.SetDefault("shutdown_grace", 60*time.Second)
	v.SetDefault("log.format", "json")
	v.SetDefault("log.level", "info")

	var raw rawConfig
	hook := viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.TextUnmarshallerHookFunc(), // slog.Level from "info"
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","), // DecodeHook replaces Viper's defaults (go-standards §10)
	))
	if err := v.Unmarshal(&raw, hook); err != nil {
		return serveConfig{}, fmt.Errorf("decode config: %w", err)
	}
	return raw.validate(addrsOf)
}

// validate checks every field and returns the serveConfig, or every problem
// joined into one error (errors.Join drops the nil results).
func (r rawConfig) validate(addrsOf interfaceAddrs) (serveConfig, error) {
	var (
		cfg  serveConfig
		errs []error
		err  error
	)

	cfg.FQDN, err = validateFQDN(r.FQDN)
	errs = append(errs, err)

	cfg.Interface = r.Provisioning.Interface
	cfg.Address, cfg.Subnet, err = validateProvisioning(r.Provisioning.Address, r.Provisioning.Subnet)
	errs = append(errs, err, validateInterface(cfg.Interface, cfg.Address, addrsOf))

	cfg.AdminAddr, err = validateAdmin(r.Admin.Address, cfg.Subnet)
	errs = append(errs, err)

	cfg.DataDir = r.Data.Dir
	dirOK := filepath.IsAbs(cfg.DataDir)
	if !dirOK {
		errs = append(errs, fmt.Errorf("HEPHAESTUS_DATA_DIR %q: must be an absolute path", cfg.DataDir))
	}

	errs = append(errs, r.validateTLS(&cfg))

	cfg.DatabaseURL = r.Database.URL
	if cfg.DatabaseURL == "" && dirOK { // a bad data dir is already reported; don't derive a second error from it
		cfg.DatabaseURL = secret.Secret("sqlite:" + filepath.Join(cfg.DataDir, "hephaestus.db"))
	}
	if cfg.DatabaseURL != "" {
		if _, _, err := db.ParseURL(cfg.DatabaseURL.Expose()); err != nil {
			errs = append(errs, fmt.Errorf("HEPHAESTUS_DATABASE_URL: %w", err))
		}
	}

	cfg.ShutdownGrace = r.ShutdownGrace
	if cfg.ShutdownGrace <= 0 {
		errs = append(errs, errors.New("HEPHAESTUS_SHUTDOWN_GRACE must be positive"))
	}

	cfg.LogFormat = r.Log.Format
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("HEPHAESTUS_LOG_FORMAT %q: must be json or text", cfg.LogFormat))
	}
	cfg.LogLevel = r.Log.Level

	if err := errors.Join(errs...); err != nil {
		return serveConfig{}, err
	}
	return cfg, nil
}

// validateFQDN lowercases fqdn and requires at least two DNS labels, 253 bytes at most.
func validateFQDN(fqdn string) (string, error) {
	if fqdn == "" {
		return "", errors.New("HEPHAESTUS_FQDN is required")
	}
	fqdn = strings.ToLower(fqdn)
	labels := strings.Split(fqdn, ".")
	valid := len(labels) >= 2 && len(fqdn) <= 253
	for _, label := range labels {
		valid = valid && dnsLabel.MatchString(label)
	}
	if !valid {
		return "", fmt.Errorf("HEPHAESTUS_FQDN %q: must be a fully qualified DNS name", fqdn)
	}
	return fqdn, nil
}

// validateProvisioning parses the provisioning address and subnet, both IPv4,
// and requires the address to lie inside the subnet. The subnet is returned masked.
// Each problem is reported even if the other value is also bad.
func validateProvisioning(addrStr, subnetStr string) (netip.Addr, netip.Prefix, error) {
	var errs []error

	var addr netip.Addr
	switch parsed, err := netip.ParseAddr(addrStr); {
	case addrStr == "":
		errs = append(errs, errors.New("HEPHAESTUS_PROVISIONING_ADDRESS is required"))
	case err != nil || !parsed.Is4():
		errs = append(errs, fmt.Errorf("HEPHAESTUS_PROVISIONING_ADDRESS %q: must be an IPv4 address", addrStr))
	default:
		addr = parsed
	}

	var subnet netip.Prefix
	switch parsed, err := netip.ParsePrefix(subnetStr); {
	case subnetStr == "":
		errs = append(errs, errors.New("HEPHAESTUS_PROVISIONING_SUBNET is required"))
	case err != nil || !parsed.Addr().Is4():
		errs = append(errs, fmt.Errorf("HEPHAESTUS_PROVISIONING_SUBNET %q: must be an IPv4 CIDR prefix", subnetStr))
	default:
		subnet = parsed.Masked()
	}

	if addr.IsValid() && subnet.IsValid() && !subnet.Contains(addr) {
		errs = append(errs, fmt.Errorf("HEPHAESTUS_PROVISIONING_ADDRESS %s is outside HEPHAESTUS_PROVISIONING_SUBNET %s", addr, subnet))
	}
	return addr, subnet, errors.Join(errs...)
}

// interfaceAddrs returns the addresses assigned to the named network
// interface, or an error when it does not exist.
type interfaceAddrs func(name string) ([]netip.Addr, error)

// hostInterfaceAddrs is the interfaceAddrs of the host's network interfaces.
func hostInterfaceAddrs(name string) ([]netip.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			if addr, ok := netip.AddrFromSlice(ipNet.IP); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out, nil
}

// validateInterface requires the provisioning interface to be named and to
// exist, and, when address is valid, to hold it: proxyDHCP binds the interface
// and TFTP, HTTP and HTTPS bind the address, so either mismatch would otherwise
// only surface when the service starts listening.
func validateInterface(name string, address netip.Addr, addrsOf interfaceAddrs) error {
	if name == "" {
		return errors.New("HEPHAESTUS_PROVISIONING_INTERFACE is required")
	}
	addrs, err := addrsOf(name)
	if err != nil {
		return fmt.Errorf("HEPHAESTUS_PROVISIONING_INTERFACE %q: %w", name, err)
	}
	if address.IsValid() && !slices.Contains(addrs, address) {
		return fmt.Errorf("HEPHAESTUS_PROVISIONING_ADDRESS %s is not assigned to interface %s", address, name)
	}
	return nil
}

// validateAdmin parses the admin listen address and requires a specific
// management IP, on a real port, that is not inside the provisioning subnet
// (when that is valid).
func validateAdmin(raw string, subnet netip.Prefix) (netip.AddrPort, error) {
	ap, err := parseAddrPort("HEPHAESTUS_ADMIN_ADDRESS", raw)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if ap.Addr().IsUnspecified() {
		return netip.AddrPort{}, fmt.Errorf("HEPHAESTUS_ADMIN_ADDRESS %q: must be a specific management IP, not a wildcard", raw)
	}
	if subnet.IsValid() && subnet.Contains(ap.Addr()) {
		return netip.AddrPort{}, fmt.Errorf("HEPHAESTUS_ADMIN_ADDRESS %s is inside the provisioning subnet %s; the admin listener must stay off the VLAN", ap, subnet)
	}
	return ap, nil
}

// parseAddrPort parses the listen address in the environment variable name: an
// IP address and a port from 1 to 65535. An IPv4-mapped IPv6 address is
// unmapped, so [::ffff:10.10.20.9]:9090 is judged as the IPv4 address it is.
func parseAddrPort(name, raw string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(raw)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%s %q: must be IP:port", name, raw)
	}
	if ap.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("%s %q: port must be 1 to 65535", name, raw)
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// validateTLS checks the TLS settings against the mode and sets the mode's
// fields of cfg. A variable that belongs to the other mode is an error rather
// than ignored: a leftover HEPHAESTUS_TLS_CERT_FILE in proxy mode would
// otherwise let an operator believe the service serves that certificate.
func (r rawConfig) validateTLS(cfg *serveConfig) error {
	mode := r.TLS.Mode
	if mode != tlsModeFile && mode != tlsModeProxy {
		return errors.New(`HEPHAESTUS_TLS_MODE must be "file" or "proxy"`)
	}
	cfg.TLSMode = mode
	errs := []error{
		tlsSetting(mode, tlsModeFile, "HEPHAESTUS_TLS_CERT_FILE", r.TLS.CertFile != ""),
		tlsSetting(mode, tlsModeFile, "HEPHAESTUS_TLS_KEY_FILE", r.TLS.KeyFile != ""),
		tlsSetting(mode, tlsModeProxy, "HEPHAESTUS_PROXY_ADDRESS", r.Proxy.Address != ""),
		tlsSetting(mode, tlsModeProxy, "HEPHAESTUS_PROXY_TRUSTED", len(r.Proxy.Trusted) > 0),
	}
	if mode == tlsModeFile {
		cfg.TLSCertFile, cfg.TLSKeyFile = r.TLS.CertFile, r.TLS.KeyFile
		return errors.Join(errs...)
	}
	if r.Proxy.Address != "" {
		var err error
		cfg.ProxyAddr, err = parseAddrPort("HEPHAESTUS_PROXY_ADDRESS", r.Proxy.Address)
		errs = append(errs, err)
	}
	for _, entry := range r.Proxy.Trusted {
		entry = strings.TrimSpace(entry)
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			errs = append(errs, fmt.Errorf("HEPHAESTUS_PROXY_TRUSTED %q: must be a CIDR prefix", entry))
			continue
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, prefix.Masked())
	}
	return errors.Join(errs...)
}

// tlsSetting is the error for the environment variable name, which is set or
// not, under mode when usedBy is the mode that reads it: required there,
// only valid there.
func tlsSetting(mode, usedBy, name string, set bool) error {
	switch {
	case mode == usedBy && !set:
		return fmt.Errorf("%s is required when HEPHAESTUS_TLS_MODE is %s", name, mode)
	case mode != usedBy && set:
		return fmt.Errorf("%s is only valid when HEPHAESTUS_TLS_MODE is %s", name, usedBy)
	}
	return nil
}
