package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
)

func newOpenAPICmd() *cobra.Command {
	var version string
	var format string
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "Print the msgvault OpenAPI schema",
		Long: `Print the msgvault OpenAPI schema to stdout.

The schema is generated in-process from the daemon's Huma route definitions; it
does not require a running daemon or a database. Use --version 3.0 for client
generators that do not yet consume OpenAPI 3.1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := renderOpenAPI(version, format)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
	cmd.Flags().StringVar(&version, "version", "3.1", "OpenAPI version to print: 3.1 or 3.0")
	cmd.Flags().StringVar(&format, "format", "yaml", "OpenAPI format to print: yaml or json")
	return cmd
}

func renderOpenAPI(version, format string) ([]byte, error) {
	switch format {
	case "yaml", "yml":
		return api.OpenAPIYAMLVersion(version)
	case "json":
		return api.OpenAPIJSONVersion(version)
	default:
		return nil, fmt.Errorf("unsupported openapi format %q", format)
	}
}

func init() {
	rootCmd.AddCommand(newOpenAPICmd())
}
