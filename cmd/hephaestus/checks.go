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
	// configPendingRunbook is what frees a machine whose config was recorded but never received, by its state.
	configPendingRunbook = "if its config was recorded but not received: installing: machine abort, then reboot; " +
		"install-unconfirmed: topf reset, machine reinstall --topf-reset-full-done, then reboot"
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

// configPendingCheck warns while a live ticket minted configPendingAfter or more ago has never been used to ask
// for its config, naming each one's machine, with the runbook of a config recorded but not received.
func configPendingCheck(repo *fleet.Repository, now func() time.Time) admin.Check {
	return admin.Check{Name: "config:pending", Run: func(ctx context.Context) admin.Result {
		tickets, err := repo.PendingTickets(ctx, now().Add(-configPendingAfter))
		if err != nil {
			return admin.Result{Status: admin.StatusFail, Output: err.Error()}
		}
		var waiting []string
		for i := range tickets {
			// PendingTickets also returns the tickets /config was asked for.
			if t := &tickets[i]; t.RequestedAt.IsZero() {
				waiting = append(waiting, fmt.Sprintf("machine %s has not asked for its config since %s",
					t.MachineID, t.MintedAt.Format(time.RFC3339)))
			}
		}
		if len(waiting) == 0 {
			return admin.Result{Status: admin.StatusPass}
		}
		return admin.Result{Status: admin.StatusWarn, Output: strings.Join(waiting, "; ") + "; " + configPendingRunbook}
	}}
}
