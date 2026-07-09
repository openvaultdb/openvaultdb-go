// Command ovdb is the OpenVaultDB CLI — the canonical developer/admin
// interface for managing, exploring, validating and operating OpenVaultDB
// instances. `ovdb serve` runs the local API server.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is the ovdb CLI/server version (overridable via ldflags).
var Version = "0.1.0-dev"

// DefaultAddr is the default listen/connect address. Local-first: binds to
// loopback unless explicitly overridden ("6832" spells OVDB on a phone keypad).
const DefaultAddr = "127.0.0.1:6832"

func main() {
	root := &cobra.Command{
		Use:           "ovdb",
		Short:         "OpenVaultDB — user-owned, portable databases with pluggable engines",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newServeCmd(),
		newInitCmd(),
		newStatusCmd(),
		newDatabasesCmd(),
		newVersionCmd(),
	)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
