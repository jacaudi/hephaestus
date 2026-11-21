package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	// configPendingAfter is how long a ticket may go unfetched before config:pending warns: the likeliest
	// first-install failure (DNS, the node's clock, TLS trust) shows as a ticket nobody asks for.
	configPendingAfter = 30 * time.Minute
	// runbookUnconfirmed is the runbook for a config recorded but never received, once the machine is
	// install-unconfirmed.
	runbookUnconfirmed = "install-unconfirmed: topf reset, machine reinstall --topf-reset-full-done, then reboot"
	// remedyNotAsked is what a ticket nobody asked for points at.
	remedyNotAsked = "check the node's DNS, its clock and its trust of the boot certificate"
	// remedyAsked is what a ticket that was asked for and is still live points at: the request was refused, or the
	// delivery happened and was not recorded, which leaves the machine assigned.
	remedyAsked = "the request was refused (audit --machine <mac|uuid> shows a config.refused event with the reason) " +
		"or the config was delivered and not recorded (machine unassign, then machine adopt <mac|uuid> <cluster>/<host>)"
)

// talosCacheCheck warns while any talos_cache row is broken, naming each row's schematic, version and arch: a
// broken row stays so until an admin re-arms it.
func talosCacheCheck(repo *fleet.Repository) admin.Check {
	return admin.Check{Name: "talos:cache", Run: func(ctx context.Context) admin.Result {
		rows, err := repo.TalosRows(ctx)
		if err != nil {
			return admin.Result{Status: admin.StatusFail, Output: err.Error()}
		}
		var broken []string
		for i := range rows {
			if r := &rows[i]; r.State == fleet.CacheBroken {
				broken = append(broken, fmt.Sprintf("schematic %s version %s arch %s is broken", r.Schematic, r.Version, r.Arch))
			}
		}
		if len(broken) == 0 {
			return admin.Result{Status: admin.StatusPass}
		}
		return admin.Result{Status: admin.StatusWarn, Output: strings.Join(broken, "; ")}
	}}
}

// configPendingCheck warns while a ticket minted configPendingAfter or more ago is neither consumed nor revoked,
// naming each one's machine. A ticket nobody asked for points at the node; one that was asked for says when, and
// points at the refusal or the delivery that was not recorded.
func configPendingCheck(repo *fleet.Repository, now func() time.Time) admin.Check {
	return admin.Check{Name: "config:pending", Run: func(ctx context.Context) admin.Result {
		tickets, err := repo.PendingTickets(ctx, now().Add(-configPendingAfter))
		if err != nil {
			return admin.Result{Status: admin.StatusFail, Output: err.Error()}
		}
		waiting := make([]string, 0, len(tickets))
		for i := range tickets {
			t := &tickets[i]
			if t.RequestedAt.IsZero() {
				waiting = append(waiting, fmt.Sprintf("machine %s has not asked for its config since %s: %s",
					t.MachineID, t.MintedAt.Format(time.RFC3339), remedyNotAsked))
				continue
			}
			waiting = append(waiting, fmt.Sprintf("machine %s asked for its config at %s and its ticket is still live: %s",
				t.MachineID, t.RequestedAt.Format(time.RFC3339), remedyAsked))
		}
		if len(waiting) == 0 {
			return admin.Result{Status: admin.StatusPass}
		}
		return admin.Result{Status: admin.StatusWarn, Output: strings.Join(waiting, "; ")}
	}}
}
