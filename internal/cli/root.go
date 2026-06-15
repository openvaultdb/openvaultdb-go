// Package cli builds the ovdb-server cobra command tree, wrapped with
// charm.land/fang/v2 to match the specscore-cli stack.
package cli

import (
	"context"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"
)

// Run executes the ovdb-server CLI with the given arguments.
func Run(args []string) error {
	root := newRootCommand()
	if len(args) > 1 {
		root.SetArgs(args[1:])
	}
	return fang.Execute(context.Background(), root, fang.WithoutVersion())
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "ovdb-server",
		Short:         "OpenVaultDB reference local server",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.AddCommand(serveCommand())
	return root
}
