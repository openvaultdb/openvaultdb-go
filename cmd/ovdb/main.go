// Command ovdb is the OpenVaultDB CLI — the canonical developer/admin
// interface for managing, exploring, validating and operating OpenVaultDB
// instances. `ovdb serve` runs the local API server.
package main

import (
	"context"
	"os"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

	"github.com/strongo/buildinfo"
	"github.com/strongo/buildinfo/fangcmd"
)

// DefaultAddr is the default listen/connect address. Local-first: binds to
// loopback unless explicitly overridden ("6832" spells OVDB on a phone keypad).
const DefaultAddr = "127.0.0.1:6832"

// version is this build's resolved bare semver, used by subcommands (serve)
// that need to report it. Set once in main, before the command tree runs;
// see github.com/strongo/buildinfo for how it is resolved (link-time -X
// stamping, falling back to runtime/debug.ReadBuildInfo()).
var version string

func main() {
	info := buildinfo.Get("ovdb")
	version = info.Short()

	root := &cobra.Command{
		Use:   "ovdb",
		Short: "OpenVaultDB — user-owned, portable databases with pluggable engines",
	}
	root.AddCommand(
		newServeCmd(),
		newInitCmd(),
		newStatusCmd(),
		newDatabasesCmd(),
		newTokenCmd(),
	)

	// fangcmd.Wire adds the `version` subcommand (printing info.Long()) and
	// returns the fang.Option(s) that make --version/-v print info.Short() —
	// the same resolved Info, so the two surfaces can't disagree. fang.Execute
	// also sets SilenceUsage/SilenceErrors and prints a styled error itself,
	// so main does not need to do either.
	opts := fangcmd.Wire(root, info)
	if err := fang.Execute(context.Background(), root, opts...); err != nil {
		os.Exit(1)
	}
}
