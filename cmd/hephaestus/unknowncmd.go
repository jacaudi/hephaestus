package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// holdLookback is how recently a discovered machine must have been seen for "unknown-action set hold" to list it:
// every one of them network-boots, or it would never have reported.
const holdLookback = 7 * 24 * time.Hour

// hostRefRow is a host of an approved pair that no machine is bound to.
type hostRefRow struct {
	Cluster string `json:"cluster"`
	Host    string `json:"host"`
}

// unknownActionSet is what "unknown-action set" reports. Applied is false when the setting was not changed.
type unknownActionSet struct {
	Setting string `json:"setting"`
	Applied bool   `json:"applied"`
}

// unknownActionHold is what "unknown-action set hold" reports: what holding would affect, and whether it was applied.
type unknownActionHold struct {
	unknownActionSet

	UnboundHosts []hostRefRow `json:"unbound_hosts"`
	Discovered   []machineRow `json:"discovered"`
}

// newUnknownActionCmd builds the unknown-action command group.
func newUnknownActionCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unknown-action",
		Short: "Choose what a machine hephaestus has no record of does at boot",
		Long: `Choose what /boot does with a machine it has no record of, or that is still discovered: boot its
local disk (local) or wait in the hold loop for an approval (hold).

These commands run inside the container and write the database that "hephaestus serve" reads.`,
		Example: `  docker exec hephaestus /hephaestus unknown-action set local`,
		Args:    noArgs,
		RunE:    helpGroup,
	}
	cmd.AddCommand(newUnknownActionSetCmd(deps))
	return cmd
}

func newUnknownActionSetCmd(deps cliDeps) *cobra.Command {
	var asJSON, yes bool
	cmd := &cobra.Command{
		Use:   "set <hold|local>",
		Short: "Set what an unknown or discovered machine does at boot",
		Long: `Set the unknown_action setting.

local boots the local disk of a machine that has no record or is discovered. hold keeps it in the hold loop
instead, waiting for an approval: before you set it, check what it will hold. Setting hold lists the hosts of
the approved pairs that no machine is bound to and every discovered machine seen in the last 7 days (each
of them network-boots, or it would never have reported), changes nothing, and refuses to run until you
repeat it with --yes.`,
		Example: `  hephaestus unknown-action set hold
  hephaestus unknown-action set hold --yes --json
  hephaestus unknown-action set local`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var action fleet.UnknownAction
			if err := action.UnmarshalText([]byte(args[0])); err != nil {
				return usageError{fmt.Errorf("%s: %w", cmd.CommandPath(), err)}
			}
			return withCLI(cmd, deps, func(s cliStores) error {
				if action == fleet.UnknownActionHold && !yes {
					if err := printHold(cmd, s, deps.now(), false, asJSON); err != nil {
						return err
					}
					return usageError{fmt.Errorf("%s: setting hold changes what the machines listed do at boot; run it again with --yes to apply it", cmd.CommandPath())}
				}
				if err := s.Fleet.SetUnknownAction(cmd.Context(), action, fleet.ActorCLI, deps.now()); err != nil {
					return err
				}
				if action == fleet.UnknownActionHold {
					return printHold(cmd, s, deps.now(), true, asJSON)
				}
				if asJSON {
					return writeJSON(cmd, unknownActionSet{Setting: string(action), Applied: true})
				}
				return writeText(cmd, "unknown_action set to "+string(action)+"\n")
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply hold after seeing what it affects")
	return cmd
}

// printHold prints what setting hold affects as of now, and whether it was applied.
func printHold(cmd *cobra.Command, s cliStores, now time.Time, applied, asJSON bool) error {
	hosts, err := s.Clusters.UnboundHosts(cmd.Context())
	if err != nil {
		return err
	}
	machines, err := s.Fleet.ListMachines(cmd.Context())
	if err != nil {
		return err
	}
	out := unknownActionHold{
		unknownActionSet: unknownActionSet{Setting: string(fleet.UnknownActionHold), Applied: applied},
		UnboundHosts:     make([]hostRefRow, 0, len(hosts)), // JSON must print [], not null
		Discovered:       make([]machineRow, 0, len(machines)),
	}
	for _, h := range hosts {
		out.UnboundHosts = append(out.UnboundHosts, hostRefRow{Cluster: h.Cluster, Host: h.Host})
	}
	for i := range machines {
		if m := &machines[i]; m.State == fleet.StateDiscovered && !m.LastSeenAt.Before(now.Add(-holdLookback)) {
			out.Discovered = append(out.Discovered, machineRowOf(m, nil))
		}
	}
	if asJSON {
		for i := range out.Discovered {
			out.Discovered[i] = out.Discovered[i].printable()
		}
		return writeJSON(cmd, out)
	}
	return writeText(cmd, out.text())
}

// text is h as "unknown-action set hold" prints it.
func (h unknownActionHold) text() string {
	var b strings.Builder
	b.WriteString("with hold, a machine with no record and every discovered machine waits in the hold loop for an approval\n")
	b.WriteString("\nunbound hosts of the approved pairs:\n")
	for _, u := range h.UnboundHosts {
		fmt.Fprintf(&b, "  %s/%s\n", safeText(u.Cluster), safeText(u.Host))
	}
	if len(h.UnboundHosts) == 0 {
		b.WriteString("  none\n")
	}
	b.WriteString("\ndiscovered machines seen in the last 7 days:\n")
	for i := range h.Discovered {
		d := &h.Discovered[i]
		fmt.Fprintf(&b, "  %s  %s  %s  %s  last seen %s\n", d.ID, dash(strings.Join(d.MACs, ",")), dash(d.UUID), d.Arch, dash(d.LastSeenAt))
	}
	if len(h.Discovered) == 0 {
		b.WriteString("  none\n")
	}
	if h.Applied {
		b.WriteString("\nunknown_action set to hold\n")
	} else {
		b.WriteString("\nnothing changed\n")
	}
	return b.String()
}
