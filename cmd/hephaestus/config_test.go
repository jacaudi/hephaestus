package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

const (
	testCertFile = "/certs/tls.crt"
	testKeyFile  = "/certs/tls.key"
)

// clearEnv blanks every inherited HEPHAESTUS_ variable: viper ignores an empty
// environment value, so the test sees defaults rather than the host's config.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "HEPHAESTUS_") {
			t.Setenv(name, "")
		}
	}
}

// setMinimalEnv sets the smallest valid configuration and returns the data dir.
func setMinimalEnv(t *testing.T) string {
	t.Helper()

	clearEnv(t)
	dir := t.TempDir()
	t.Setenv("HEPHAESTUS_FQDN", "hephaestus.example.test")
	t.Setenv("HEPHAESTUS_PROVISIONING_INTERFACE", "vlan20")
	t.Setenv("HEPHAESTUS_PROVISIONING_ADDRESS", "10.10.20.5")
	t.Setenv("HEPHAESTUS_PROVISIONING_SUBNET", "10.10.20.0/24")
	t.Setenv("HEPHAESTUS_DATA_DIR", dir)
	t.Setenv("HEPHAESTUS_TLS_CERT_FILE", testCertFile)
	t.Setenv("HEPHAESTUS_TLS_KEY_FILE", testKeyFile)
	return dir
}

// proxyModeEnv switches the minimal environment from file mode to proxy mode,
// with a valid proxy address and trusted list; override applies on top of it.
func proxyModeEnv(override map[string]string) map[string]string {
	env := map[string]string{
		"TLS_MODE": "proxy", "TLS_CERT_FILE": "", "TLS_KEY_FILE": "",
		"PROXY_ADDRESS": "127.0.0.1:8080", "PROXY_TRUSTED": "127.0.0.0/8",
	}
	maps.Copy(env, override)
	return env
}

// testInterfaces is the interface table the config tests see instead of the
// host's: vlan20 holds the fixture provisioning address, and nothing else exists.
func testInterfaces(name string) ([]netip.Addr, error) {
	if name == "vlan20" {
		return []netip.Addr{netip.MustParseAddr("fe80::1"), netip.MustParseAddr("10.10.20.5")}, nil
	}
	return nil, errors.New("no such network interface")
}

// load runs loadConfig against a flag set that carries --log-format, parsed from args.
func load(t *testing.T, args ...string) (serveConfig, error) {
	t.Helper()

	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("log-format", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags %v: %v", args, err)
	}
	return loadConfig(fs, testInterfaces)
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := setMinimalEnv(t)

	got, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if want := netip.MustParseAddrPort("127.0.0.1:9090"); got.AdminAddr != want {
		t.Errorf("AdminAddr = %v, want %v", got.AdminAddr, want)
	}
	if want := "file"; got.TLSMode != want {
		t.Errorf("TLSMode = %q, want the default %q", got.TLSMode, want)
	}
	if got.TLSCertFile != testCertFile || got.TLSKeyFile != testKeyFile {
		t.Errorf("TLS files = %q and %q, want %q and %q", got.TLSCertFile, got.TLSKeyFile, testCertFile, testKeyFile)
	}
	if got.ProxyAddr.IsValid() || got.TrustedProxies != nil {
		t.Errorf("proxy settings = %v and %v in file mode, want none", got.ProxyAddr, got.TrustedProxies)
	}
	if want := 60 * time.Second; got.ShutdownGrace != want {
		t.Errorf("ShutdownGrace = %v, want %v", got.ShutdownGrace, want)
	}
	if want := "json"; got.LogFormat != want {
		t.Errorf("LogFormat = %q, want %q", got.LogFormat, want)
	}
	if want := slog.LevelInfo; got.LogLevel != want {
		t.Errorf("LogLevel = %v, want %v", got.LogLevel, want)
	}
	if want := "sqlite:" + filepath.Join(dir, "hephaestus.db"); got.DatabaseURL.Expose() != want {
		t.Errorf("DatabaseURL = %q, want %q", got.DatabaseURL.Expose(), want)
	}
	if want := "hephaestus.example.test"; got.FQDN != want {
		t.Errorf("FQDN = %q, want %q", got.FQDN, want)
	}
	if want := netip.MustParseAddr("10.10.20.5"); got.Address != want {
		t.Errorf("Address = %v, want %v", got.Address, want)
	}
	if want := netip.MustParsePrefix("10.10.20.0/24"); got.Subnet != want {
		t.Errorf("Subnet = %v, want %v", got.Subnet, want)
	}
	if got.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, dir)
	}
}

// The go-standards §10 canary: a nested key with no default, set only through
// the environment, must decode. Plain viper.New() drops it silently.
func TestLoadConfig_EnvOnlyNestedKey(t *testing.T) {
	setMinimalEnv(t)

	got, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := "vlan20"; got.Interface != want {
		t.Errorf("Interface = %q, want %q", got.Interface, want)
	}
}

func TestLoadConfig_ReportsEveryProblem(t *testing.T) {
	clearEnv(t)
	t.Setenv("HEPHAESTUS_DATA_DIR", t.TempDir())

	_, err := load(t)
	if err == nil {
		t.Fatal("loadConfig succeeded with only DATA_DIR set")
	}

	for _, want := range []string{
		"HEPHAESTUS_FQDN is required",
		"HEPHAESTUS_PROVISIONING_INTERFACE is required",
		"HEPHAESTUS_PROVISIONING_ADDRESS is required",
		"HEPHAESTUS_PROVISIONING_SUBNET is required",
		"HEPHAESTUS_TLS_CERT_FILE is required when HEPHAESTUS_TLS_MODE is file",
		"HEPHAESTUS_TLS_KEY_FILE is required when HEPHAESTUS_TLS_MODE is file",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLoadConfig_Invalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"fqdn single label", map[string]string{"FQDN": "localhost"}, `HEPHAESTUS_FQDN "localhost": must be a fully qualified DNS name`},
		{"fqdn underscore", map[string]string{"FQDN": "bad_name.example.test"}, `HEPHAESTUS_FQDN "bad_name.example.test": must be a fully qualified DNS name`},
		{"fqdn too long", map[string]string{"FQDN": strings.Repeat("a.", 127) + "bc"}, "must be a fully qualified DNS name"},
		{"ipv6 address", map[string]string{"PROVISIONING_ADDRESS": "fe80::1"}, `HEPHAESTUS_PROVISIONING_ADDRESS "fe80::1": must be an IPv4 address`},
		{"bad prefix", map[string]string{"PROVISIONING_SUBNET": "10.10.20.0/33"}, `HEPHAESTUS_PROVISIONING_SUBNET "10.10.20.0/33": must be an IPv4 CIDR prefix`},
		{"address outside", map[string]string{"PROVISIONING_ADDRESS": "10.10.30.5"}, "HEPHAESTUS_PROVISIONING_ADDRESS 10.10.30.5 is outside HEPHAESTUS_PROVISIONING_SUBNET 10.10.20.0/24"},
		{"admin wildcard", map[string]string{"ADMIN_ADDRESS": "0.0.0.0:9090"}, `HEPHAESTUS_ADMIN_ADDRESS "0.0.0.0:9090": must be a specific management IP, not a wildcard`},
		{"admin on VLAN", map[string]string{"ADMIN_ADDRESS": "10.10.20.9:9090"}, "HEPHAESTUS_ADMIN_ADDRESS 10.10.20.9:9090 is inside the provisioning subnet 10.10.20.0/24; the admin listener must stay off the VLAN"},
		{"admin on VLAN as IPv4-mapped", map[string]string{"ADMIN_ADDRESS": "[::ffff:10.10.20.9]:9090"}, "HEPHAESTUS_ADMIN_ADDRESS 10.10.20.9:9090 is inside the provisioning subnet 10.10.20.0/24"},
		{"admin no port", map[string]string{"ADMIN_ADDRESS": "127.0.0.1"}, `HEPHAESTUS_ADMIN_ADDRESS "127.0.0.1": must be IP:port`},
		{"admin port zero", map[string]string{"ADMIN_ADDRESS": "127.0.0.1:0"}, `HEPHAESTUS_ADMIN_ADDRESS "127.0.0.1:0": port must be 1 to 65535`},
		{"unknown interface", map[string]string{"PROVISIONING_INTERFACE": "nosuch0"}, `HEPHAESTUS_PROVISIONING_INTERFACE "nosuch0": no such network interface`},
		{"address not on interface", map[string]string{"PROVISIONING_ADDRESS": "10.10.20.77"}, "HEPHAESTUS_PROVISIONING_ADDRESS 10.10.20.77 is not assigned to interface vlan20"},
		{"relative data dir", map[string]string{"DATA_DIR": "relative/dir"}, `HEPHAESTUS_DATA_DIR "relative/dir": must be an absolute path`},
		{"mysql url", map[string]string{"DATABASE_URL": "mysql://x"}, "HEPHAESTUS_DATABASE_URL: unsupported scheme"},
		{"relative sqlite", map[string]string{"DATABASE_URL": "sqlite:relative.db"}, "HEPHAESTUS_DATABASE_URL: sqlite path must be absolute"},
		{"unknown TLS mode", map[string]string{"TLS_MODE": "acme"}, `HEPHAESTUS_TLS_MODE must be "file" or "proxy"`},
		{"TLS mode is case sensitive", map[string]string{"TLS_MODE": "File"}, `HEPHAESTUS_TLS_MODE must be "file" or "proxy"`},
		{"file mode without a certificate", map[string]string{"TLS_CERT_FILE": ""}, "HEPHAESTUS_TLS_CERT_FILE is required when HEPHAESTUS_TLS_MODE is file"},
		{"file mode without a key", map[string]string{"TLS_KEY_FILE": ""}, "HEPHAESTUS_TLS_KEY_FILE is required when HEPHAESTUS_TLS_MODE is file"},
		{"file mode with a proxy address", map[string]string{"PROXY_ADDRESS": "127.0.0.1:8080"}, "HEPHAESTUS_PROXY_ADDRESS is only valid when HEPHAESTUS_TLS_MODE is proxy"},
		{"file mode with trusted proxies", map[string]string{"PROXY_TRUSTED": "127.0.0.0/8"}, "HEPHAESTUS_PROXY_TRUSTED is only valid when HEPHAESTUS_TLS_MODE is proxy"},
		{"proxy mode with a certificate", proxyModeEnv(map[string]string{"TLS_CERT_FILE": testCertFile}), "HEPHAESTUS_TLS_CERT_FILE is only valid when HEPHAESTUS_TLS_MODE is file"},
		{"proxy mode with a key", proxyModeEnv(map[string]string{"TLS_KEY_FILE": testKeyFile}), "HEPHAESTUS_TLS_KEY_FILE is only valid when HEPHAESTUS_TLS_MODE is file"},
		{"proxy mode without an address", proxyModeEnv(map[string]string{"PROXY_ADDRESS": ""}), "HEPHAESTUS_PROXY_ADDRESS is required when HEPHAESTUS_TLS_MODE is proxy"},
		{"proxy address without a port", proxyModeEnv(map[string]string{"PROXY_ADDRESS": "127.0.0.1"}), `HEPHAESTUS_PROXY_ADDRESS "127.0.0.1": must be IP:port`},
		{"proxy address with a host name", proxyModeEnv(map[string]string{"PROXY_ADDRESS": "localhost:8080"}), `HEPHAESTUS_PROXY_ADDRESS "localhost:8080": must be IP:port`},
		{"proxy address port zero", proxyModeEnv(map[string]string{"PROXY_ADDRESS": "127.0.0.1:0"}), `HEPHAESTUS_PROXY_ADDRESS "127.0.0.1:0": port must be 1 to 65535`},
		{"proxy mode without trusted proxies", proxyModeEnv(map[string]string{"PROXY_TRUSTED": ""}), "HEPHAESTUS_PROXY_TRUSTED is required when HEPHAESTUS_TLS_MODE is proxy"},
		{"trusted proxy not a prefix", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "127.0.0.0/8,10.0.0.1"}), `HEPHAESTUS_PROXY_TRUSTED "10.0.0.1": must be a CIDR prefix`},
		{"trusted proxy prefix too long", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "10.0.0.0/33"}), `HEPHAESTUS_PROXY_TRUSTED "10.0.0.0/33": must be a CIDR prefix`},
		{"trusted proxies with an empty entry", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "127.0.0.0/8,"}), `HEPHAESTUS_PROXY_TRUSTED "": must be a CIDR prefix`},
		{"trusted prefix contains the subnet", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "10.10.0.0/16"}), "HEPHAESTUS_PROXY_TRUSTED prefix 10.10.0.0/16 contains the provisioning subnet 10.10.20.0/24; list only the proxy's own addresses"},
		{"trusted prefix is the subnet", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "10.10.20.77/24"}), "HEPHAESTUS_PROXY_TRUSTED prefix 10.10.20.0/24 contains the provisioning subnet 10.10.20.0/24; list only the proxy's own addresses"},
		{"trusted prefix is everything", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "127.0.0.0/8,0.0.0.0/0"}), "HEPHAESTUS_PROXY_TRUSTED prefix 0.0.0.0/0 contains the provisioning subnet 10.10.20.0/24"},
		{"IPv4-mapped trusted prefix contains the subnet", proxyModeEnv(map[string]string{"PROXY_TRUSTED": "::ffff:10.10.0.0/112"}), "HEPHAESTUS_PROXY_TRUSTED prefix 10.10.0.0/16 contains the provisioning subnet 10.10.20.0/24"},
		{"zero grace", map[string]string{"SHUTDOWN_GRACE": "0s"}, "HEPHAESTUS_SHUTDOWN_GRACE must be positive"},
		{"xml logs", map[string]string{"LOG_FORMAT": "xml"}, `HEPHAESTUS_LOG_FORMAT "xml": must be json or text`},
		{"bad level", map[string]string{"LOG_LEVEL": "loud"}, "decode config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMinimalEnv(t)
			for k, v := range tt.env {
				t.Setenv("HEPHAESTUS_"+k, v)
			}

			_, err := load(t)
			if err == nil {
				t.Fatal("loadConfig succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A relative data dir must not also produce a database URL error about the
// default sqlite path derived from it.
func TestLoadConfig_RelativeDataDirReportsOnlyTheDir(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_DATA_DIR", "relative/dir")

	_, err := load(t)
	if err == nil {
		t.Fatal("loadConfig succeeded, want an error")
	}
	if strings.Contains(err.Error(), "HEPHAESTUS_DATABASE_URL") {
		t.Errorf("error = %q, want no DATABASE_URL complaint", err)
	}
}

// A missing interface must not also produce an address complaint derived from it.
func TestLoadConfig_UnknownInterfaceReportsOnlyTheInterface(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_PROVISIONING_INTERFACE", "nosuch0")

	_, err := load(t)
	if err == nil {
		t.Fatal("loadConfig succeeded, want an error")
	}
	if strings.Contains(err.Error(), "is not assigned to interface") {
		t.Errorf("error = %q, want no address complaint", err)
	}
}

// TestHostInterfaceAddrs runs the host lookup serve uses against the host's
// loopback interface, found by flag so no NIC name is assumed.
func TestHostInterfaceAddrs(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	i := slices.IndexFunc(ifaces, func(i net.Interface) bool { return i.Flags&net.FlagLoopback != 0 })
	if i < 0 {
		t.Skip("host has no loopback interface")
	}

	got, err := hostInterfaceAddrs(ifaces[i].Name)
	if err != nil {
		t.Fatalf("hostInterfaceAddrs(%q): %v", ifaces[i].Name, err)
	}
	if want := netip.MustParseAddr("127.0.0.1"); !slices.Contains(got, want) {
		t.Errorf("hostInterfaceAddrs(%q) = %v, want it to contain %v", ifaces[i].Name, got, want)
	}

	if _, err := hostInterfaceAddrs("nosuch0"); err == nil {
		t.Error("hostInterfaceAddrs(nosuch0) succeeded, want an error")
	}
}

func TestLoadConfig_ProxyMode(t *testing.T) {
	setMinimalEnv(t)
	for k, v := range proxyModeEnv(map[string]string{
		"PROXY_ADDRESS": "10.10.20.7:8080",                                   // it need not be off the provisioning VLAN
		"PROXY_TRUSTED": "10.0.0.5/16, 192.168.1.7/24,::1/128,10.10.20.9/32", // the last is a proxy on the VLAN itself
	}) {
		t.Setenv("HEPHAESTUS_"+k, v)
	}

	got, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if want := "proxy"; got.TLSMode != want {
		t.Errorf("TLSMode = %q, want %q", got.TLSMode, want)
	}
	if want := netip.MustParseAddrPort("10.10.20.7:8080"); got.ProxyAddr != want {
		t.Errorf("ProxyAddr = %v, want %v", got.ProxyAddr, want)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("10.10.20.9/32")}
	if !slices.Equal(got.TrustedProxies, want) {
		t.Errorf("TrustedProxies = %v, want the masked prefixes %v", got.TrustedProxies, want)
	}
	if got.TLSCertFile != "" || got.TLSKeyFile != "" {
		t.Errorf("TLS files = %q and %q in proxy mode, want none", got.TLSCertFile, got.TLSKeyFile)
	}
}

// TestLoadConfig_ProxyTrustedUnmapsIPv4Mapped pins that a trusted prefix written
// in IPv4-mapped form is stored as the IPv4 prefix it means: peers are unmapped
// before they are matched, so a mapped prefix would otherwise never match.
func TestLoadConfig_ProxyTrustedUnmapsIPv4Mapped(t *testing.T) {
	tests := []struct {
		trusted string
		want    string
	}{
		{"::ffff:10.0.0.1/128", "10.0.0.1/32"},
		{"::ffff:10.0.1.7/120", "10.0.1.0/24"},
	}
	for _, tt := range tests {
		t.Run(tt.trusted, func(t *testing.T) {
			setMinimalEnv(t)
			for k, v := range proxyModeEnv(map[string]string{"PROXY_TRUSTED": tt.trusted}) {
				t.Setenv("HEPHAESTUS_"+k, v)
			}

			got, err := load(t)

			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if want := []netip.Prefix{netip.MustParsePrefix(tt.want)}; !slices.Equal(got.TrustedProxies, want) {
				t.Errorf("TrustedProxies = %v, want %v", got.TrustedProxies, want)
			}
		})
	}
}

func TestLoadConfig_ProxyModeReportsEveryProblem(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_TLS_MODE", "proxy") // the file variables stay set; no proxy variable is

	_, err := load(t)
	if err == nil {
		t.Fatal("loadConfig succeeded with proxy mode and no proxy settings")
	}

	for _, want := range []string{
		"HEPHAESTUS_PROXY_ADDRESS is required when HEPHAESTUS_TLS_MODE is proxy",
		"HEPHAESTUS_PROXY_TRUSTED is required when HEPHAESTUS_TLS_MODE is proxy",
		"HEPHAESTUS_TLS_CERT_FILE is only valid when HEPHAESTUS_TLS_MODE is file",
		"HEPHAESTUS_TLS_KEY_FILE is only valid when HEPHAESTUS_TLS_MODE is file",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLoadConfig_Normalizes(t *testing.T) {
	const postgresURL = "postgres://u:p@db.example.test:5432/heph"

	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_FQDN", "Hephaestus.Example.Test")
	t.Setenv("HEPHAESTUS_PROVISIONING_SUBNET", "10.10.20.7/24")
	t.Setenv("HEPHAESTUS_DATABASE_URL", postgresURL)

	got, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := "hephaestus.example.test"; got.FQDN != want {
		t.Errorf("FQDN = %q, want %q", got.FQDN, want)
	}
	if want := netip.MustParsePrefix("10.10.20.0/24"); got.Subnet != want {
		t.Errorf("Subnet = %v, want %v", got.Subnet, want)
	}
	if got.DatabaseURL.Expose() != postgresURL {
		t.Errorf("DatabaseURL = %q, want %q", got.DatabaseURL.Expose(), postgresURL)
	}
}

func TestLoadConfig_SecretsNeverPrinted(t *testing.T) {
	const password = "s3cr3t-db-password"

	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_DATABASE_URL", "postgres://u:"+password+"@db.example.test:5432/heph")

	cfg, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if printed := fmt.Sprintf("%+v", cfg); strings.Contains(printed, password) {
		t.Errorf("%%+v of the config contains the database password: %s", printed)
	}
}

func TestLoadConfig_LogFormatFlag(t *testing.T) {
	tests := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"flag overrides env", "json", []string{"--log-format=text"}, "text"},
		{"flag unset, env json", "json", nil, "json"},
		{"flag unset, env text", "text", nil, "text"},
		{"flag unset, env unset", "", nil, "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv("HEPHAESTUS_LOG_FORMAT", tt.env)

			got, err := load(t, tt.args...)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got.LogFormat != tt.want {
				t.Errorf("LogFormat = %q, want %q", got.LogFormat, tt.want)
			}
		})
	}
}
