package cmd

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
			client, closeClient, err := openKataClient(cmd, kataEvidenceMinVersion(request.Selectors, func(s generated.Selector) bool { return s.Kind == generated.SelectorKindDocbankRendition }))
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
			client, closeClient, err := openKataClient(cmd, kataEvidenceMinVersion(request.Evidence, citesDocbank))
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
			client, closeClient, err := openKataClient(cmd, kataEvidenceMinVersion(request.Evidence, citesDocbank))
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

	var messageID, attachmentID int64
	issues := &cobra.Command{
		Use:   "issues",
		Short: "List Kata issues, open or closed, that cite a message or one of its files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			query := generated.FindKataIssuesQuery{MessageID: messageID}
			if cmd.Flags().Changed("attachment") {
				query.AttachmentID = &attachmentID
			}
			client, closeClient, err := openKataClient(cmd, kataLookupMinAPISchemaVersion)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.FindKataIssues(cmd.Context(), query)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			out := cmd.OutOrStdout()
			for _, issue := range result.Issues {
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", textutil.SanitizeTerminal(issue.QualifiedRef), textutil.SanitizeTerminal(issue.Status), textutil.SanitizeTerminal(issue.Title)); err != nil {
					return fmt.Errorf("write Kata issues: %w", err)
				}
			}
			if result.Truncated {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "More issues cite this source; see them in Kata."); err != nil {
					return fmt.Errorf("write Kata issues: %w", err)
				}
			}
			return nil
		},
	}
	issues.Flags().Int64Var(&messageID, "message", 0, "Message ID the issues cite (required)")
	_ = issues.MarkFlagRequired("message")
	issues.Flags().Int64Var(&attachmentID, "attachment", 0, "Only issues citing this attachment of the message")

	var offset int64
	issueContext := &cobra.Command{
		Use:   "context <ref>",
		Short: "Show each passage a Kata issue cites, with its state in the archive and the text around it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, closeClient, err := openKataClient(cmd, kataContextMinAPISchemaVersion)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.GetKataIssueContext(cmd.Context(), args[0], generated.GetKataIssueContextQuery{Offset: &offset})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			return writeKataIssueContext(cmd.OutOrStdout(), result)
		},
	}
	issueContext.Flags().Int64Var(&offset, "offset", 0, "Index of the first passage to show")

	root.AddCommand(evidence, create, link, issues, issueContext)
	return root
}

// kataEvidenceMinVersion needs a daemon that reads Docbank transcripts only
// when the input cites one.
func kataEvidenceMinVersion[T any](items []T, docbank func(T) bool) string {
	if slices.ContainsFunc(items, docbank) {
		return kataDocbankMinAPISchemaVersion
	}
	return kataIssuesMinAPISchemaVersion
}

func citesDocbank(ref generated.Reference) bool { return ref.Kind == generated.DocbankRendition }

// openKataClient refuses daemons older than the Kata route a command needs,
// which would otherwise answer with a bare 404.
func openKataClient(cmd *cobra.Command, minVersion string) (*daemonclient.Client, func(), error) {
	client, closeClient, err := openPersonAgendaClient(cmd)
	if err != nil {
		return nil, nil, err
	}
	supported, err := client.SupportsAPISchemaVersion(cmd.Context(), minVersion)
	if err != nil {
		closeClient()
		return nil, nil, fmt.Errorf("check daemon Kata issue support: %w", err)
	}
	if !supported {
		closeClient()
		return nil, nil, fmt.Errorf("this daemon is too old for Kata issues; upgrade it to API schema %s or newer", minVersion)
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

// writeKataIssueContext prints the issue, then each passage's state and
// source with the cited words in brackets inside the text around them.
func writeKataIssueContext(out io.Writer, result generated.KataIssueContextResponse) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\t%s\t%s\n", textutil.SanitizeTerminal(result.Issue.QualifiedRef), textutil.SanitizeTerminal(result.Issue.Status), textutil.SanitizeTerminal(result.Issue.Title))
	for _, passage := range result.Passages {
		display := passage.Evidence.Display
		// An unavailable reference may name another archive, where its IDs mean nothing here.
		ref := passage.Evidence.Reference
		source := fmt.Sprintf("message %d", ref.MessageID)
		switch {
		case passage.State == "unreachable":
			source = "Docbank unreachable; retry later"
		case passage.State == "unavailable" && ref.AttachmentID != nil:
			source = "attachment in this or another archive"
		case passage.State == "unavailable":
			source = "message in this or another archive"
		case ref.AttachmentID != nil:
			source = fmt.Sprintf("attachment %d of message %d", *ref.AttachmentID, ref.MessageID)
		}
		if name := cmp.Or(stringOrEmpty(display.Filename), stringOrEmpty(display.ContainingTitle)); name != "" {
			source += ": " + name
		}
		fmt.Fprintf(&b, "\n%s\t%s\n", textutil.SanitizeTerminal(passage.State), textutil.SanitizeTerminal(source))
		if passage.Evidence.Excerpt != "" {
			fmt.Fprintf(&b, "  %s[%s]%s\n", textutil.SanitizeTerminal(stringOrEmpty(passage.Before)), textutil.SanitizeTerminal(passage.Evidence.Excerpt), textutil.SanitizeTerminal(stringOrEmpty(passage.After)))
		}
		if quote := stringOrEmpty(passage.SavedQuote); quote != "" {
			fmt.Fprintf(&b, "  saved quote: %s\n", textutil.SanitizeTerminal(quote))
		}
	}
	if result.NextOffset != nil {
		fmt.Fprintf(&b, "\nmore passages: --offset %d\n", *result.NextOffset)
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("write Kata issue context: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newKataCmd()) }
