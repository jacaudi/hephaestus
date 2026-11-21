package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// bytesPerMiB converts the catalog's minimum RAM to the MiB "utility list" prints.
const bytesPerMiB = 1 << 20

// utilityFetch is one row of "utility fetch".
type utilityFetch struct {
	Entry         string `json:"entry"`
	Arch          string `json:"arch"`
	Pin           string `json:"pin"`
	State         string `json:"state"`
	PreviousError string `json:"previous_error"`
}

// utilityRow is one row of "utility list".
type utilityRow struct {
	Entry       string   `json:"entry"`
	Arch        string   `json:"arch"`
	Version     string   `json:"version"`
	Platforms   []string `json:"platforms"`
	Pin         string   `json:"pin"`
	State       string   `json:"state"`
	BytesDone   int64    `json:"bytes_done"`
	BytesTotal  int64    `json:"bytes_total"`
	LastError   string   `json:"last_error"`
	UpdatedAt   string   `json:"updated_at"`
	TOFU        bool     `json:"tofu"`
	MinRAMBytes int64    `json:"min_ram_bytes"`
}

// newUtilityCmd builds the utility command group.
func newUtilityCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "utility",
		Short: "Fetch and inspect the utility OS catalog",
		Long: `Ask for utility operating systems to be fetched into the content store, and list what the
catalog holds and how far each fetch is.

These commands run inside the container and write the database that "hephaestus serve" reads;
serve does the fetching. They never touch the content store.`,
		Example: `  docker exec hephaestus /hephaestus utility fetch systemrescue
  docker exec hephaestus /hephaestus utility list`,
		Args: noArgs,
		RunE: helpGroup,
	}
	cmd.AddCommand(newUtilityFetchCmd(deps), newUtilityListCmd(deps))
	return cmd
}

func newUtilityFetchCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON bool
		arch   string
	)
	cmd := &cobra.Command{
		Use:   "fetch <entry>",
		Short: "Fetch a utility OS ahead of its first boot",
		Long: `Ask serve to fetch the entry for every architecture it is built for, or for the one --arch
names. A failed or broken fetch is re-armed, and the error it had is shown.`,
		Example: `  hephaestus utility fetch systemrescue
  hephaestus utility fetch grml --arch arm64 --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entry := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				if !s.Catalog.Has(entry) {
					return unknownUtility(s.Catalog, entry)
				}
				arches := s.Catalog.Arches(entry)
				if arch != "" {
					arches = []string{arch}
				}
				entries := make([]catalog.Entry, 0, len(arches))
				for _, a := range arches {
					e, err := entryFor(s.Catalog, entry, fleet.Arch(a))
					if err != nil {
						return err
					}
					entries = append(entries, e)
				}
				// Rows requested before a failure are still printed, so the admin knows what stuck.
				rows, reqErr := requestFetches(cmd.Context(), s.Fleet, entries, deps.now)
				if len(rows) == 0 {
					return reqErr
				}
				if asJSON {
					// A row stored before storage cleaned its error may still hold control
					// runes, which encoding/json prints raw; the text paths escape them instead.
					for i := range rows {
						rows[i].PreviousError = fleet.PrintableText(rows[i].PreviousError)
					}
					return errors.Join(writeJSON(cmd, rows), reqErr)
				}
				return errors.Join(writeText(cmd, fetchText(rows)), reqErr)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().StringVar(&arch, "arch", "", "architecture to fetch, amd64 or arm64 (default: every build of the entry)")
	return cmd
}

// requestFetches asks for each of entries, in order, and returns the rows it
// requested, up to the first failure.
func requestFetches(ctx context.Context, repo *fleet.Repository, entries []catalog.Entry, now func() time.Time) ([]utilityFetch, error) {
	var rows []utilityFetch
	for i := range entries {
		e := &entries[i]
		key := fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)}
		req, err := repo.RequestFetch(ctx, key, e.Pin.ID, 0, now())
		if err != nil {
			return rows, err
		}
		rows = append(rows, utilityFetch{
			Entry: e.Entry, Arch: e.Arch, Pin: e.Pin.ID,
			State: stateAtPin(req.Row, e.Pin.ID), PreviousError: req.PreviousError,
		})
	}
	return rows, nil
}

// fetchText is rows as "utility fetch" prints them.
func fetchText(rows []utilityFetch) string {
	var b strings.Builder
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(&b, "%s/%s: %s\n", r.Entry, r.Arch, r.State)
		if r.PreviousError != "" {
			fmt.Fprintf(&b, "  previous error: %s\n", safeText(r.PreviousError))
		}
	}
	return b.String()
}

func newUtilityListCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the utility OS catalog and its fetch state",
		Long: `List every catalog entry by architecture: version, platforms, fetch state, progress, the last
error, when the row last changed, whether the content is trust-on-first-use and the memory the
entry needs. The state is not_requested when nothing asked for it, and stale_pin when the stored
row is for an older pin than the catalog's.`,
		Example: `  hephaestus utility list
  hephaestus utility list --json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				cached, err := cacheRows(cmd, s.Fleet)
				if err != nil {
					return err
				}
				entries := s.Catalog.Entries()
				rows := make([]utilityRow, 0, len(entries)) // JSON must print [], not null
				for i := range entries {
					rows = append(rows, utilityRowOf(&entries[i], cached))
				}
				if asJSON {
					for i := range rows {
						rows[i].LastError = fleet.PrintableText(rows[i].LastError) // as in utility fetch
					}
					return writeJSON(cmd, rows)
				}
				return writeUtilityRows(cmd, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the catalog as JSON")
	return cmd
}

// utilityRowOf is e with the state of its cache row, if it has one.
func utilityRowOf(e *catalog.Entry, cached map[fleet.CacheKey]fleet.CacheRow) utilityRow {
	r := utilityRow{
		Entry: e.Entry, Arch: e.Arch, Version: e.Version, Pin: e.Pin.ID, State: stateNotRequested,
		TOFU: e.TOFU(), MinRAMBytes: e.MinRAMBytes(),
	}
	for _, p := range e.Platforms {
		r.Platforms = append(r.Platforms, string(p))
	}
	if row, ok := cached[fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)}]; ok {
		r.State = stateAtPin(row, e.Pin.ID)
		r.BytesDone, r.BytesTotal = row.BytesDone, row.BytesTotal
		r.LastError, r.UpdatedAt = row.LastError, formatTime(row.UpdatedAt)
	}
	return r
}

// writeUtilityRows prints rows as a table.
func writeUtilityRows(cmd *cobra.Command, rows []utilityRow) error {
	table := make([][]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		table = append(table, []string{
			r.Entry, r.Arch, r.Version, strings.Join(r.Platforms, ","), r.State,
			fmt.Sprintf("%d/%d", r.BytesDone, r.BytesTotal), safeText(r.LastError), r.UpdatedAt,
			strconv.FormatBool(r.TOFU), strconv.FormatInt((r.MinRAMBytes+bytesPerMiB-1)/bytesPerMiB, 10),
		})
	}
	header := []string{"ENTRY", "ARCH", "VERSION", "PLATFORMS", "STATE", "PROGRESS", "LAST_ERROR", "UPDATED", "TOFU", "MIN_RAM_MIB"}
	return writeTable(cmd, header, table)
}
