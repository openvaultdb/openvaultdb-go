package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func newServeCmd() *cobra.Command {
	var addr, dir string
	var manifests []string
	var authEnabled bool
	var ownerToken, authStorePath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the OpenVaultDB API server",
		Long: `Run the OpenVaultDB HTTP API server over databases described by manifests.

Databases are mounted from --manifest files and/or every *.yaml manifest in --dir.`,
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
			if len(dbs) == 0 {
				return fmt.Errorf("no databases mounted: provide --manifest files or a --dir with *.yaml manifests")
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

			srv := &http.Server{
				Addr:              addr,
				Handler:           server.New(Version, dbs, opts...).Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "OpenVaultDB %s serving on http://%s\n", Version, addr)

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
	cmd.Flags().BoolVar(&authEnabled, "auth", false, "require bearer tokens (owner token + app connect flow)")
	cmd.Flags().StringVar(&ownerToken, "owner-token", "", "owner bearer token (default: $OVDB_OWNER_TOKEN, else generated)")
	cmd.Flags().StringVar(&authStorePath, "auth-store", "ovdb-auth.json", "path of the persisted app-grants file (tokens stored hashed)")
	return cmd
}
