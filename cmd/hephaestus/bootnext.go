package main

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// The statuses "boot-next list" gives a flag besides the cache row's state.
const (
	statusExpired      = "expired"
	statusUnknownEntry = "unknown_entry"
	statusUnsupported  = "unsupported"
)

// bootNextTTLHours is boot.BootNextTTL in hours, for the help text.
const bootNextTTLHours = int(boot.BootNextTTL / time.Hour)

// bootNextSet is what "boot-next set" reports.
type bootNextSet struct {
	MachineID     string   `json:"machine_id"`
	MACs          []string `json:"macs"`
	Entry         string   `json:"entry"`
	Arch          string   `json:"arch"`
	SetAt         string   `json:"set_at"`
	ExpiresAt     string   `json:"expires_at"`
	State         string   `json:"state"`
	PreviousError string   `json:"previous_error"`
	Warning       string   `json:"warning"`
	BoundMAC      string   `json:"bound_mac,omitempty"`
}

func (s bootNextSet) text() string {
	var b strings.Builder
	machine := fmt.Sprintf("machine %s (%s)", s.MachineID, strings.Join(s.MACs, ", "))
	if s.Warning != "" {
		fmt.Fprintf(&b, "WARNING: %s: %s\n", machine, s.Warning)
	}
	fmt.Fprintf(&b, "%s\nentry: %s\narch: %s\n", machine, s.Entry, s.Arch)
	if s.BoundMAC != "" {
		fmt.Fprintf(&b, "bound MAC: %s\n", safeText(s.BoundMAC))
	}
	fmt.Fprintf(&b, "expires: %s\nutility: %s\n", s.ExpiresAt, s.State)
	if s.PreviousError != "" {
		fmt.Fprintf(&b, "previous error: %s\n", safeText(s.PreviousError))
	}
	return b.String()
}

// bootNextClear is what "boot-next clear" reports.
type bootNextClear struct {
	MachineID string `json:"machine_id"`
	Entry     string `json:"entry"`
	Cleared   bool   `json:"cleared"`
}

// bootNextRow is one flag of "boot-next list".
type bootNextRow struct {
	MachineID  string   `json:"machine_id"`
	MACs       []string `json:"macs"`
	Entry      string   `json:"entry"`
	Arch       string   `json:"arch"`
	SetAt      string   `json:"set_at"`
	AgeSeconds int64    `json:"age_seconds"`
	Status     string   `json:"status"`
	BoundMAC   string   `json:"bound_mac,omitempty"`
}

// newBootNextCmd builds the boot-next command group.
func newBootNextCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "boot-next",
		Short: "Boot a machine into a utility OS once",
		Long: `Arm, clear and list the one-shot utility boots. A machine with a boot-next flag boots
that utility OS the next time it PXE-boots, then boots as usual.

These commands run inside the container and write the database that "hephaestus serve" reads;
shell access is the authorization. They never touch the content store.`,
		Example: `  docker exec hephaestus /hephaestus boot-next set 52:54:00:12:34:56 systemrescue
  docker exec hephaestus /hephaestus boot-next list`,
		Args: noArgs,
		RunE: helpGroup,
	}
	cmd.AddCommand(newBootNextSetCmd(deps), newBootNextClearCmd(deps), newBootNextListCmd(deps))
	return cmd
}

func newBootNextSetCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "set <mac|uuid> <entry>",
		Short: "Boot a machine into a utility OS on its next boot",
		Long: fmt.Sprintf(`Arm the machine's boot-next with the utility entry and ask for its content to be fetched.
The machine must have booted through hephaestus once. A UUID that several machines share is
refused; name one by MAC. A machine with a junk-pattern UUID (empty, all zeros or all Fs) must be
named by MAC: its flag is bound to that MAC and served only to a boot from it. The flag expires
%d hours after it was set.

The output shows when the flag expires and the utility's state: wanted, fetching, ready, failed,
broken or stale_pin. A failed or broken utility is re-armed, and the error it had is shown.`, bootNextTTLHours),
		Example: `  hephaestus boot-next set 52:54:00:12:34:56 systemrescue
  hephaestus boot-next set 4c4c4544-0042-3510-8052-b4c04f4e4d32 memtest86plus --json`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(repo *fleet.Repository, cat *catalog.Catalog) error {
				m, err := resolveMachine(cmd, repo, args[0])
				if err != nil {
					return err
				}
				bound, err := boundMAC(m, args[0])
				if err != nil {
					return err
				}
				e, err := entryFor(cat, args[1], m.Arch)
				if err != nil {
					return err
				}
				res, err := repo.SetBootNext(cmd.Context(), fleet.SetBootNextRequest{MachineID: m.ID, Entry: e.Entry, Pin: e.Pin.ID, BoundMAC: bound, At: deps.now()})
				if err != nil {
					return err
				}
				out := bootNextSet{
					MachineID: m.ID, MACs: m.MACs, Entry: e.Entry, Arch: string(m.Arch),
					SetAt:         formatTime(res.Machine.BootNextSetAt),
					ExpiresAt:     formatTime(res.Machine.BootNextSetAt.Add(boot.BootNextTTL)),
					State:         stateAtPin(res.Fetch.Row, e.Pin.ID),
					PreviousError: res.Fetch.PreviousError,
					Warning:       e.Warning,
					BoundMAC:      res.Machine.BootNextMAC,
				}
				if asJSON {
					return writeJSON(cmd, out)
				}
				return writeText(cmd, out.text())
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newBootNextClearCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "clear <mac|uuid>",
		Short: "Cancel a machine's boot-next",
		Long: `Clear the machine's boot-next flag. Nothing is cleared, and the exit status is 0, when the
machine has no flag or the flag changed while the command ran.`,
		Example: `  hephaestus boot-next clear 52:54:00:12:34:56
  hephaestus boot-next clear 52:54:00:12:34:56 --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(repo *fleet.Repository, _ *catalog.Catalog) error {
				m, err := resolveMachine(cmd, repo, args[0])
				if err != nil {
					return err
				}
				out := bootNextClear{MachineID: m.ID, Entry: m.BootNext}
				text := "no boot-next set for " + m.ID + "\n"
				if m.BootNext != "" {
					req := fleet.ClearBootNextRequest{
						MachineID: m.ID, Entry: m.BootNext, SetAt: m.BootNextSetAt,
						Reason: fleet.ClearCLI, Actor: cliActor, At: deps.now(),
					}
					if out.Cleared, err = repo.ClearBootNext(cmd.Context(), req); err != nil {
						return err
					}
					text = "boot-next changed meanwhile; nothing cleared\n"
					if out.Cleared {
						text = fmt.Sprintf("cleared boot-next %s of machine %s\n", m.BootNext, m.ID)
					}
				}
				if asJSON {
					return writeJSON(cmd, out)
				}
				return writeText(cmd, text)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newBootNextListCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the machines with a boot-next flag",
		Long: fmt.Sprintf(`List every machine with a boot-next flag: the entry, when it was set, its age in seconds and
a status, and the MAC a junk-pattern machine's flag is bound to. The status is expired after %d hours, unknown_entry when the catalog no longer has the
entry, unsupported when it has no build for the machine's architecture, and otherwise the
utility's state (not_requested, wanted, fetching, ready, failed or broken).`, bootNextTTLHours),
		Example: `  hephaestus boot-next list
  hephaestus boot-next list --json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLI(cmd, deps, func(repo *fleet.Repository, cat *catalog.Catalog) error {
				machines, err := repo.ListBootNext(cmd.Context())
				if err != nil {
					return err
				}
				cached, err := cacheRows(cmd, repo)
				if err != nil {
					return err
				}
				now := deps.now()
				rows := make([]bootNextRow, 0, len(machines)) // JSON must print [], not null
				for i := range machines {
					m := &machines[i]
					rows = append(rows, bootNextRow{
						MachineID: m.ID, MACs: m.MACs, Entry: m.BootNext, Arch: string(m.Arch),
						SetAt: formatTime(m.BootNextSetAt), AgeSeconds: int64(now.Sub(m.BootNextSetAt) / time.Second),
						Status: bootNextStatus(m, now, cat, cached), BoundMAC: m.BootNextMAC,
					})
				}
				if asJSON {
					return writeJSON(cmd, rows)
				}
				return writeBootNextRows(cmd, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the flags as JSON")
	return cmd
}

// bootNextStatus is m's flag as boot decision row 3 would find it, in its
// order: expired (the same comparison Decide makes), then an entry the catalog
// lost, then one with no build for the machine, then the utility's state.
func bootNextStatus(m *fleet.Machine, now time.Time, cat *catalog.Catalog, cached map[fleet.CacheKey]fleet.CacheRow) string {
	e, built := cat.Lookup(m.BootNext, string(m.Arch))
	switch {
	case now.Sub(m.BootNextSetAt) > boot.BootNextTTL:
		return statusExpired
	case !cat.Has(m.BootNext):
		return statusUnknownEntry
	case !built:
		return statusUnsupported
	}
	if row, ok := cached[fleet.CacheKey{Entry: m.BootNext, Arch: m.Arch}]; ok && row.Pin == e.Pin.ID {
		return string(row.State)
	}
	return stateNotRequested
}

// writeBootNextRows prints rows as a table, or a note when there are none.
func writeBootNextRows(cmd *cobra.Command, rows []bootNextRow) error {
	if len(rows) == 0 {
		return writeText(cmd, "no boot-next set on any machine\n")
	}
	table := make([][]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		table = append(table, []string{r.MachineID, strings.Join(r.MACs, ","), r.Entry, r.Arch, r.SetAt, strconv.FormatInt(r.AgeSeconds, 10), r.Status, cmp.Or(safeText(r.BoundMAC), "-")})
	}
	return writeTable(cmd, []string{"MACHINE", "MACS", "ENTRY", "ARCH", "SET_AT", "AGE_SECONDS", "STATUS", "BOUND_MAC"}, table)
}

// boundMAC is the MAC to bind m's flag to: for a junk-pattern machine, the MAC
// ref names, because a junk machine is known only by the MAC it boots from and
// its flag is served only to a boot from that MAC; "" for any other machine. A
// junk machine named any other way is refused.
func boundMAC(m fleet.Machine, ref string) (string, error) {
	if !m.Junk {
		return "", nil
	}
	mac, err := fleet.NormalizeMAC(ref)
	if err != nil {
		return "", fmt.Errorf("%s has a junk-pattern UUID; name it by the MAC it boots from", m.ID)
	}
	return mac, nil
}
