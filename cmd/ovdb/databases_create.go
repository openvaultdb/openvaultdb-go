package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// newDatabasesCreateCmd returns "ovdb databases create <id>": creates a
// database on a running server (requires --data-dir on the server side).
// Authenticates with --token (any bearer token that carries databases:create,
// e.g. an app provisioning token), --owner-token, or $OVDB_OWNER_TOKEN.
func newDatabasesCreateCmd() *cobra.Command {
	var addr, ownerToken, bearer, label string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create a database on a running OpenVaultDB server",
		Long: `Create a database at runtime on a running OpenVaultDB server.

The server must run with --data-dir. The caller must be the owner or hold a
token with the databases:create capability. When a non-owner (app) token
creates a database, the server also mints and returns a fresh token scoped to
the new database — printed ONCE, save it immediately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := bearer
			if token == "" {
				token = ownerToken
			}
			if token == "" {
				token = os.Getenv("OVDB_OWNER_TOKEN")
			}
			if token == "" {
				return fmt.Errorf("bearer token required: set --token, --owner-token or $OVDB_OWNER_TOKEN")
			}
			id := args[0]
			type createReq struct {
				ID    string `json:"id"`
				Label string `json:"label,omitempty"`
			}
			base := serverBase(addr)
			b, status, err := doRequest(http.MethodPost, base+"/v1/databases", token,
				createReq{ID: id, Label: label})
			if err != nil {
				return err
			}
			if status != http.StatusCreated {
				return fmt.Errorf("server returned %d: %s", status, b)
			}
			if jsonOut {
				cmd.Println(string(b))
				return nil
			}
			var resp struct {
				Database struct {
					ID         string `json:"id"`
					Engine     string `json:"engine"`
					SchemaMode string `json:"schemaMode"`
				} `json:"database"`
				Token *struct {
					ID         string     `json:"id"`
					Token      string     `json:"token"`
					DatabaseID string     `json:"databaseId"`
					ExpiresAt  *time.Time `json:"expiresAt"`
				} `json:"token"`
			}
			if err = json.Unmarshal(b, &resp); err != nil {
				return fmt.Errorf("parse response: %w", err)
			}
			cmd.Println("Database created successfully.")
			cmd.Println("  ID:          ", resp.Database.ID)
			cmd.Println("  Engine:      ", resp.Database.Engine)
			cmd.Println("  Schema mode: ", resp.Database.SchemaMode)
			if resp.Token != nil {
				cmd.Println()
				cmd.Println("Scoped token for this database (save it — it will not be shown again):")
				cmd.Println("  Token ID: ", resp.Token.ID)
				cmd.Println("  Token:    " + resp.Token.Token)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "http://"+DefaultAddr, "server address (host:port or full URL)")
	cmd.Flags().StringVar(&ownerToken, "owner-token", "", "owner bearer token (default: $OVDB_OWNER_TOKEN)")
	cmd.Flags().StringVar(&bearer, "token", "", "bearer token to use (e.g. an app databases:create token; overrides --owner-token)")
	cmd.Flags().StringVar(&label, "label", "", "human-readable label for the database (also labels the minted token)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print raw JSON response")
	return cmd
}
