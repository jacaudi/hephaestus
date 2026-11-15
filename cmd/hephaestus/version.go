package main

import (
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// buildVersion returns the -ldflags version, or the module version from the build info.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// versionInfo is the JSON shape printed by `hephaestus version --json`.
type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// newVersionCmd builds the version subcommand.
func newVersionCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Long: `Print the version, commit and build date of this binary.

With --json the output is a JSON object with the keys version, commit and date.`,
		Example: `  hephaestus version
  hephaestus version --json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionInfo{Version: buildVersion(), Commit: commit, Date: date}
			if jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "hephaestus %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date)
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the version as JSON")
	return cmd
}
