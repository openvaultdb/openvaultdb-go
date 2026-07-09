package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newDatabasesCmd() *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "databases",
		Short: "List databases mounted on a running OpenVaultDB server",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := fetchJSON(url + "/v1/databases")
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(body, "", "  ")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://"+DefaultAddr, "server base URL")
	cmd.AddCommand(newDatabasesCreateCmd())
	return cmd
}
