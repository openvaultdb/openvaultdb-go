package cli

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/openvaultdb/openvaultdb-go/internal/server"
	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

func serveCommand() *cobra.Command {
	var (
		port    int
		dataDir string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OpenVaultDB HTTP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(dataDir)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}

			baseURL := fmt.Sprintf("http://localhost:%d", port)
			srv := server.New(st, baseURL)

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "OWNER_TOKEN=%s\n", st.OwnerToken())
			fmt.Fprintf(out, "OpenVaultDB server listening on %s (data dir: %s)\n", baseURL, st.Dir())

			addr := fmt.Sprintf(":%d", port)
			return http.ListenAndServe(addr, srv.Handler())
		},
	}
	cmd.Flags().IntVar(&port, "port", 8088, "TCP port to listen on")
	cmd.Flags().StringVar(&dataDir, "data-dir", "./ovdb-data", "directory for on-disk vault data")
	return cmd
}
