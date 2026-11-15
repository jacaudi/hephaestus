package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

const (
	testDataKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	testCFToken = "cf-test-token"
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
	t.Setenv("HEPHAESTUS_DATA_KEY", testDataKey)
	t.Setenv("HEPHAESTUS_CLOUDFLARE_TOKEN", testCFToken)
	return dir
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

// writeFile writes content to a new file in a temp dir and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
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
	if want := "https://acme-v02.api.letsencrypt.org/directory"; got.ACMEDirectoryURL != want {
		t.Errorf("ACMEDirectoryURL = %q, want %q", got.ACMEDirectoryURL, want)
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
	if got.CloudflareToken.Expose() != testCFToken {
		t.Errorf("CloudflareToken = %q, want %q", got.CloudflareToken.Expose(), testCFToken)
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
		"set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE",
		"set exactly one of HEPHAESTUS_CLOUDFLARE_TOKEN or HEPHAESTUS_CLOUDFLARE_TOKEN_FILE",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLoadConfig_Invalid(t *testing.T) {
	keyFile := writeFile(t, testDataKey)
	emptyFile := writeFile(t, "\n")

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
		{"short key", map[string]string{"DATA_KEY": "AAECAwQFBgcICQoLDA0ODw=="}, "decoded 16 bytes, want 32"},
		{"both keys", map[string]string{"DATA_KEY_FILE": keyFile}, "set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE"},
		{"missing key file", map[string]string{"DATA_KEY": "", "DATA_KEY_FILE": "/nonexistent"}, "read HEPHAESTUS_DATA_KEY_FILE"},
		{"empty token file", map[string]string{"CLOUDFLARE_TOKEN": "", "CLOUDFLARE_TOKEN_FILE": emptyFile}, "HEPHAESTUS_CLOUDFLARE_TOKEN_FILE: file is empty"},
		{"both tokens", map[string]string{"CLOUDFLARE_TOKEN_FILE": emptyFile}, "set exactly one of HEPHAESTUS_CLOUDFLARE_TOKEN or HEPHAESTUS_CLOUDFLARE_TOKEN_FILE"},
		{"mysql url", map[string]string{"DATABASE_URL": "mysql://x"}, "HEPHAESTUS_DATABASE_URL: unsupported scheme"},
		{"relative sqlite", map[string]string{"DATABASE_URL": "sqlite:relative.db"}, "HEPHAESTUS_DATABASE_URL: sqlite path must be absolute"},
		{"http acme", map[string]string{"ACME_DIRECTORY_URL": "http://acme.example.test/dir"}, `HEPHAESTUS_ACME_DIRECTORY_URL "http://acme.example.test/dir": must be an https URL`},
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

func TestLoadConfig_FileSecrets(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("HEPHAESTUS_DATA_KEY", "")
	t.Setenv("HEPHAESTUS_DATA_KEY_FILE", writeFile(t, testDataKey+"\n"))
	t.Setenv("HEPHAESTUS_CLOUDFLARE_TOKEN", "")
	t.Setenv("HEPHAESTUS_CLOUDFLARE_TOKEN_FILE", writeFile(t, "cf-file-token\n"))

	got, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := "cf-file-token"; got.CloudflareToken.Expose() != want {
		t.Errorf("CloudflareToken = %q, want %q", got.CloudflareToken.Expose(), want)
	}

	// A key parsed independently from the fixture must open what the loaded key seals.
	reference, err := datakey.Parse(secret.Secret(testDataKey))
	if err != nil {
		t.Fatalf("parse fixture key: %v", err)
	}
	opened, err := reference.Open(got.DataKey.Seal([]byte("payload"), nil), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := "payload"; string(opened) != want {
		t.Errorf("opened = %q, want %q", opened, want)
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
	setMinimalEnv(t)

	cfg, err := load(t)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	printed := fmt.Sprintf("%+v", cfg)
	for _, leaked := range []string{testCFToken, testDataKey} {
		if strings.Contains(printed, leaked) {
			t.Errorf("%%+v of the config contains %q: %s", leaked, printed)
		}
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
