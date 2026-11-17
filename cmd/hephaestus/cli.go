package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	// cliActor is the actor the admin commands audit their changes as.
	cliActor = "cli"

	// stateNotRequested is the utility state of an entry nothing has asked for:
	// it has no cache row.
	stateNotRequested = "not_requested"
	// stateStalePin is the utility state of an entry whose cache row is for an
	// older pin than the catalog's; serve repins it at its next start.
	stateStalePin = "stale_pin"
)

// cliDeps are what the admin commands read from the host, replaced in tests.
type cliDeps struct {
	addrsOf interfaceAddrs
	now     func() time.Time
	catalog func() (*catalog.Catalog, error)
}

// defaultCLIDeps is the host's interfaces, its clock and the compiled-in catalog.
func defaultCLIDeps() cliDeps {
	return cliDeps{addrsOf: hostInterfaceAddrs, now: time.Now, catalog: catalog.Default}
}

// openCLI loads the same configuration as serve and opens its database, which
// it refuses while a migration is pending. It returns the fleet repository, the
// catalog and the function that closes the database. It never opens the
// content store and never migrates: serve owns both.
func openCLI(cmd *cobra.Command, deps cliDeps) (*fleet.Repository, *catalog.Catalog, func() error, error) {
	cfg, err := loadConfig(cmd.Flags(), deps.addrsOf)
	if err != nil {
		return nil, nil, nil, err
	}
	cat, err := deps.catalog()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load utility catalog: %w", err)
	}
	dbh, err := db.Open(cmd.Context(), cfg.DatabaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	pending, err := dbh.Pending(cmd.Context())
	if err == nil && pending {
		err = errors.New(`database has pending migrations; start "hephaestus serve" once to apply them`)
	}
	if err != nil {
		return nil, nil, nil, errors.Join(err, dbh.Close())
	}
	return fleet.NewRepository(dbh), cat, dbh.Close, nil
}

// withCLI opens the CLI's database and catalog for run and closes the database
// after it.
func withCLI(cmd *cobra.Command, deps cliDeps, run func(*fleet.Repository, *catalog.Catalog) error) (err error) {
	repo, cat, closeDB, err := openCLI(cmd, deps)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeDB()) }()
	return run(repo, cat)
}

// exactArgs requires n positional arguments; any other count is a usageError
// (cobra.ExactArgs returns a plain error, which would exit 1).
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// helpGroup is the RunE of a command that only groups subcommands.
func helpGroup(cmd *cobra.Command, _ []string) error { return cmd.Help() }

// resolveMachine finds the machine ref names, with the admin's wording for the
// two refusals: nothing matches, or a UUID names several machines.
func resolveMachine(cmd *cobra.Command, repo *fleet.Repository, ref string) (fleet.Machine, error) {
	m, err := repo.ResolveMachine(cmd.Context(), ref)
	if err == nil {
		return m, nil
	}
	if amb, ok := errors.AsType[*fleet.AmbiguousError](err); ok {
		return fleet.Machine{}, amb
	}
	if errors.Is(err, fleet.ErrNotFound) {
		return fleet.Machine{}, fmt.Errorf("no machine matches %s; it must have booted through hephaestus once", ref)
	}
	return fleet.Machine{}, fmt.Errorf("resolve %s: %w", ref, err)
}

// entryFor returns the catalog entry for entry and arch, or the admin's
// refusal: no such utility, or none built for arch.
func entryFor(cat *catalog.Catalog, entry string, arch fleet.Arch) (catalog.Entry, error) {
	if e, ok := cat.Lookup(entry, string(arch)); ok {
		return e, nil
	}
	if !cat.Has(entry) {
		return catalog.Entry{}, unknownUtility(cat, entry)
	}
	return catalog.Entry{}, fmt.Errorf("%s has no %s build; it supports %s", entry, arch, strings.Join(cat.Arches(entry), ", "))
}

// unknownUtility is the refusal for an entry the catalog does not have.
func unknownUtility(cat *catalog.Catalog, entry string) error {
	entries := cat.Entries()
	names := make([]string, 0, len(entries))
	for i := range entries {
		names = append(names, entries[i].Entry)
	}
	return fmt.Errorf("unknown utility %s; known: %s", entry, strings.Join(slices.Compact(names), ", "))
}

// cacheRows returns the stored utility_cache rows by key.
func cacheRows(cmd *cobra.Command, repo *fleet.Repository) (map[fleet.CacheKey]fleet.CacheRow, error) {
	rows, err := repo.CacheRows(cmd.Context())
	if err != nil {
		return nil, err
	}
	byKey := make(map[fleet.CacheKey]fleet.CacheRow, len(rows))
	for i := range rows {
		byKey[rows[i].Key()] = rows[i]
	}
	return byKey, nil
}

// stateAtPin is row's state, or stateStalePin when the row is for another pin.
func stateAtPin(row fleet.CacheRow, pin string) string {
	if row.Pin != pin {
		return stateStalePin
	}
	return string(row.State)
}

// formatTime is t in RFC 3339 and UTC, or "" for the zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeJSON prints v as indented JSON.
func writeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeText prints s.
func writeText(cmd *cobra.Command, s string) error {
	_, err := io.WriteString(cmd.OutOrStdout(), s)
	return err
}

// writeTable prints a header and rows as aligned columns.
func writeTable(cmd *cobra.Command, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	for _, line := range slices.Concat([][]string{header}, rows) {
		if _, err := fmt.Fprintln(tw, strings.Join(line, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
