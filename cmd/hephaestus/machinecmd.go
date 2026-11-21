package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// noHostReason is why "machine fetch" has nothing to fetch for a machine without a host.
const noHostReason = "machine is not bound to a host"

// actionFetchRequested is the audit action of a "machine fetch" that wants a Talos row; with --reset-hash the reset is
// audited as talos.hash_reset by fleet.
const actionFetchRequested = "talos.fetch_requested"

// machineRow is a machine as "machine list", "machine show" and the lifecycle commands print it. Cluster is the
// cluster's name.
type machineRow struct {
	ID             string   `json:"id"`
	UUID           string   `json:"uuid"`
	MACs           []string `json:"macs"`
	Serial         string   `json:"serial"`
	Product        string   `json:"product"`
	Arch           string   `json:"arch"`
	State          string   `json:"state"`
	PriorState     string   `json:"prior_state"`
	OrphanedFrom   string   `json:"orphaned_from"`
	Cluster        string   `json:"cluster"`
	Host           string   `json:"host"`
	TalosSchematic string   `json:"talos_schematic"`
	TalosVersion   string   `json:"talos_version"`
	Junk           bool     `json:"junk"`
	LastSeenAt     string   `json:"last_seen_at"`
}

// printable is r with the text the machine reported cleaned for JSON, which prints the runes encoding/json leaves
// raw; the text output escapes them with safeText instead.
func (r machineRow) printable() machineRow {
	r.Serial, r.Product = fleet.PrintableText(r.Serial), fleet.PrintableText(r.Product)
	return r
}

// clusterHost is the cluster and host of r as "cluster/host", or "" when r has none.
func (r machineRow) clusterHost() string {
	if r.Cluster == "" {
		return ""
	}
	return r.Cluster + "/" + r.Host
}

// machineRowOf is m as a row. names maps cluster ids to cluster names; an id it lacks is printed as it is.
func machineRowOf(m *fleet.Machine, names map[string]string) machineRow {
	r := machineRow{
		ID: m.ID, UUID: m.UUID, MACs: append(make([]string, 0, len(m.MACs)), m.MACs...), Serial: m.Serial, Product: m.Product,
		Arch: string(m.Arch), State: stateLabel(m.State), PriorState: string(m.Prior), OrphanedFrom: string(m.OrphanedFrom),
		Junk: m.Junk, LastSeenAt: formatTime(m.LastSeenAt),
	}
	if m.ClusterID != nil {
		r.Cluster = *m.ClusterID
		if name, ok := names[r.Cluster]; ok {
			r.Cluster = name
		}
	}
	if m.HostName != nil {
		r.Host = *m.HostName
	}
	if m.TalosSchematic != nil {
		r.TalosSchematic = *m.TalosSchematic
	}
	if m.TalosVersion != nil {
		r.TalosVersion = *m.TalosVersion
	}
	return r
}

// stateLabel names a state as the audit log does, where no record reads "none".
func stateLabel(s fleet.State) string {
	if s == fleet.StateNone {
		return "none"
	}
	return string(s)
}

// clusterNames maps the id of every cluster to its name.
func clusterNames(ctx context.Context, repo *clusters.Repository) (map[string]string, error) {
	list, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(list))
	for i := range list {
		names[list[i].ID] = list[i].Name
	}
	return names, nil
}

// machineChange is what a lifecycle command reports: the machine as it is now and the state it was in.
type machineChange struct {
	machineRow

	FromState string `json:"from_state"`
}

// talosState is the talos_cache row a machine installs from, as "machine show" reports it.
type talosState struct {
	State          string `json:"state"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error"`
	AssetsRecorded bool   `json:"assets_recorded"`
}

// ticketState is a machine's live ticket as "machine show" reports it: never its token or its hash. A ticket that
// is minted and has not been asked for is the likeliest first-install failure.
type ticketState struct {
	MintedAt          string `json:"minted_at"`
	ExpiresAt         string `json:"expires_at"`
	ConfigRequested   bool   `json:"config_requested"`
	ConfigRequestedAt string `json:"config_requested_at"`
}

// machineShow is what "machine show" reports. Talos and Ticket are null when the machine has none.
type machineShow struct {
	machineRow

	Talos  *talosState  `json:"talos"`
	Ticket *ticketState `json:"ticket"`
}

// talosFetchDetail is the detail of a talos.fetch_requested audit event, shaped as fleet's talos.hash_reset detail.
type talosFetchDetail struct {
	Schematic string `json:"schematic"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
}

// machineFetched is what "machine fetch" reports. Wanted is false, with a Reason, when the machine has no host and so
// nothing to fetch; ResetHash is true when the recorded sizes and hashes of an existing row were cleared.
type machineFetched struct {
	MachineID string `json:"machine_id"`
	Schematic string `json:"schematic"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
	Wanted    bool   `json:"wanted"`
	ResetHash bool   `json:"reset_hash"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Reason    string `json:"reason"`
}

// newMachineCmd builds the machine command group.
func newMachineCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "List machines and move them through their lifecycle",
		Long: `List and show machines, bind them to hosts of an approved cluster pair, and move them through
their lifecycle: one command per admin action. A machine is named by its MAC or its UUID, and must
have booted through hephaestus once (adopt --uuid creates one).

These commands run inside the container and write the database that "hephaestus serve" reads. Every
change is audited with the actor cli.`,
		Example: `  docker exec hephaestus /hephaestus machine list
  docker exec hephaestus /hephaestus machine approve 52:54:00:12:34:56 lab/cp-01`,
		Args: noArgs,
		RunE: helpGroup,
	}
	cmd.AddCommand(newMachineListCmd(deps), newMachineShowCmd(deps), newMachineFetchCmd(deps),
		newMachineApproveCmd(deps), newMachineAdoptCmd(deps), newMachineRebindCmd(deps))
	for _, spec := range transitionSpecs() {
		cmd.AddCommand(newTransitionCmd(deps, spec))
	}
	return cmd
}

func newMachineListCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the machines",
		Long: `List every machine, oldest first: its id, MACs, UUID, architecture, state, the host it is bound to
and when it was last seen.`,
		Example: `  hephaestus machine list
  hephaestus machine list --json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				list, err := s.Fleet.ListMachines(cmd.Context())
				if err != nil {
					return err
				}
				names, err := clusterNames(cmd.Context(), s.Clusters)
				if err != nil {
					return err
				}
				rows := machineRows(list, names)
				if asJSON {
					for i := range rows {
						rows[i] = rows[i].printable()
					}
					return writeJSON(cmd, rows)
				}
				return writeMachineRows(cmd, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the machines as JSON")
	return cmd
}

// machineRows is machines as rows; JSON must print [], not null.
func machineRows(machines []fleet.Machine, names map[string]string) []machineRow {
	rows := make([]machineRow, 0, len(machines))
	for i := range machines {
		rows = append(rows, machineRowOf(&machines[i], names))
	}
	return rows
}

// writeMachineRows prints rows as a table, or a note when there are none.
func writeMachineRows(cmd *cobra.Command, rows []machineRow) error {
	if len(rows) == 0 {
		return writeText(cmd, "no machines\n")
	}
	table := make([][]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		table = append(table, []string{
			r.ID, dash(strings.Join(r.MACs, ",")), dash(r.UUID), r.Arch, r.State, dash(r.clusterHost()), dash(r.LastSeenAt),
		})
	}
	return writeTable(cmd, []string{"ID", "MACS", "UUID", "ARCH", "STATE", "HOST", "LAST_SEEN"}, table)
}

func newMachineShowCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <mac|uuid>",
		Short: "Show a machine, its binding, its Talos assets and its ticket",
		Long: `Show one machine: its state, its binding and Talos tuple, the state of the Talos assets it installs
from, and its live config ticket: when it was minted and whether /config was ever asked for with it. A
ticket minted long ago and never asked for is the likeliest first-install failure (DNS, the node's
clock, TLS trust). The ticket's token and hash are never shown.`,
		Example: `  hephaestus machine show 52:54:00:12:34:56
  hephaestus machine show 4c4c4544-0042-3510-8052-b4c04f4e4d32 --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				m, err := resolveMachine(cmd, s.Fleet, args[0])
				if err != nil {
					return err
				}
				show, err := machineShowOf(cmd.Context(), s, &m)
				if err != nil {
					return err
				}
				if asJSON {
					show.machineRow = show.printable()
					if show.Talos != nil {
						show.Talos.LastError = fleet.PrintableText(show.Talos.LastError)
					}
					return writeJSON(cmd, show)
				}
				return writeText(cmd, show.text())
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the machine as JSON")
	return cmd
}

// machineShowOf is m with its Talos row and its live ticket.
func machineShowOf(ctx context.Context, s cliStores, m *fleet.Machine) (machineShow, error) {
	names, err := clusterNames(ctx, s.Clusters)
	if err != nil {
		return machineShow{}, err
	}
	show := machineShow{machineRow: machineRowOf(m, names)}
	if m.TalosSchematic != nil && m.TalosVersion != nil {
		row, ok, err := s.Fleet.Talos(ctx, talosKeyOf(m))
		if err != nil {
			return machineShow{}, err
		}
		if ok {
			show.Talos = &talosState{State: string(row.State), Attempts: row.Attempts, LastError: row.LastError, AssetsRecorded: row.Assets.Complete()}
		}
	}
	tk, ok, err := s.Fleet.LiveTicket(ctx, m.ID)
	if err != nil {
		return machineShow{}, err
	}
	if ok {
		show.Ticket = &ticketState{
			MintedAt: formatTime(tk.MintedAt), ExpiresAt: formatTime(tk.ExpiresAt),
			ConfigRequested: !tk.RequestedAt.IsZero(), ConfigRequestedAt: formatTime(tk.RequestedAt),
		}
	}
	return show, nil
}

// talosKeyOf is the key of the talos_cache row of m, which must have a Talos tuple.
func talosKeyOf(m *fleet.Machine) fleet.TalosKey {
	return fleet.TalosKey{Schematic: *m.TalosSchematic, Version: *m.TalosVersion, Arch: m.Arch}
}

// text is s as "machine show" prints it, one field a line.
func (s machineShow) text() string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%s: %s\n", label, dash(value)) }
	line("machine", s.ID)
	line("state", s.State)
	line("prior state", s.PriorState)
	line("orphaned from", s.OrphanedFrom)
	line("uuid", s.UUID)
	line("macs", strings.Join(s.MACs, ", "))
	line("serial", s.Serial)
	line("product", s.Product)
	line("arch", s.Arch)
	line("junk", strconv.FormatBool(s.Junk))
	line("host", s.clusterHost())
	line("talos", strings.TrimSpace(s.TalosSchematic+" "+s.TalosVersion))
	if s.Talos == nil {
		line("talos assets", "none")
	} else {
		assets := fmt.Sprintf("%s, %d attempts, hashes recorded: %t", s.Talos.State, s.Talos.Attempts, s.Talos.AssetsRecorded)
		if s.Talos.LastError != "" {
			assets += ", last error: " + s.Talos.LastError
		}
		line("talos assets", assets)
	}
	switch {
	case s.Ticket == nil:
		line("ticket", "none")
	case s.Ticket.ConfigRequested:
		line("ticket", fmt.Sprintf("minted %s, expires %s, config requested: yes, at %s", s.Ticket.MintedAt, s.Ticket.ExpiresAt, s.Ticket.ConfigRequestedAt))
	default:
		line("ticket", fmt.Sprintf("minted %s, expires %s, config requested: no", s.Ticket.MintedAt, s.Ticket.ExpiresAt))
	}
	line("last seen", s.LastSeenAt)
	return b.String()
}

func newMachineFetchCmd(deps cliDeps) *cobra.Command {
	var asJSON, resetHash bool
	cmd := &cobra.Command{
		Use:   "fetch <mac|uuid> [--reset-hash]",
		Short: "Ask serve to fetch the Talos assets a machine installs from",
		Long: `Want the Talos assets of the machine's host: the row is created when absent, with the host's
schematic, and a failed or broken row is moved back to wanted with its attempts reset. Serve fetches
it within seconds. The recorded sizes and hashes are kept, so a changed upstream build still fails.

With --reset-hash the recorded sizes and hashes are cleared and the row is wanted from any state,
ready included, so the next fetch downloads and records afresh: a changed upstream build is a trust
decision, and this is how an admin accepts it. It is audited.

A machine that is not bound to a host has nothing to fetch; the command says so and exits 0.`,
		Example: `  hephaestus machine fetch 52:54:00:12:34:56
  hephaestus machine fetch 52:54:00:12:34:56 --reset-hash --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				m, err := resolveMachine(cmd, s.Fleet, args[0])
				if err != nil {
					return err
				}
				out, err := fetchAssets(cmd.Context(), s, &m, resetHash, deps.now)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd, out)
				}
				return writeText(cmd, out.text())
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().BoolVar(&resetHash, "reset-hash", false, "clear the recorded sizes and hashes, so the next fetch records afresh")
	return cmd
}

// fetchAssets wants the Talos row of m. With resetHash an existing row is reset first, and a row that is absent is
// wanted as it is without. A machine without a host, or whose host the approved pair lacks, has nothing to fetch.
func fetchAssets(ctx context.Context, s cliStores, m *fleet.Machine, resetHash bool, now func() time.Time) (machineFetched, error) {
	out := machineFetched{MachineID: m.ID, Arch: string(m.Arch), Reason: noHostReason}
	if m.ClusterID == nil || m.TalosSchematic == nil || m.TalosVersion == nil {
		return out, nil
	}
	key := talosKeyOf(m)
	out.Schematic, out.Version = key.Schematic, key.Version
	if resetHash {
		moved, err := s.Fleet.ResetTalosHash(ctx, key, fleet.ActorCLI, now())
		if err != nil {
			return machineFetched{}, err
		}
		out.ResetHash = moved
	}
	if !out.ResetHash {
		err := s.Clusters.WantAssets(ctx, *m, now())
		if errors.Is(err, clusters.ErrNotFound) {
			return out, nil // not bound to a host of an approved pair: nothing to fetch, which is no failure
		}
		if err != nil {
			return machineFetched{}, err
		}
		ev := fleet.AuditEvent{
			At: now(), Actor: fleet.ActorCLI, Action: actionFetchRequested, MachineID: m.ID,
			Detail: talosFetchDetail{Schematic: key.Schematic, Version: key.Version, Arch: string(key.Arch)},
		}
		if err := s.Fleet.Audit(ctx, ev); err != nil {
			return machineFetched{}, err
		}
	}
	row, ok, err := s.Fleet.Talos(ctx, key)
	if err != nil {
		return machineFetched{}, err
	}
	out.Wanted, out.Reason = true, ""
	if ok {
		out.State, out.Attempts = string(row.State), row.Attempts
	}
	return out, nil
}

// text is f as "machine fetch" prints it.
func (f machineFetched) text() string {
	if !f.Wanted {
		return fmt.Sprintf("nothing to fetch for machine %s: %s\n", f.MachineID, f.Reason)
	}
	reset := ""
	if f.ResetHash {
		reset = "; recorded hashes cleared"
	}
	return fmt.Sprintf("Talos assets %s %s %s of machine %s are %s, %d attempts%s\n",
		f.Schematic, f.Version, f.Arch, f.MachineID, f.State, f.Attempts, reset)
}

// hostOf splits "<cluster>/<host>" at its only "/". Both parts must be given.
func hostOf(cmd *cobra.Command, arg string) (cluster, host string, err error) {
	cluster, host, ok := strings.Cut(arg, "/")
	if !ok || cluster == "" || host == "" || strings.Contains(host, "/") {
		return "", "", usageError{fmt.Errorf("%s: %q is not <cluster>/<host>", cmd.CommandPath(), arg)}
	}
	return cluster, host, nil
}

// bindRequest is a machine to bind to a host of a cluster with bind, a command of the clusters repository.
type bindRequest struct {
	verb          string
	machine, host string
	asJSON        bool
	bind          func(*clusters.Repository, context.Context, clusters.MachineRequest) (fleet.Machine, error)
}

// runBind resolves the machine and binds it to the host, which goes through the clusters repository: it holds the
// cluster row, so a bind cannot race a cluster's removal.
func runBind(cmd *cobra.Command, deps cliDeps, req bindRequest) error {
	cluster, host, err := hostOf(cmd, req.host)
	if err != nil {
		return err
	}
	return withCLI(cmd, deps, func(s cliStores) error {
		m, err := resolveMachine(cmd, s.Fleet, req.machine)
		if err != nil {
			return err
		}
		bound, err := req.bind(s.Clusters, cmd.Context(), clusters.MachineRequest{
			MachineID: m.ID, Cluster: cluster, Host: host, Actor: fleet.ActorCLI, At: deps.now(),
		})
		if err != nil {
			return bindErr(err, req.verb, m.ID, cluster, host)
		}
		return printChange(cmd, s, &bound, stateLabel(m.State), req.asJSON)
	})
}

// bindErr is err in the admin's words when it is a refusal an admin causes, and err otherwise.
func bindErr(err error, verb, subject, cluster, host string) error {
	if te, ok := errors.AsType[*fleet.TransitionError](err); ok {
		switch te.Reason {
		case "junk_uuid":
			return fmt.Errorf("cannot %s %s: its UUID is junk, so it names no one machine", verb, subject)
		case "host_not_found":
			return fmt.Errorf("cannot %s %s: host %s is not in the approved pair of cluster %s", verb, subject, host, cluster)
		case "host_bound":
			return fmt.Errorf("cannot %s %s: host %s/%s is already bound to a machine", verb, subject, cluster, host)
		}
		return fmt.Errorf("cannot %s %s: it is %s", verb, subject, stateLabel(te.From.State))
	}
	if errors.Is(err, fleet.ErrUUIDExists) {
		return fmt.Errorf("cannot %s %s: a machine with this UUID already has a record; name it by MAC or UUID instead", verb, subject)
	}
	return clusterErr(err, cluster)
}

// printChange prints m, a machine a command just changed from the state from.
func printChange(cmd *cobra.Command, s cliStores, m *fleet.Machine, from string, asJSON bool) error {
	names, err := clusterNames(cmd.Context(), s.Clusters)
	if err != nil {
		return err
	}
	row := machineRowOf(m, names)
	if asJSON {
		return writeJSON(cmd, machineChange{machineRow: row.printable(), FromState: from})
	}
	where := ""
	if host := row.clusterHost(); host != "" {
		where = " (" + host + ")"
	}
	return writeText(cmd, fmt.Sprintf("machine %s: %s -> %s%s\n", row.ID, from, row.State, safeText(where)))
}

func newMachineApproveCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "approve <mac|uuid> <cluster>/<host>",
		Short: "Approve a discovered machine into a host to install it",
		Long: `Bind a discovered machine to a host of the cluster's approved pair, assigned: its next boot installs
Talos there. The host must be in the approved pair and unbound, and the machine's UUID must not be junk.
The machine's Talos assets are wanted at once.`,
		Example: `  hephaestus machine approve 52:54:00:12:34:56 lab/cp-01
  hephaestus machine approve 4c4c4544-0042-3510-8052-b4c04f4e4d32 lab/wrk-01 --json`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBind(cmd, deps, bindRequest{
				verb: "approve", machine: args[0], host: args[1], asJSON: asJSON, bind: (*clusters.Repository).ApproveMachine,
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newMachineRebindCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "rebind <mac|uuid> <cluster>/<host>",
		Short: "Bind an orphaned machine to another host, or its own former one",
		Long: `Bind a machine whose host left the cluster definition to an unbound host of a cluster's approved
pair. It lands installed when it was installed or install-unconfirmed before it was orphaned, and
assigned otherwise. Its own former host is a valid target when the new definition has it again.
A machine with a junk UUID is never bound again.`,
		Example: `  hephaestus machine rebind 52:54:00:12:34:56 lab/cp-02`,
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBind(cmd, deps, bindRequest{
				verb: "rebind", machine: args[0], host: args[1], asJSON: asJSON, bind: (*clusters.Repository).RebindMachine,
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newMachineAdoptCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON     bool
		uuid, arch string
	)
	cmd := &cobra.Command{
		Use:   "adopt <mac|uuid> <cluster>/<host> | adopt --uuid <uuid> --arch <amd64|arm64> <cluster>/<host>",
		Short: "Adopt a machine that already runs Talos into a host",
		Long: `Bind a discovered machine to a host of the cluster's approved pair as installed, its stored render
the host's approved one: nothing is installed.

With --uuid and --arch, create the machine from the UUID typed, as installed, when it has never
booted through hephaestus. The architecture is stated because no boot has reported it. Its MACs, serial
and product are filled in by its first boot. A UUID that is junk or that already has a record is refused.`,
		Example: `  hephaestus machine adopt 52:54:00:12:34:56 lab/cp-01
  hephaestus machine adopt --uuid 4c4c4544-0042-3510-8052-b4c04f4e4d32 --arch amd64 lab/cp-01 --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("uuid") && !cmd.Flags().Changed("arch") {
				return exactArgs(2)(cmd, args)
			}
			if err := requireFlags(cmd, "uuid", "arch"); err != nil {
				return err
			}
			if len(args) != 1 {
				return usageError{fmt.Errorf("%s: with --uuid and --arch, give only <cluster>/<host>; got %d argument(s)", cmd.CommandPath(), len(args))}
			}
			if fleet.NormalizeUUID(uuid) == "" {
				return usageError{fmt.Errorf("%s: --uuid is not a UUID", cmd.CommandPath())}
			}
			_, err := archOf(cmd, arch)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("uuid") {
				return runAdoptByUUID(cmd, deps, adoptByUUID{uuid: uuid, arch: arch, host: args[0], asJSON: asJSON})
			}
			return runBind(cmd, deps, bindRequest{
				verb: "adopt", machine: args[0], host: args[1], asJSON: asJSON, bind: (*clusters.Repository).AdoptMachine,
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().StringVar(&uuid, "uuid", "", "create the machine with this UUID instead of naming one that booted")
	cmd.Flags().StringVar(&arch, "arch", "", "architecture of the machine created with --uuid: amd64 or arm64")
	return cmd
}

// archOf is the architecture named arch, or a usageError for any other value.
func archOf(cmd *cobra.Command, arch string) (fleet.Arch, error) {
	switch a := fleet.Arch(arch); a {
	case fleet.ArchAMD64, fleet.ArchARM64:
		return a, nil
	}
	return "", usageError{fmt.Errorf("%s: --arch must be %s or %s", cmd.CommandPath(), fleet.ArchAMD64, fleet.ArchARM64)}
}

// adoptByUUID is a machine to create from a typed UUID, bound as installed to host, "<cluster>/<host>".
type adoptByUUID struct {
	uuid, arch, host string
	asJSON           bool
}

// runAdoptByUUID creates the machine and binds it to the host.
func runAdoptByUUID(cmd *cobra.Command, deps cliDeps, req adoptByUUID) error {
	cluster, host, err := hostOf(cmd, req.host)
	if err != nil {
		return err
	}
	arch, err := archOf(cmd, req.arch)
	if err != nil {
		return err
	}
	uuid := fleet.NormalizeUUID(req.uuid)
	return withCLI(cmd, deps, func(s cliStores) error {
		m, err := s.Clusters.AdoptByUUID(cmd.Context(), clusters.AdoptByUUIDRequest{
			UUID: uuid, Arch: arch, Cluster: cluster, Host: host, Actor: fleet.ActorCLI, At: deps.now(),
		})
		if err != nil {
			return bindErr(err, "adopt", uuid, cluster, host)
		}
		return printChange(cmd, s, &m, stateLabel(fleet.StateNone), req.asJSON)
	})
}

// transitionSpec is a lifecycle command that applies one trigger to one machine.
type transitionSpec struct {
	name, short, long, example string
	trigger                    fleet.Trigger
	// needsReset is true for the command that refuses to run without --topf-reset-full-done.
	needsReset bool
}

// transitionSpecs are the commands that need only the machine: one per admin trigger that binds no host. They go
// through fleet.Repository.Transition, which a bind never does.
func transitionSpecs() []transitionSpec {
	return []transitionSpec{
		{
			name: "mark-installed", trigger: fleet.TriggerMarkInstalled,
			short:   "Confirm that a machine installed",
			long:    "Mark an installing or install-unconfirmed machine installed. Its next boot exits to its disk.",
			example: "  hephaestus machine mark-installed 52:54:00:12:34:56",
		},
		{
			name: "abort", trigger: fleet.TriggerAbortInstall,
			short: "Give up on an install in progress",
			long: `Return an installing machine to assigned, so its next boot mints a new ticket and serves its config
again. It works only from installing: once a network boot after the config was served has made the machine
install-unconfirmed, it is refused and the runbook is to reset the node fully and use "machine reinstall".

Warning: if the installer already wrote the META partition, aborting needs a full reset ("topf reset", which
wipes the whole disk by default) before the next install, or Talos halts at its next boot.`,
			example: "  hephaestus machine abort 52:54:00:12:34:56",
		},
		{
			name: "reinstall", trigger: fleet.TriggerReinstall, needsReset: true,
			short: "Send an installed machine back to assigned after a full reset",
			long: `Return an installed or install-unconfirmed machine to assigned, so its next boot installs Talos
again. The install always sets talos.halt_if_installed=1, and Talos halts when the META partition
exists, so the machine must have been wiped first. The order:

  1. topf reset            wipes the whole disk (--full defaults to true) and reboots the node; while the
                           machine is still installed hephaestus answers its boot with exit, and it finds no OS
  2. machine reinstall     with --topf-reset-full-done, to say you did
  3. power-cycle the node, or wait for its next network boot

"topf reset --full=false" keeps META and leads to a halt at every boot. Running this command before the
wipe is accepted but wrong: any reboot before the wipe would halt. Without --topf-reset-full-done it is
refused.`,
			example: "  hephaestus machine reinstall 52:54:00:12:34:56 --topf-reset-full-done",
		},
		{
			name: "unassign", trigger: fleet.TriggerUnassign,
			short: "Return an assigned machine to discovered", long: "Release the host of an assigned machine and return it to discovered.",
			example: "  hephaestus machine unassign 52:54:00:12:34:56",
		},
		{
			name: "unbind", trigger: fleet.TriggerUnbind,
			short: "Release an orphaned machine", long: "Return an orphaned machine, whose host left the cluster definition, to discovered.",
			example: "  hephaestus machine unbind 52:54:00:12:34:56",
		},
		{
			name: "cordon", trigger: fleet.TriggerCordon,
			short: "Cordon a machine", long: "Set a machine aside, remembering its state, until it is uncordoned.",
			example: "  hephaestus machine cordon 52:54:00:12:34:56",
		},
		{
			name: "uncordon", trigger: fleet.TriggerUncordon,
			short: "Lift a machine's cordon", long: "Return a cordoned machine to the state it had.",
			example: "  hephaestus machine uncordon 52:54:00:12:34:56",
		},
		{
			name: "local-only", trigger: fleet.TriggerMarkLocalOnly,
			short: "Make a machine always boot its local disk", long: "Mark a discovered machine local-only: it always boots its local disk.",
			example: "  hephaestus machine local-only 52:54:00:12:34:56",
		},
		{
			name: "unmark-local-only", trigger: fleet.TriggerUnmarkLocalOnly,
			short: "Return a local-only machine to discovered", long: "Return a local-only machine to discovered.",
			example: "  hephaestus machine unmark-local-only 52:54:00:12:34:56",
		},
		{
			name: "reject", trigger: fleet.TriggerReject,
			short: "Turn a discovered machine away", long: "Mark a discovered machine rejected.",
			example: "  hephaestus machine reject 52:54:00:12:34:56",
		},
		{
			name: "unreject", trigger: fleet.TriggerUnreject,
			short: "Return a rejected machine to discovered", long: "Return a rejected machine to discovered.",
			example: "  hephaestus machine unreject 52:54:00:12:34:56",
		},
		{
			name: "forget", trigger: fleet.TriggerForget,
			short:   "Remove a machine's record",
			long:    "Delete a rejected, discovered or local-only machine with its MACs and boot events. It is recorded again if it boots.",
			example: "  hephaestus machine forget 52:54:00:12:34:56",
		},
	}
}

// newTransitionCmd builds the command of spec.
func newTransitionCmd(deps cliDeps, spec transitionSpec) *cobra.Command {
	var asJSON, resetDone bool
	cmd := &cobra.Command{
		Use:     spec.name + " <mac|uuid>",
		Short:   spec.short,
		Long:    spec.long,
		Example: spec.example,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactArgs(1)(cmd, args); err != nil {
				return err
			}
			if spec.needsReset && !resetDone {
				return usageError{fmt.Errorf("%s: refused without --topf-reset-full-done: run \"topf reset\" first (its --full defaults to true), "+
					"then this command, then power-cycle the machine", cmd.CommandPath())}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				m, err := resolveMachine(cmd, s.Fleet, args[0])
				if err != nil {
					return err
				}
				changed, err := s.Fleet.Transition(cmd.Context(), fleet.TransitionRequest{
					MachineID: m.ID, Trigger: spec.trigger, Guard: fleet.Guard{ResetConfirmed: resetDone}, Actor: fleet.ActorCLI, At: deps.now(),
				})
				if err != nil {
					return transitionErr(spec.name, &m, err)
				}
				return printChange(cmd, s, &changed, stateLabel(m.State), asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	if spec.needsReset {
		cmd.Flags().BoolVar(&resetDone, "topf-reset-full-done", false, "confirm that the node was wiped with \"topf reset\" (--full=true, the default)")
	}
	return cmd
}

// transitionErr is err in the admin's words when the machine's state refuses the command, and err otherwise.
func transitionErr(name string, m *fleet.Machine, err error) error {
	if !errors.Is(err, fleet.ErrInvalidTransition) {
		return err
	}
	refusal := fmt.Errorf("cannot %s machine %s: it is %s", name, m.ID, stateLabel(m.State))
	if name == "abort" && m.State == fleet.StateInstallUnconfirmed {
		return fmt.Errorf("%w; abort works only from installing, and the runbook for this one is %s", refusal, runbookUnconfirmed)
	}
	return refusal
}
