// Command hephaestus boots bare metal into configured Talos nodes: proxyDHCP,
// TFTP and HTTP(S) boot serving, per-machine decisions and the admin surface.
//
// Exit codes: 0 success, 1 failure, 2 usage error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// Set at build time with -ldflags -X (go-standards §9.2).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError marks an error caused by how the command was invoked (unknown
// command, unknown flag, stray argument) so main exits 2 instead of 1.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func (e usageError) Unwrap() error { return e.err }

// loggedError marks an error that was already logged through slog, so main
// does not print it a second time.
type loggedError struct{ err error }

func (e loggedError) Error() string { return e.err.Error() }

func (e loggedError) Unwrap() error { return e.err }

// reportError prints err to w as plain text, unless it was already logged.
func reportError(w io.Writer, err error) {
	if _, ok := errors.AsType[loggedError](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return
	}
	if _, werr := fmt.Fprintf(w, "hephaestus: %v\n", err); werr != nil {
		return // stderr is gone; there is no one left to tell
	}
}

// exitCode maps the error returned by the root command to a process exit code.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	if _, ok := errors.AsType[usageError](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return exitUsage
	}
	return exitError
}

// noArgs rejects any positional argument with a usageError (cobra.NoArgs
// returns a plain error, which would exit 1).
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unexpected argument %q for %q", args[0], cmd.CommandPath())}
	}
	return nil
}

// newRootCmd builds the hephaestus root command with its subcommands.
func newRootCmd() *cobra.Command { return newRootCmdWith(defaultCLIDeps()) }

// newRootCmdWith is newRootCmd with the admin commands reading deps instead of
// the host, so tests can supply a fake interface table, clock and catalog.
func newRootCmdWith(deps cliDeps) *cobra.Command {
	root := &cobra.Command{
		Use:   "hephaestus",
		Short: "Boot bare metal into configured Talos nodes",
		Long: `hephaestus takes bare metal from power-on to a correctly configured Talos node.

Run "hephaestus version" to print the build version.`,
		Example: `  hephaestus version
  hephaestus version --json`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.PersistentFlags().String("log-format", "", "log format: json or text (overrides HEPHAESTUS_LOG_FORMAT)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError{err}
	})
	root.AddCommand(newVersionCmd(), newServeCmd(), newBootNextCmd(deps), newUtilityCmd(deps))
	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// After the first signal, restore default handling so a second one kills a
	// stuck drain (go-standards §13.1).
	context.AfterFunc(ctx, stop)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		reportError(os.Stderr, err)
	}
	os.Exit(exitCode(err))
}
