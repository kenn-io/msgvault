package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newKataCmd() *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{Use: "kata", Short: "Create Kata issues that quote exact message and file evidence"}
	root.PersistentFlags().BoolVar(&jsonOutput, flagJSON, false, "Output the full JSON response")

	var prepareInput string
	prepare := &cobra.Command{
		Use:   "prepare",
		Short: "Prepare exact citations from message bodies or extracted file chunks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := readKataInput[generated.KataEvidencePrepareRequest](cmd, prepareInput)
			if err != nil {
				return usageErr(cmd, err)
			}
			client, closeClient, err := openKataClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.PrepareKataEvidence(cmd.Context(), request)
			if err != nil {
				return err
			}
			return writePersonAgendaJSON(cmd, result)
		},
	}
	prepare.Flags().StringVar(&prepareInput, "input", "-", "JSON request file, or - for stdin")
	evidence := &cobra.Command{Use: "evidence", Short: "Prepare exact archive evidence"}
	evidence.AddCommand(prepare)

	var createInput, idempotencyKey string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a Kata issue that quotes prepared evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := readKataInput[generated.KataIssueCreateRequest](cmd, createInput)
			if err != nil {
				return usageErr(cmd, err)
			}
			key := strings.TrimSpace(idempotencyKey)
			if key == "" {
				return usageErr(cmd, errors.New("--idempotency-key is required; choose a key that names this issue, and reuse it to retry"))
			}
			client, closeClient, err := openKataClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.CreateKataIssue(cmd.Context(), key, request)
			if err != nil {
				return err
			}
			return writeKataIssue(cmd, result, jsonOutput)
		},
	}
	create.Flags().StringVar(&createInput, "input", "-", "JSON request file, or - for stdin")
	create.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Required retry key that names this issue; reuse it with the same input to retry")

	var linkInput string
	link := &cobra.Command{
		Use:   "link <ref>",
		Short: "Add prepared evidence to an existing Kata issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := readKataInput[generated.KataEvidenceLinkRequest](cmd, linkInput)
			if err != nil {
				return usageErr(cmd, err)
			}
			client, closeClient, err := openKataClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.LinkKataEvidence(cmd.Context(), args[0], request)
			if err != nil {
				return err
			}
			return writeKataIssue(cmd, result, jsonOutput)
		},
	}
	link.Flags().StringVar(&linkInput, "input", "-", "JSON request file, or - for stdin")

	root.AddCommand(evidence, create, link)
	return root
}

// openKataClient refuses daemons older than the Kata issue routes, which would
// otherwise answer with a bare 404.
func openKataClient(cmd *cobra.Command) (*daemonclient.Client, func(), error) {
	client, closeClient, err := openPersonAgendaClient(cmd)
	if err != nil {
		return nil, nil, err
	}
	supported, err := client.SupportsAPISchemaVersion(cmd.Context(), kataIssuesMinAPISchemaVersion)
	if err != nil {
		closeClient()
		return nil, nil, fmt.Errorf("check daemon Kata issue support: %w", err)
	}
	if !supported {
		closeClient()
		return nil, nil, fmt.Errorf("this daemon is too old for Kata issues; upgrade it to API schema %s or newer", kataIssuesMinAPISchemaVersion)
	}
	return client, closeClient, nil
}

func readKataInput[T any](cmd *cobra.Command, path string) (T, error) {
	var value T
	reader := cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return value, err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, kataevidence.MaxRequestBytes+1))
	if err != nil {
		return value, err
	}
	if len(data) > kataevidence.MaxRequestBytes {
		return value, errors.New("kata input exceeds 512 KiB")
	}
	if err := json.Unmarshal(data, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, fmt.Errorf("invalid Kata input: %w", err)
	}
	return value, nil
}

func writeKataIssue(cmd *cobra.Command, result generated.KataIssueResponse, jsonOutput bool) error {
	if jsonOutput {
		return writePersonAgendaJSON(cmd, result)
	}
	replayed := ""
	if result.Replayed {
		replayed = fmt.Sprintf(" (already filed, %s)", textutil.SanitizeTerminal(result.Issue.Status))
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s%s\n", textutil.SanitizeTerminal(result.Issue.QualifiedRef), textutil.SanitizeTerminal(result.Issue.Title), replayed); err != nil {
		return fmt.Errorf("write Kata issue: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newKataCmd()) }
