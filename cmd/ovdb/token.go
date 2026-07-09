package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// tokenFlags holds the flags shared across all token subcommands.
type tokenFlags struct {
	addr       string
	ownerToken string
}

// newTokenCmd returns the "ovdb token" command group.
func newTokenCmd() *cobra.Command {
	var tf tokenFlags
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage scoped API tokens (talks to a running ovdb server)",
		Long: `Manage revocable scoped tokens on a running OpenVaultDB server.

All subcommands talk to a running server via HTTP and require the owner token.
The --addr flag (or its default) must point at the server's listen address.
The --owner-token flag (or $OVDB_OWNER_TOKEN) authenticates as the instance owner.

Tokens are created and immediately usable without restarting the server.
Revoking a token takes effect immediately on the running server.`,
	}
	cmd.PersistentFlags().StringVar(&tf.addr, "addr", "http://"+DefaultAddr,
		"server address (host:port or full URL)")
	cmd.PersistentFlags().StringVar(&tf.ownerToken, "owner-token", "",
		"owner bearer token (default: $OVDB_OWNER_TOKEN)")

	cmd.AddCommand(
		newTokenCreateCmd(&tf),
		newTokenListCmd(&tf),
		newTokenRevokeCmd(&tf),
	)
	return cmd
}

// resolveOwnerToken returns the owner token from the flag or env, or an error.
func resolveOwnerToken(tf *tokenFlags) (string, error) {
	if tf.ownerToken != "" {
		return tf.ownerToken, nil
	}
	if v := os.Getenv("OVDB_OWNER_TOKEN"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("owner token required: set --owner-token or $OVDB_OWNER_TOKEN")
}

// serverBase normalises the address to a base URL (no trailing slash).
func serverBase(addr string) string {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// doRequest performs an authenticated HTTP request against the ovdb server.
func doRequest(method, url, ownerToken string, body any) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// scopeCapabilities maps a scope name to capability strings.
// Exported as a testable pure function.
func scopeCapabilities(scope string) ([]string, error) {
	switch scope {
	case "read-only":
		return []string{"records:read", "collections:read", "schema:read"}, nil
	case "read-write", "":
		return []string{"records:read", "collections:read", "schema:read",
			"records:write", "records:delete"}, nil
	default:
		return nil, fmt.Errorf("unknown scope %q: use read-only or read-write", scope)
	}
}

// newTokenCreateCmd returns "ovdb token create".
func newTokenCreateCmd(tf *tokenFlags) *cobra.Command {
	var dbID, label, scope, expires string
	var extraCaps []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new scoped token",
		Long: `Create a new scoped token on the running server.

The token secret is printed ONCE and never stored — save it immediately.

--scope read-only  → records:read, collections:read, schema:read
--scope read-write → read-only set + records:write, records:delete  (default)
--capability       → append extra raw capability strings (validated server-side)
--expires          → Go duration string e.g. 720h (default: never expires)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ownerToken, err := resolveOwnerToken(tf)
			if err != nil {
				return err
			}
			if dbID == "" {
				return fmt.Errorf("--db is required")
			}
			caps, err := scopeCapabilities(scope)
			if err != nil {
				return err
			}
			caps = append(caps, extraCaps...)

			type createReq struct {
				Label        string   `json:"label"`
				DatabaseID   string   `json:"databaseId"`
				Capabilities []string `json:"capabilities"`
				ExpiresIn    string   `json:"expiresIn,omitempty"`
			}
			reqBody := createReq{
				Label:        label,
				DatabaseID:   dbID,
				Capabilities: caps,
				ExpiresIn:    expires,
			}

			base := serverBase(tf.addr)
			b, status, err := doRequest(http.MethodPost, base+"/v1/tokens", ownerToken, reqBody)
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
				ID         string     `json:"id"`
				Token      string     `json:"token"`
				Label      string     `json:"label"`
				DatabaseID string     `json:"databaseId"`
				ExpiresAt  *time.Time `json:"expiresAt"`
			}
			if err = json.Unmarshal(b, &resp); err != nil {
				return fmt.Errorf("parse response: %w", err)
			}
			cmd.Println("Token created successfully.")
			cmd.Println("  ID:       ", resp.ID)
			if resp.Label != "" {
				cmd.Println("  Label:    ", resp.Label)
			}
			cmd.Println("  Database: ", resp.DatabaseID)
			if resp.ExpiresAt != nil {
				cmd.Println("  Expires:  ", resp.ExpiresAt.Format(time.RFC3339))
			} else {
				cmd.Println("  Expires:   never")
			}
			cmd.Println()
			cmd.Println("Token (save this — it will not be shown again):")
			cmd.Println("  " + resp.Token)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbID, "db", "", "database ID (required)")
	cmd.Flags().StringVar(&label, "label", "", "human-readable label for this token")
	cmd.Flags().StringVar(&scope, "scope", "", "capability scope: read-only or read-write (default read-write)")
	cmd.Flags().StringArrayVar(&extraCaps, "capability", nil, "additional capability string (repeatable)")
	cmd.Flags().StringVar(&expires, "expires", "", "token lifetime as a Go duration (e.g. 720h); default: never")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print raw JSON response")
	_ = cmd.MarkFlagRequired("db")
	return cmd
}

// newTokenListCmd returns "ovdb token list".
func newTokenListCmd(tf *tokenFlags) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			ownerToken, err := resolveOwnerToken(tf)
			if err != nil {
				return err
			}
			base := serverBase(tf.addr)
			b, status, err := doRequest(http.MethodGet, base+"/v1/tokens", ownerToken, nil)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("server returned %d: %s", status, b)
			}
			if jsonOut {
				cmd.Println(string(b))
				return nil
			}
			var resp struct {
				Tokens []struct {
					ID           string     `json:"id"`
					Label        string     `json:"label"`
					DatabaseID   string     `json:"databaseId"`
					Capabilities []string   `json:"capabilities"`
					IssuedAt     time.Time  `json:"issuedAt"`
					ExpiresAt    *time.Time `json:"expiresAt"`
					RevokedAt    *time.Time `json:"revokedAt"`
				} `json:"tokens"`
			}
			if err = json.Unmarshal(b, &resp); err != nil {
				return fmt.Errorf("parse response: %w", err)
			}
			if len(resp.Tokens) == 0 {
				cmd.Println("No tokens.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tLABEL\tDB\tCAPABILITIES\tISSUED\tEXPIRES\tREVOKED")
			for _, t := range resp.Tokens {
				expires := "never"
				if t.ExpiresAt != nil {
					expires = t.ExpiresAt.Format("2006-01-02")
				}
				revoked := ""
				if t.RevokedAt != nil {
					revoked = t.RevokedAt.Format("2006-01-02")
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					t.ID, t.Label, t.DatabaseID,
					strings.Join(t.Capabilities, ","),
					t.IssuedAt.Format("2006-01-02"),
					expires, revoked)
			}
			_ = tw.Flush()
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print raw JSON response")
	return cmd
}

// newTokenRevokeCmd returns "ovdb token revoke <token-id>".
func newTokenRevokeCmd(tf *tokenFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a token by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ownerToken, err := resolveOwnerToken(tf)
			if err != nil {
				return err
			}
			id := args[0]
			base := serverBase(tf.addr)
			b, status, err := doRequest(http.MethodDelete, base+"/v1/tokens/"+id, ownerToken, nil)
			if err != nil {
				return err
			}
			if status == http.StatusNotFound {
				return fmt.Errorf("token not found: %s", id)
			}
			if status != http.StatusOK {
				return fmt.Errorf("server returned %d: %s", status, b)
			}
			cmd.Printf("Token %s has been revoked.\n", id)
			return nil
		},
	}
}
