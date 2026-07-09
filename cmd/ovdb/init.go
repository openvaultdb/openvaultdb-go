package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

func newInitCmd() *cobra.Command {
	var id, engine, mode, storagePath, out string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a database manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			if storagePath == "" {
				switch engine {
				case "sqlite":
					storagePath = fmt.Sprintf("./data/%s.sqlite", id)
				default:
					storagePath = fmt.Sprintf("./data/%s", id)
				}
			}
			content := fmt.Sprintf(`database:
  id: %s
  schema_mode: %s

storage:
  engine: %s
  path: %s
`, id, mode, engine, storagePath)
			if mode == "strict" {
				content += `
schemas:
  collections:
    # strict mode requires every collection to be declared before writes, e.g.:
    # contacts:
    #   fields:
    #     title: {type: string, required: true}
    example:
      fields:
        title: {type: string, required: true}
`
			}
			if _, err := manifest.Parse([]byte(content)); err != nil {
				return fmt.Errorf("generated manifest is invalid (bug): %w", err)
			}
			if out == "" {
				out = id + ".yaml"
			}
			if _, err := os.Stat(out); err == nil {
				return fmt.Errorf("%s already exists; refusing to overwrite", out)
			}
			if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "database id (required)")
	cmd.Flags().StringVar(&engine, "engine", "ingitdb", "storage engine: sqlite | ingitdb")
	cmd.Flags().StringVar(&mode, "schema-mode", "schemaless", "schema mode: strict | partial | schemaless")
	cmd.Flags().StringVar(&storagePath, "path", "", "storage path (default derived from id)")
	cmd.Flags().StringVar(&out, "out", "", "output manifest file (default <id>.yaml)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
