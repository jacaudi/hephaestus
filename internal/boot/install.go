package boot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// ConfigSource is what /boot asks of the approved cluster definitions for an
// assigned machine. *clusters.Service implements it.
type ConfigSource interface {
	// Drift reports whether the approved render of the machine's host records drift.
	Drift(ctx context.Context, m fleet.Machine) (bool, error)
	// EnsureAssets creates the talos_cache row the machine installs from when it
	// is absent. A machine not bound to a host of an approved pair is
	// clusters.ErrNotFound.
	EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error
	// Approved returns the cluster's approved pair, clusters.ErrNotFound when it has none.
	Approved(ctx context.Context, clusterID string) (clusters.Pair, error)
	// Render returns the machine config of the ticket's host.
	Render(ctx context.Context, t fleet.Ticket) ([]byte, error)
}

// MaxCommandLine is the longest kernel command line the install script may
// compose: iPXE's BIOS bzImage loader passes at most this many bytes
// (bzimage.c:314), the lowest bound of every platform.
const MaxCommandLine = 2046

// ErrCommandLineTooLong is returned by InstallScript when the command line it
// would compose is over MaxCommandLine bytes: a truncated line would lose the
// config URL.
var ErrCommandLineTooLong = errors.New("boot: kernel command line too long")

const (
	// kernelName is the name the install script loads the kernel under. On EFI
	// iPXE passes "<name> <cmdline>" to the kernel, so the name counts.
	kernelName = "k"
	// talosArgs follow the cached command line; the FQDN comes next.
	talosArgs = " talos.halt_if_installed=1 talos.config=https://"
	// configQuery follows the FQDN; the UUID iPXE expands, then ticketParam
	// and the ticket.
	configQuery = "/config?u="
	ticketParam = "&t="
	// uuidChars and ticketChars are what ${uuid} and a ticket token expand to.
	uuidChars   = 36
	ticketChars = 43

	reasonCmdlineTooLong = "kernel command line too long"
	reasonMachineChanged = "machine changed while deciding"
	alertKindCmdline     = "cmdline_too_long"
	actionCmdlineAlert   = "alert.cmdline_too_long"
)

// InstallRequest is what the install script names: the provisioning address the
// Talos artifacts are fetched from, the service's FQDN, the Talos row's key,
// the cached kernel command line and the ticket's token.
type InstallRequest struct {
	Address                  netip.Addr
	FQDN, Schematic, Version string
	Arch                     fleet.Arch
	Cmdline, Token           string
}

// InstallScript returns the iPXE script that boots the Talos installer for req
// with its config URL. It returns ErrCommandLineTooLong when the kernel command
// line, counted with the kernel's name, a 36-character UUID and a 43-character
// ticket, is over MaxCommandLine bytes. Request facts appear only as iPXE
// variables.
func InstallScript(req InstallRequest) (string, error) {
	if n := commandLineLength(req.Cmdline, req.FQDN); n > MaxCommandLine {
		return "", fmt.Errorf("%w: %d bytes, over %d", ErrCommandLineTooLong, n, MaxCommandLine)
	}
	base := "http://" + req.Address.String() + "/t/" + req.Schematic + "/" + req.Version + "/" + string(req.Arch) + "/"
	return ipxeHeader + "imgfree\n" +
		"kernel --name " + kernelName + " " + base + string(fleet.TalosKernel) + " " + req.Cmdline +
		talosArgs + req.FQDN + configQuery + "${uuid}" + ticketParam + req.Token + "\n" +
		"initrd " + base + string(fleet.TalosInitramfs) + "\n" +
		"boot\n", nil
}

// commandLineLength is the length of the command line the kernel receives from
// the install script for cmdline and fqdn.
func commandLineLength(cmdline, fqdn string) int {
	return len(kernelName) + 1 + len(cmdline) + len(talosArgs) + len(fqdn) + len(configQuery) + uuidChars + len(ticketParam) + ticketChars
}

// talosKeyOf returns the talos_cache key of m's Talos tuple and arch, and false
// when m has no tuple.
func talosKeyOf(m fleet.Machine) (fleet.TalosKey, bool) {
	if m.TalosSchematic == nil || m.TalosVersion == nil {
		return fleet.TalosKey{}, false
	}
	return fleet.TalosKey{Schematic: *m.TalosSchematic, Version: *m.TalosVersion, Arch: m.Arch}, true
}

// assignedFacts fills in's AssetsReady and Drift for an assigned machine with a
// Talos tuple, when a ConfigSource is set, and returns the cached command line
// of a ready row. Drift is asked only of a machine whose assets are ready, the
// only one row 9 looks at.
func (h *Handlers) assignedFacts(ctx context.Context, in *Input) (string, error) {
	key, ok := talosKeyOf(in.Machine)
	if !ok || h.configs == nil || in.Machine.State != fleet.StateAssigned {
		return "", nil
	}
	cmdline, ready, err := h.talosReady(ctx, key, in.Now)
	if err != nil || !ready {
		return "", err
	}
	in.AssetsReady = true
	if in.Drift, err = h.configs.Drift(ctx, in.Machine); err != nil {
		return "", fmt.Errorf("boot: drift of %s: %w", in.Machine.ID, err)
	}
	return cmdline, nil
}

// talosReady reports whether the row at key is ready and its tag in the store
// holds the content it records, and returns the cached command line, read to
// at most MaxCommandLine+1 bytes (any longer line cannot fit). A row whose
// content the store lacks, or holds other content than recorded, is put back
// to wanted and errTalosMissing is reported; one the store cannot read is
// reported and left alone. A failure to read the row is returned.
func (h *Handlers) talosReady(ctx context.Context, key fleet.TalosKey, now time.Time) (string, bool, error) {
	row, found, err := h.repo.Talos(ctx, key)
	if err != nil {
		return "", false, err
	}
	if !found || row.State != fleet.CacheReady {
		return "", false, nil
	}
	cmdline, err := h.readCmdline(ctx, row)
	if err != nil {
		h.talosUnusable(ctx, key, err, now)
		return "", false, nil
	}
	return cmdline, true, nil
}

// readCmdline checks every layer of row's tag against the store and row's
// recorded assets, then reads the command line layer.
func (h *Handlers) readCmdline(ctx context.Context, row fleet.TalosRow) (string, error) {
	tag := row.Key().Tag()
	if err := h.content.CheckLayers(ctx, tag); err != nil {
		return "", err
	}
	layers, err := h.content.Layers(ctx, tag)
	if err != nil {
		return "", err
	}
	var cmdline ocispec.Descriptor
	for _, p := range fleet.TalosParts() {
		l, err := talosLayer(layers, row, p)
		if err != nil {
			return "", err
		}
		if p == fleet.TalosCmdline {
			cmdline = l
		}
	}
	blob, err := h.content.OpenBlob(cmdline)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(blob, MaxCommandLine+1))
	if err = errors.Join(err, blob.Close()); err != nil {
		return "", fmt.Errorf("read the command line of %s: %w", tag, err)
	}
	return string(b), nil
}

// installCheck turns an install decision whose command line cannot fit into a
// row 10 hold; the decision's other fields are kept. It reports the length
// that did not fit, 0 when the line fits or d is not an install.
func (h *Handlers) installCheck(d *Decision, m fleet.Machine, cmdline string) int {
	if !d.NeedsTicket {
		return 0
	}
	_, err := InstallScript(h.installRequest(m, cmdline, strings.Repeat("x", ticketChars)))
	if !errors.Is(err, ErrCommandLineTooLong) {
		return 0
	}
	d.Action, d.Reason, d.NeedsTicket = ActionHold, reasonCmdlineTooLong, false
	return commandLineLength(cmdline, h.fqdn)
}

// installRequest is the install script request for m with cmdline and token.
func (h *Handlers) installRequest(m fleet.Machine, cmdline, token string) InstallRequest {
	key, _ := talosKeyOf(m) // an install decision is only made for a machine with a tuple
	return InstallRequest{
		Address: h.address, FQDN: h.fqdn, Schematic: key.Schematic, Version: key.Version, Arch: key.Arch,
		Cmdline: cmdline, Token: token,
	}
}

// approvedPair returns the approved pair of m's cluster, the pair an install's
// ticket binds.
func (h *Handlers) approvedPair(ctx context.Context, m fleet.Machine) (clusters.Pair, error) {
	if m.ClusterID == nil || m.HostName == nil {
		return clusters.Pair{}, fmt.Errorf("boot: install of %s: the machine is not bound to a host", m.ID)
	}
	pair, err := h.configs.Approved(ctx, *m.ClusterID)
	if err != nil {
		return clusters.Pair{}, fmt.Errorf("boot: install of %s: %w", m.ID, err)
	}
	return pair, nil
}

// installScript mints a ticket for the machine out decided on, bound to its
// host and the approved pair read when deciding, and returns the install script
// that carries it. The mint is refused with fleet.ErrMachineChanged when the
// machine's state, binding or Talos tuple changed since the decision, as a
// cluster approval landing in between does.
func (h *Handlers) installScript(ctx context.Context, out bootOutcome) (string, error) {
	m := out.in.Machine
	req := h.installRequest(m, out.cmdline, "")
	token, _, err := h.repo.MintTicket(ctx, fleet.MintRequest{
		MachineID: m.ID, ClusterID: *m.ClusterID, HostName: *m.HostName,
		PairID: out.pair.ID, SecretVersionID: out.pair.SecretVersionID,
		TalosSchematic: req.Schematic, TalosVersion: req.Version, Arch: req.Arch, At: out.in.Now,
	})
	if err != nil {
		return "", fmt.Errorf("boot: install of %s: %w", m.ID, err)
	}
	h.minted.Add(ctx, 1)
	req.Token = token
	return InstallScript(req)
}

// ensureAssets asks for the row the machine installs from to exist, as row 8
// does. A machine not bound to an approved host has nothing to fetch; any other
// failure is reported, and the machine still holds.
func (h *Handlers) ensureAssets(ctx context.Context, m fleet.Machine, at time.Time) {
	if err := h.configs.EnsureAssets(ctx, m, at); err != nil && !errors.Is(err, clusters.ErrNotFound) {
		h.onError(ctx, fmt.Errorf("boot: ensure assets of %s: %w", m.ID, err))
	}
}

// cmdlineDetail is the detail of an alert.cmdline_too_long audit event.
type cmdlineDetail struct {
	MachineID string `json:"machine_id"`
	Length    int    `json:"length"`
}

// alertCmdline raises the cmdline_too_long alert for the machine: an audit
// event and the alert counter. A failure to audit is reported.
func (h *Handlers) alertCmdline(ctx context.Context, m fleet.Machine, length int, at time.Time) {
	h.countAlert(ctx, alertKindCmdline)
	ev := fleet.AuditEvent{
		At: at, Actor: fleet.ActorSystem, Action: actionCmdlineAlert, MachineID: m.ID,
		Detail: cmdlineDetail{MachineID: m.ID, Length: length},
	}
	if err := h.repo.Audit(ctx, ev); err != nil {
		h.onError(ctx, fmt.Errorf("boot: audit %s of %s (%d bytes): %w", actionCmdlineAlert, m.ID, length, err))
	}
}
