package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// auditRow is one event of "audit list". Detail is the event's JSON, as stored.
type auditRow struct {
	At        string `json:"at"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	MachineID string `json:"machine_id"`
	Detail    string `json:"detail"`
}

// newAuditCmd builds the audit command group.
func newAuditCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the audit log",
		Long: `Read the audit log: every change an admin command or hephaestus made, and every alert.

These commands run inside the container and only read the database that "hephaestus serve" writes.`,
		Example: `  docker exec hephaestus /hephaestus audit list --since 24h
  docker exec hephaestus /hephaestus audit list --machine 52:54:00:12:34:56`,
		Args: noArgs,
		RunE: helpGroup,
	}
	cmd.AddCommand(newAuditListCmd(deps))
	return cmd
}

func newAuditListCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON  bool
		since   time.Duration
		machine string
	)
	cmd := &cobra.Command{
		Use:   "list [--since <duration>] [--machine <mac|uuid>]",
		Short: "List audit events, oldest first",
		Long: `List the audit events, oldest first: when, who, what, the machine it names and its detail. With
--since, only the events of that long ago until now (a Go duration such as 90m or 72h); with
--machine, only the events that name that machine, which must still exist.`,
		Example: `  hephaestus audit list --since 24h
  hephaestus audit list --machine 52:54:00:12:34:56 --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := noArgs(cmd, args); err != nil {
				return err
			}
			if cmd.Flags().Changed("since") && since <= 0 {
				return usageError{fmt.Errorf("%s: --since must be a positive duration, got %s", cmd.CommandPath(), since)}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				var from time.Time // the zero time keeps every event
				if since > 0 {
					from = deps.now().Add(-since)
				}
				machineID := ""
				if machine != "" {
					m, err := resolveMachine(cmd, s.Fleet, machine)
					if err != nil {
						return err
					}
					machineID = m.ID
				}
				events, err := s.Fleet.AuditEvents(cmd.Context(), from, machineID)
				if err != nil {
					return err
				}
				rows := make([]auditRow, 0, len(events)) // JSON must print [], not null
				for i := range events {
					e := &events[i]
					rows = append(rows, auditRow{At: formatTime(e.At), Actor: e.Actor, Action: e.Action, MachineID: e.MachineID, Detail: e.Detail})
				}
				if asJSON {
					for i := range rows {
						rows[i].Detail = fleet.PrintableText(rows[i].Detail) // as in cluster list
					}
					return writeJSON(cmd, rows)
				}
				return writeAuditRows(cmd, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the events as JSON")
	cmd.Flags().DurationVar(&since, "since", 0, "only events from this long ago until now, for example 24h (default: every event)")
	cmd.Flags().StringVar(&machine, "machine", "", "only events that name this machine, by MAC or UUID")
	return cmd
}

// writeAuditRows prints rows as a table, or a note when there are none.
func writeAuditRows(cmd *cobra.Command, rows []auditRow) error {
	if len(rows) == 0 {
		return writeText(cmd, "no audit events\n")
	}
	table := make([][]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		table = append(table, []string{r.At, safeText(r.Actor), safeText(r.Action), dash(r.MachineID), safeText(r.Detail)})
	}
	return writeTable(cmd, []string{"AT", "ACTOR", "ACTION", "MACHINE", "DETAIL"}, table)
}
