package cmd

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newKataAgreementCommand() *cobra.Command {
	root := &cobra.Command{Use: "agreement", Short: "Review accepted terms and proposed changes on an existing Kata issue"}
	show := &cobra.Command{
		Use: "show <ref>", Short: "Read saved agreement history and last accepted terms", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return usageErr(cmd, errors.New("kata issue ref is required"))
			}
			client, closeClient, err := openKataClient(cmd, daemonclient.KataAgreementMinAPISchemaVersion)
			if err != nil {
				return err
			}
			defer closeClient()
			view, err := client.GetKataAgreement(cmd.Context(), ref)
			if err != nil {
				return err
			}
			return writeJSONOutput(cmd, view)
		},
	}
	var inputFile string
	record := &cobra.Command{
		Use: "record <ref>", Short: "Record one explicitly reviewed and cited agreement event", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return usageErr(cmd, errors.New("kata issue ref is required"))
			}
			input, err := readKataInput[generated.KataAgreementRecordRequest](cmd, inputFile)
			if err != nil {
				return usageErr(cmd, err)
			}
			client, closeClient, err := openKataClient(cmd, daemonclient.KataAgreementMinAPISchemaVersion)
			if err != nil {
				return err
			}
			defer closeClient()
			view, err := client.RecordKataAgreement(cmd.Context(), ref, input)
			if err != nil {
				return err
			}
			return writeJSONOutput(cmd, view)
		},
	}
	record.Flags().StringVar(&inputFile, "input", "-", "Reviewed event JSON file, or - for stdin")
	root.AddCommand(show, record)
	return root
}
