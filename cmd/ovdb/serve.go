package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func newServeCmd() *cobra.Command {
	var addr, dir, dataDir string
	var manifests []string
	var authEnabled bool
	var ownerToken, authStorePath string
	var corsOrigins []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the OpenVaultDB API server",
		Long: `Run the OpenVaultDB HTTP API server over databases described by manifests.

Databases are mounted from --manifest files and/or every *.yaml manifest in --dir.
With --data-dir, databases can also be created at runtime (POST /v1/databases);
created databases persist as manifests in the data-dir and are remounted on restart.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbs := map[string]*core.Database{}
			if dir != "" {
				mounted, err := mount.Dir(dir)
				if err != nil {
					return err
				}
				dbs = mounted
			}
			for _, path := range manifests {
				db, err := mount.File(path)
				if err != nil {
					return err
				}
				if _, dup := dbs[db.ID()]; dup {
					return fmt.Errorf("%s: duplicate database id %q", path, db.ID())
				}
				dbs[db.ID()] = db
			}
			if dataDir != "" {
				// Rescan previously created databases (each persisted as a
				// manifest YAML in the data-dir by POST /v1/databases).
				if err := os.MkdirAll(dataDir, 0o755); err != nil {
					return fmt.Errorf("failed to create data dir %s: %w", dataDir, err)
				}
				created, err := mount.Dir(dataDir)
				if err != nil {
					return err
				}
				for id, db := range created {
					if _, dup := dbs[id]; dup {
						return fmt.Errorf("%s: duplicate database id %q (also in --data-dir)", dataDir, id)
					}
					dbs[id] = db
				}
			}
			if len(dbs) == 0 && dataDir == "" {
				return fmt.Errorf("no databases mounted: provide --manifest files, a --dir with *.yaml manifests, or a --data-dir for runtime-created databases")
			}
			defer func() {
				for _, db := range dbs {
					_ = db.Close()
				}
			}()

			for _, db := range dbs {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mounted %q (engine=%s, schema_mode=%s)\n",
					db.ID(), db.Manifest.Storage.Engine, db.Manifest.Database.SchemaMode)
			}

			var opts []server.Option
			if authEnabled {
				if ownerToken == "" {
					ownerToken = os.Getenv("OVDB_OWNER_TOKEN")
				}
				if ownerToken == "" {
					generated, err := auth.NewToken()
					if err != nil {
						return err
					}
					ownerToken = generated
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"auth enabled; generated owner token (set --owner-token or OVDB_OWNER_TOKEN to pin):\n  %s\n", ownerToken)
				} else {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "auth enabled")
				}
				store, err := auth.OpenStore(authStorePath)
				if err != nil {
					return err
				}
				opts = append(opts, server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
			}
			if dataDir != "" {
				opts = append(opts, server.WithDataDir(dataDir))
			}
			if len(corsOrigins) > 0 {
				corsCfg := server.ParseCORSOrigins(corsOrigins)
				if corsCfg != nil {
					opts = append(opts, server.WithCORS(corsCfg))
					// Warn when --cors * is combined with --auth=false on a non-loopback addr.
					if !authEnabled && isNonLoopback(addr) {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(),
							"WARNING: --cors * with auth disabled on a non-loopback address allows any browser origin to read and write all data without credentials")
					}
				}
			}

			srv := &http.Server{
				Addr:              addr,
				Handler:           server.New(version, dbs, opts...).Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "OpenVaultDB %s serving on http://%s\n", version, addr)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			select {
			case err := <-errCh:
				return err
			case <-sigCh:
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "shutting down...")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&addr, "addr", DefaultAddr, "listen address")
	cmd.Flags().StringVar(&dir, "dir", "", "directory with database manifest *.yaml files")
	cmd.Flags().StringArrayVar(&manifests, "manifest", nil, "database manifest file (repeatable)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "directory for runtime-created databases (enables POST /v1/databases)")
	cmd.Flags().BoolVar(&authEnabled, "auth", false, "require bearer tokens (owner token + app connect flow)")
	cmd.Flags().StringVar(&ownerToken, "owner-token", "", "owner bearer token (default: $OVDB_OWNER_TOKEN, else generated)")
	cmd.Flags().StringVar(&authStorePath, "auth-store", "ovdb-auth.json", "path of the persisted app-grants file (tokens stored hashed)")
	cmd.Flags().StringArrayVar(&corsOrigins, "cors", nil,
		"allowed CORS origin (repeatable; comma-separated values accepted).\n"+
			"Examples: --cors https://sneat.app --cors http://localhost:4200\n"+
			"Use --cors '*' to allow any origin (development only — use with caution on public addresses)")
	return cmd
}

// isNonLoopback reports whether addr (host:port) binds to a non-loopback
// interface.  It is a best-effort heuristic used only for the foot-gun warning.
func isNonLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	return true
}
