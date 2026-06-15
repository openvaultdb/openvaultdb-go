package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/openvaultdb/openvaultdb-go/internal/server"
	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// defaultDataDir is ~/openvaultdb when a home directory is resolvable, else a
// local ./openvaultdb fallback.
func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "openvaultdb")
	}
	return "./openvaultdb"
}

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
	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir(), "directory for on-disk vault data (default ~/openvaultdb)")
	return cmd
}
