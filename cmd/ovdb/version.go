package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ovdb version",
		Run: func(cmd *cobra.Command, args []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "ovdb", Version)
		},
	}
}
