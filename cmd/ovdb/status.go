package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func fetchJSON(url string) (map[string]any, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	var out map[string]any
	if err = json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func newStatusCmd() *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of a running OpenVaultDB server",
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := fetchJSON(url + "/v1/status")
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(status, "", "  ")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://"+DefaultAddr, "server base URL")
	return cmd
}
