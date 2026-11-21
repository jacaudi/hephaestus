package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// diffContext is how many unchanged lines a unified diff shows around a change.
const diffContext = 3

// errorHidden is what the routine listing prints for a stored error text: the text can quote a fragment of a secret
// value, so it is shown only on request.
const errorHidden = "(hidden: pass --errors)"

// clusterRow is a cluster as "cluster list", "cluster import" and "cluster set" print it. Each error text is the
// cluster's stored one, opened, or errorHidden when there is one and the caller did not ask for it.
type clusterRow struct {
	Name              string `json:"name"`
	Repo              string `json:"repo"`
	Ref               string `json:"ref"`
	Path              string `json:"path"`
	Revision          int64  `json:"revision"`
	ApprovedPairID    string `json:"approved_pair_id"`
	PendingPairID     string `json:"pending_pair_id"`
	FailedTree        string `json:"failed_tree"`
	LastFetchAt       string `json:"last_fetch_at"`
	RefreshIncomplete bool   `json:"refresh_incomplete"`
	FetchError        string `json:"fetch_error"`
	FailedError       string `json:"failed_error"`
	LastCallError     string `json:"last_call_error"`
}

// printable is r with its stored text cleaned for JSON, which prints the runes encoding/json leaves raw; the text
// output escapes them with safeText instead.
func (r clusterRow) printable() clusterRow {
	for _, s := range []*string{&r.Repo, &r.Ref, &r.Path, &r.FailedTree, &r.FetchError, &r.FailedError, &r.LastCallError} {
		*s = fleet.PrintableText(*s)
	}
	return r
}

// clusterRowOf is c with its error texts opened; unless showErrors, each one that exists is errorHidden.
func clusterRowOf(ctx context.Context, repo *clusters.Repository, c *clusters.Cluster, showErrors bool) (clusterRow, error) {
	errs, err := repo.Errors(ctx, c.ID)
	if err != nil {
		return clusterRow{}, err
	}
	if !showErrors {
		for _, s := range []*string{&errs.Fetch, &errs.Failed, &errs.LastCall} {
			if *s != "" {
				*s = errorHidden
			}
		}
	}
	return clusterRow{
		Name: c.Name, Repo: c.RepoURL, Ref: c.Ref, Path: c.Subpath, Revision: c.Revision,
		ApprovedPairID: c.ApprovedPairID, PendingPairID: c.PendingPairID, FailedTree: c.FailedTree,
		LastFetchAt: formatTime(c.LastFetchAt), RefreshIncomplete: c.RefreshIncomplete,
		FetchError: errs.Fetch, FailedError: errs.Failed, LastCallError: errs.LastCall,
	}, nil
}

// clusterApproved is what "cluster approve" reports for a pair.
type clusterApproved struct {
	Name     string `json:"name"`
	PairID   string `json:"pair_id"`
	Orphaned int    `json:"orphaned"`
	Rearmed  int    `json:"rearmed"`
	Revoked  int    `json:"revoked"`
}

// driftApproved is what "cluster approve --host" reports.
type driftApproved struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Hash string `json:"hash"`
}

// clusterRefreshed is what "cluster refresh" reports.
type clusterRefreshed struct {
	Name             string `json:"name"`
	RefreshRequested bool   `json:"refresh_requested"`
}

// clusterRemoved is what "cluster remove" reports.
type clusterRemoved struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// hostDiff is one host of "cluster diff": the unified diff of the two redacted renders, which is empty when they
// match, and the config hashes of the two sides.
type hostDiff struct {
	Host                string `json:"host"`
	Diff                string `json:"diff"`
	OldHash             string `json:"old_hash"`
	NewHash             string `json:"new_hash"`
	SecretValuesChanged bool   `json:"secret_values_changed"`
}

// clusterDiff is what "cluster diff" reports. PairID is the pending pair, which an approval names; a drift diff has
// none, and its NewHash is what the drift approval names.
type clusterDiff struct {
	Name   string `json:"name"`
	PairID string `json:"pair_id,omitempty"`
	// References are the 1Password references the pending pair resolved, and NewReferences those the approved pair did
	// not: which items fill the redacted values. Their names are not secret; their values are never shown.
	References    []string   `json:"references,omitempty"`
	NewReferences []string   `json:"new_references,omitempty"`
	Hosts         []hostDiff `json:"hosts"`
}

// newClusterCmd builds the cluster command group.
func newClusterCmd(deps cliDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Define clusters and approve what is rendered from them",
		Long: `Import, change, list and remove the cluster definitions serve renders machine configs from,
see what a new render changes, and approve it.

These commands run inside the container and write the database that "hephaestus serve" reads;
serve does every fetch and render. They never touch the content store or the git store.`,
		Example: `  docker exec hephaestus /hephaestus cluster list
  docker exec hephaestus /hephaestus cluster diff lab`,
		Args: noArgs,
		RunE: helpGroup,
	}
	cmd.AddCommand(
		newClusterImportCmd(deps), newClusterSetCmd(deps), newClusterRemoveCmd(deps), newClusterListCmd(deps),
		newClusterDiffCmd(deps), newClusterApproveCmd(deps), newClusterRefreshCmd(deps),
	)
	return cmd
}

// requireFlags is a usageError unless every named flag was given. Cobra's own required-flag error is a plain
// error, which would exit 1.
func requireFlags(cmd *cobra.Command, names ...string) error {
	var missing []string
	for _, name := range names {
		if !cmd.Flags().Changed(name) {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return usageError{fmt.Errorf("%s: required flag(s) %s not set", cmd.CommandPath(), strings.Join(missing, ", "))}
	}
	return nil
}

// clusterErr is err in the admin's words when it is a refusal an admin causes, and err otherwise.
func clusterErr(err error, name string) error {
	switch {
	case errors.Is(err, clusters.ErrNotFound):
		return fmt.Errorf("no cluster named %s", name)
	case errors.Is(err, clusters.ErrExists):
		return fmt.Errorf("a cluster named %s exists", name)
	case errors.Is(err, clusters.ErrClusterInUse):
		return fmt.Errorf("cluster %s still has machines bound to it; unbind or forget them first", name)
	}
	return err
}

// requireCluster is an error unless a cluster is named name.
func requireCluster(ctx context.Context, repo *clusters.Repository, name string) error {
	if _, err := repo.Get(ctx, name); err != nil {
		return clusterErr(err, name)
	}
	return nil
}

func newClusterImportCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON                                     bool
		repoURL, ref, path, username, credentialID string
	)
	cmd := &cobra.Command{
		Use:   "import <name>",
		Short: "Import a cluster definition from a git repository",
		Long: `Store a cluster: its name, the https URL of its repository, the ref to follow and the path of its
Topf directory in the repository. Serve fetches and renders it. A URL that is not https, or that
carries a user or password, is refused. A private repository takes a --username and a --credential,
one ref+op:// reference to its password or token, which serve resolves.`,
		Example: `  hephaestus cluster import lab --repo https://git.example.com/lab/cluster.git --ref main --path clusters/lab
  hephaestus cluster import lab --repo https://git.example.com/lab/cluster.git --ref main --path . --username deploy --credential ref+op://vault/git/token --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactArgs(1)(cmd, args); err != nil {
				return err
			}
			return requireFlags(cmd, "repo", "ref", "path")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				c, err := s.Clusters.Import(cmd.Context(), clusters.ImportRequest{
					Name: name, RepoURL: repoURL, Ref: ref, Subpath: path, Username: username, CredentialRef: credentialID, At: deps.now(),
				})
				if err != nil {
					return clusterErr(err, name)
				}
				return printCluster(cmd, s.Clusters, &c, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().StringVar(&repoURL, "repo", "", "https URL of the git repository, without a user or password (required)")
	cmd.Flags().StringVar(&ref, "ref", "", "branch to follow (required)")
	cmd.Flags().StringVar(&path, "path", "", "path of the cluster's directory in the repository (required)")
	cmd.Flags().StringVar(&username, "username", "", "username for a private repository")
	cmd.Flags().StringVar(&credentialID, "credential", "", "ref+op:// reference to the repository's password or token")
	return cmd
}

func newClusterSetCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON                                     bool
		repoURL, ref, path, username, credentialID string
	)
	fields := []string{"repo", "ref", "path", "username", "credential"}
	cmd := &cobra.Command{
		Use:   "set <name> [--repo <url>] [--ref <ref>] [--path <path>] [--username <name>] [--credential <ref>]",
		Short: "Change a cluster's repository, ref, path or credentials",
		Long: `Change the fields given and leave the rest. A changed repo, ref or path discards the pending
pair and fetches the new source at once; the approved pair keeps serving until a new one is
approved. A changed credential is resolved again by serve. An empty value clears a username
or a credential.`,
		Example: `  hephaestus cluster set lab --ref release
  hephaestus cluster set lab --credential ref+op://vault/git/token --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactArgs(1)(cmd, args); err != nil {
				return err
			}
			if !slices.ContainsFunc(fields, cmd.Flags().Changed) {
				return usageError{fmt.Errorf("%s: give at least one of --%s", cmd.CommandPath(), strings.Join(fields, ", --"))}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				c, err := s.Clusters.Set(cmd.Context(), clusters.SetRequest{
					Name: name, RepoURL: given(cmd, "repo", repoURL), Ref: given(cmd, "ref", ref), Subpath: given(cmd, "path", path),
					Username: given(cmd, "username", username), CredentialRef: given(cmd, "credential", credentialID), At: deps.now(),
				})
				if err != nil {
					return clusterErr(err, name)
				}
				return printCluster(cmd, s.Clusters, &c, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().StringVar(&repoURL, "repo", "", "https URL of the git repository, without a user or password")
	cmd.Flags().StringVar(&ref, "ref", "", "branch to follow")
	cmd.Flags().StringVar(&path, "path", "", "path of the cluster's directory in the repository")
	cmd.Flags().StringVar(&username, "username", "", "username for a private repository")
	cmd.Flags().StringVar(&credentialID, "credential", "", "ref+op:// reference to the repository's password or token")
	return cmd
}

// given is a pointer to value when the flag was set on the command line, and nil otherwise.
func given(cmd *cobra.Command, flag, value string) *string {
	if !cmd.Flags().Changed(flag) {
		return nil
	}
	return new(value)
}

func newClusterRemoveCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a cluster and its pairs",
		Long: `Delete the cluster with its pending and approved pairs and their stored renders. It is refused while
any machine is bound to the cluster.`,
		Example: `  hephaestus cluster remove lab
  hephaestus cluster remove lab --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				if err := s.Clusters.Remove(cmd.Context(), name, fleet.ActorCLI, deps.now()); err != nil {
					return clusterErr(err, name)
				}
				if asJSON {
					return writeJSON(cmd, clusterRemoved{Name: name, Removed: true})
				}
				return writeText(cmd, "removed cluster "+name+"\n")
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newClusterListCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON     bool
		showErrors bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the clusters and their state",
		Long: `List every cluster: its repository, ref and path, its revision (it counts each change of the
source), the approved and pending pair ids, the tree that failed to render, when serve last fetched,
whether the last refresh could not finish, and whether serve stored a fetch, render or whole-call error.

The error texts are not printed: they come from git, the 1Password SDK and the render, and can quote a
fragment of a secret value. Pass --errors to print them, and keep that output out of tickets and logs.`,
		Example: `  hephaestus cluster list
  hephaestus cluster list --json
  hephaestus cluster list --errors`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				list, err := s.Clusters.List(cmd.Context())
				if err != nil {
					return err
				}
				rows := make([]clusterRow, 0, len(list)) // JSON must print [], not null
				for i := range list {
					row, err := clusterRowOf(cmd.Context(), s.Clusters, &list[i], showErrors)
					if err != nil {
						return err
					}
					rows = append(rows, row)
				}
				if asJSON {
					for i := range rows {
						rows[i] = rows[i].printable()
					}
					return writeJSON(cmd, rows)
				}
				return writeClusterRows(cmd, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the clusters as JSON")
	cmd.Flags().BoolVar(&showErrors, "errors", false, "print the stored error texts; they can quote fragments of secret values")
	return cmd
}

// printCluster prints c as one object in JSON, or as a table, its error texts hidden.
func printCluster(cmd *cobra.Command, repo *clusters.Repository, c *clusters.Cluster, asJSON bool) error {
	row, err := clusterRowOf(cmd.Context(), repo, c, false)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd, row.printable())
	}
	return writeClusterRows(cmd, []clusterRow{row})
}

// writeClusterRows prints rows as a table, or a note when there are none.
func writeClusterRows(cmd *cobra.Command, rows []clusterRow) error {
	if len(rows) == 0 {
		return writeText(cmd, "no clusters\n")
	}
	table := make([][]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		table = append(table, []string{
			r.Name, safeText(r.Repo), safeText(r.Ref), safeText(r.Path), strconv.FormatInt(r.Revision, 10),
			dash(r.ApprovedPairID), dash(r.PendingPairID), dash(r.FailedTree), dash(r.LastFetchAt), strconv.FormatBool(r.RefreshIncomplete),
			dash(r.FetchError), dash(r.FailedError), dash(r.LastCallError),
		})
	}
	header := []string{
		"NAME", "REPO", "REF", "PATH", "REVISION", "APPROVED", "PENDING", "FAILED_TREE", "LAST_FETCH", "REFRESH_INCOMPLETE",
		"FETCH_ERROR", "FAILED_ERROR", "LAST_CALL_ERROR",
	}
	return writeTable(cmd, header, table)
}

// dash is s made safe for the terminal, or "-" when it is empty.
func dash(s string) string { return cmp.Or(safeText(s), "-") }

func newClusterDiffCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON bool
		host   string
	)
	cmd := &cobra.Command{
		Use:   "diff <name> [--host <host>]",
		Short: "Show what the pending pair changes, or what a host's drift changed",
		Long: `Show, host by host, a unified diff of the redacted render of the pending pair against the approved
one, and the id that "cluster approve" takes. A host whose redacted render did not change but whose
real config did shows "secret values changed". It also lists the 1Password references the pending
pair resolved and those the approved pair did not, so you can see which items fill the redacted values.
With --host, show the drift of that host in the
approved pair: its stored render against the running config's, and the hash that "cluster approve
--host --hash" takes. Secret values are never shown.`,
		Example: `  hephaestus cluster diff lab
  hephaestus cluster diff lab --host cp-01 --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				if err := requireCluster(cmd.Context(), s.Clusters, name); err != nil {
					return err
				}
				out, err := clusterDiffOf(cmd.Context(), s.Clusters, name, host)
				if err != nil {
					return err
				}
				if asJSON {
					for i := range out.Hosts {
						out.Hosts[i].Diff = mapLines(out.Hosts[i].Diff, fleet.PrintableText)
					}
					return writeJSON(cmd, out)
				}
				return writeText(cmd, diffText(out, host != ""))
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the diff as JSON")
	cmd.Flags().StringVar(&host, "host", "", "show the drift of this host of the approved pair instead")
	return cmd
}

// clusterDiffOf compares the pending pair with the approved one, or, for a host, its stored render with its drift.
func clusterDiffOf(ctx context.Context, repo *clusters.Repository, name, host string) (clusterDiff, error) {
	if host != "" {
		d, err := repo.DriftDiff(ctx, name, host)
		if errors.Is(err, clusters.ErrNotFound) {
			return clusterDiff{}, fmt.Errorf("host %s of cluster %s has no drift to show", host, name)
		}
		if err != nil {
			return clusterDiff{}, err
		}
		h, err := hostDiffOf("stored/"+host, "drift/"+host, d)
		return clusterDiff{Name: name, Hosts: []hostDiff{h}}, err
	}
	pairID, hosts, err := repo.Diff(ctx, name)
	if errors.Is(err, clusters.ErrNotFound) {
		return clusterDiff{}, fmt.Errorf("cluster %s has no pending pair to diff", name)
	}
	if err != nil {
		return clusterDiff{}, err
	}
	out := clusterDiff{Name: name, PairID: pairID, Hosts: make([]hostDiff, 0, len(hosts))}
	if out.References, out.NewReferences, err = diffReferences(ctx, repo, name); err != nil {
		return clusterDiff{}, err
	}
	for i := range hosts {
		h, err := hostDiffOf("approved/"+hosts[i].Host, "pending/"+hosts[i].Host, hosts[i])
		if err != nil {
			return clusterDiff{}, err
		}
		out.Hosts = append(out.Hosts, h)
	}
	return out, nil
}

// diffReferences returns the references the pending pair of the cluster resolved, and those the approved pair did not.
func diffReferences(ctx context.Context, repo *clusters.Repository, name string) (all, added []string, err error) {
	c, err := repo.Get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if all, err = pairReferences(ctx, repo, c.PendingPairID); err != nil {
		return nil, nil, err
	}
	var approved []string
	if c.ApprovedPairID != "" {
		if approved, err = pairReferences(ctx, repo, c.ApprovedPairID); err != nil {
			return nil, nil, err
		}
	}
	for _, ref := range all {
		if !slices.Contains(approved, ref) {
			added = append(added, ref)
		}
	}
	return all, added, nil
}

// pairReferences is the sorted references of the pair's secret version; its values stay in the repository's call.
func pairReferences(ctx context.Context, repo *clusters.Repository, pairID string) ([]string, error) {
	p, err := repo.Pair(ctx, pairID)
	if err != nil {
		return nil, err
	}
	values, err := repo.Values(ctx, p.SecretVersionID)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(values)), nil
}

// hostDiffOf is d as a unified diff of its redacted renders, from fromFile to toFile.
func hostDiffOf(fromFile, toFile string, d clusters.HostDiff) (hostDiff, error) {
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: diffLines(d.Old), B: diffLines(d.New), FromFile: fromFile, ToFile: toFile, Context: diffContext,
	})
	if err != nil {
		return hostDiff{}, fmt.Errorf("diff %s: %w", d.Host, err)
	}
	return hostDiff{Host: d.Host, Diff: text, OldHash: d.OldHash, NewHash: d.NewHash, SecretValuesChanged: d.SecretValuesChanged}, nil
}

// diffLines is b as lines that each end in a newline, which difflib prints as they are. (difflib.SplitLines adds a
// line to text that already ends in one.)
func diffLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return slices.Collect(strings.Lines(strings.TrimSuffix(string(b), "\n") + "\n"))
}

// mapLines is s with f applied to each of its lines, so that f may remove or escape what a terminal or JSON would
// misread without touching the line breaks.
func mapLines(s string, f func(string) string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = f(lines[i])
	}
	return strings.Join(lines, "\n")
}

// diffText is the diff as "cluster diff" prints it: the approval to run, then each host with its diff.
func diffText(d clusterDiff, drift bool) string {
	var b strings.Builder
	if drift {
		h := d.Hosts[0]
		fmt.Fprintf(&b, "drift of %s/%s; approve it with: cluster approve %s --host %s --hash %s\n",
			d.Name, safeText(h.Host), d.Name, safeText(h.Host), safeText(h.NewHash))
	} else {
		fmt.Fprintf(&b, "pending pair %s of %s; approve it with: cluster approve %s %s\n", d.PairID, d.Name, d.Name, d.PairID)
		writeReferences(&b, "references", d.References)
		writeReferences(&b, "new references", d.NewReferences)
	}
	for i := range d.Hosts {
		h := &d.Hosts[i]
		fmt.Fprintf(&b, "\nhost %s\n", safeText(h.Host))
		switch {
		case h.Diff != "":
			b.WriteString(mapLines(h.Diff, safeText))
		case !h.SecretValuesChanged:
			b.WriteString("no change\n")
		}
		if h.SecretValuesChanged {
			b.WriteString("secret values changed\n")
		}
	}
	return b.String()
}

// writeReferences prints the titled list of references, or nothing when there are none.
func writeReferences(b *strings.Builder, title string, refs []string) {
	if len(refs) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", title)
	for _, ref := range refs {
		fmt.Fprintf(b, "  %s\n", safeText(ref))
	}
}

func newClusterApproveCmd(deps cliDeps) *cobra.Command {
	var (
		asJSON     bool
		host, hash string
	)
	cmd := &cobra.Command{
		Use:   "approve <name> <pair id> | approve <name> --host <host> --hash <hash>",
		Short: "Approve the pending pair, or a host's drift",
		Long: `Approve the pair "cluster diff" showed, by its id. Approving makes it the approved pair, deletes the
older one, revokes every live ticket of the cluster, and orphans the machines whose host the new
pair lacks. The id must still be the pending pair; a newer render replaces it and makes the id stale.

With --host and --hash, approve the drift of that host of the approved pair: the running config
becomes the stored one and the host is released. The hash is the one "cluster diff --host" printed
and must still be the drift's. The pair stays approved and no ticket is revoked.`,
		Example: `  hephaestus cluster approve lab 7QF3K2X5HDXBBA5YZLVC3AMMTK
  hephaestus cluster approve lab --host cp-01 --hash 9f86d081884c7d65 --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageError{fmt.Errorf("%s: want <name> and a pair id, or <name> with --host and --hash; got %d argument(s)", cmd.CommandPath(), len(args))}
			}
			byPair := len(args) == 2
			set := cmd.Flags().Changed("host") || cmd.Flags().Changed("hash")
			switch {
			case byPair && set:
				return usageError{fmt.Errorf("%s: a pair id and --host/--hash are alternatives", cmd.CommandPath())}
			case !byPair:
				return requireFlags(cmd, "host", "hash")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLI(cmd, deps, func(s cliStores) error {
				if len(args) == 2 {
					req := clusters.ApproveClusterRequest{Cluster: args[0], PairID: args[1], Actor: fleet.ActorCLI, At: deps.now()}
					return approvePair(cmd, s.Clusters, req, asJSON)
				}
				req := clusters.DriftApproveRequest{Cluster: args[0], Host: host, Hash: hash, Actor: fleet.ActorCLI, At: deps.now()}
				return approveDrift(cmd, s.Clusters, req, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().StringVar(&host, "host", "", "approve the drift of this host of the approved pair")
	cmd.Flags().StringVar(&hash, "hash", "", "config hash of the drift to approve, from cluster diff --host")
	return cmd
}

// approvePair makes the pair of req the approved pair of its cluster.
func approvePair(cmd *cobra.Command, repo *clusters.Repository, req clusters.ApproveClusterRequest, asJSON bool) error {
	name, pairID := req.Cluster, req.PairID
	res, err := repo.ApproveCluster(cmd.Context(), req)
	if errors.Is(err, clusters.ErrStale) {
		return usageError{fmt.Errorf("pair %s of %s is no longer pending; run \"cluster diff %s\" to see the current one", pairID, name, name)}
	}
	if err != nil {
		return clusterErr(err, name)
	}
	out := clusterApproved{Name: name, PairID: res.Pair.ID, Orphaned: res.Orphaned, Rearmed: res.Rearmed, Revoked: res.Revoked}
	if asJSON {
		return writeJSON(cmd, out)
	}
	return writeText(cmd, fmt.Sprintf("approved pair %s of %s: %d machines orphaned, %d re-armed for new Talos assets, %d tickets revoked\n",
		out.PairID, name, out.Orphaned, out.Rearmed, out.Revoked))
}

// approveDrift makes the drift of req's host, with req's config hash, the stored render of the approved pair.
func approveDrift(cmd *cobra.Command, repo *clusters.Repository, req clusters.DriftApproveRequest, asJSON bool) error {
	name, host, hash := req.Cluster, req.Host, req.Hash
	err := repo.ApproveDrift(cmd.Context(), req)
	switch {
	case errors.Is(err, clusters.ErrStale):
		return usageError{fmt.Errorf("the drift changed, or %s has none; run \"cluster diff %s --host %s\" to see it", host, name, host)}
	case errors.Is(err, clusters.ErrNotFound):
		if cerr := requireCluster(cmd.Context(), repo, name); cerr != nil {
			return cerr
		}
		return fmt.Errorf("host %s is not in the approved pair of %s", host, name)
	case err != nil:
		return err
	}
	if asJSON {
		return writeJSON(cmd, driftApproved{Name: name, Host: host, Hash: hash})
	}
	return writeText(cmd, fmt.Sprintf("approved the drift of %s/%s (config hash %s)\n", name, host, hash))
}

func newClusterRefreshCmd(deps cliDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "refresh <name>",
		Short: "Ask serve to fetch and render a cluster again",
		Long: `Flag the cluster for a refresh and clear its retry backoff, so serve fetches it and resolves its
secret references again at its next pick-up, within 10 seconds, and stores a new pending pair if
anything changed.`,
		Example: `  hephaestus cluster refresh lab
  hephaestus cluster refresh lab --json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withCLI(cmd, deps, func(s cliStores) error {
				if err := s.Clusters.RequestRefresh(cmd.Context(), name, fleet.ActorCLI, deps.now()); err != nil {
					return clusterErr(err, name)
				}
				if asJSON {
					return writeJSON(cmd, clusterRefreshed{Name: name, RefreshRequested: true})
				}
				return writeText(cmd, "refresh of "+name+" requested\n")
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}
