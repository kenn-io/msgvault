package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	personresolver "go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/vector/visual"
)

var (
	multimodalSearchImage     string
	multimodalSearchLimit     int
	multimodalSearchJSON      bool
	multimodalBuildYes        bool
	multimodalStatusJSON      bool
	multimodalSenderPersonID  int64
	multimodalPersonID        int64
	multimodalParticipantID   int64
	multimodalDirections      []string
	multimodalSearchAfter     string
	multimodalSearchBefore    string
	multimodalSearchCursor    string
	multimodalSearchSourceID  int64
	multimodalSearchMessageID int64
	multimodalSearchFilename  string
	multimodalSearchMIME      string
	multimodalRetireYes       bool
	multimodalRetryMessageID  int64
	multimodalRetryHash       string
)

const (
	multimodalStatusSubcommand = "status"
	multimodalBuildSubcommand  = "build"
)

var multimodalCmd = &cobra.Command{Use: "multimodal", Short: "Manage and search visual attachment embeddings"}

var multimodalSearchCmd = &cobra.Command{
	Use:   "search [text]",
	Short: "Search attachment pixels by text or a query image",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		text := strings.TrimSpace(strings.Join(args, " "))
		if (text == "") == (multimodalSearchImage == "") {
			return usageErr(cmd, errors.New("provide exactly one text query or --image"))
		}
		if multimodalSearchLimit < 1 || multimodalSearchLimit > 100 {
			return usageErr(cmd, errors.New("--limit must be between 1 and 100"))
		}
		if multimodalSenderPersonID < 0 || multimodalPersonID < 0 || multimodalParticipantID < 0 || multimodalSearchSourceID < 0 || multimodalSearchMessageID < 0 {
			return usageErr(cmd, errors.New("ID filters must be positive"))
		}
		if multimodalSenderPersonID > 0 && (multimodalPersonID > 0 || multimodalParticipantID > 0 || len(multimodalDirections) > 0) {
			return usageErr(cmd, errors.New("--sender-person cannot be combined with --person, --participant, or --direction"))
		}
		if multimodalPersonID > 0 && multimodalParticipantID > 0 {
			return usageErr(cmd, errors.New("--person and --participant are mutually exclusive"))
		}
		if len(multimodalDirections) > 0 && multimodalPersonID == 0 && multimodalParticipantID == 0 {
			return usageErr(cmd, errors.New("--direction requires --person or --participant"))
		}
		directions := make([]personscope.Direction, len(multimodalDirections))
		for i, direction := range multimodalDirections {
			directions[i] = personscope.Direction(direction)
		}
		if len(directions) > 0 {
			if _, _, err := personresolver.NormalizeDirections(directions); err != nil {
				return usageErr(cmd, err)
			}
		}
		var image []byte
		if multimodalSearchImage != "" {
			file, err := os.Open(multimodalSearchImage)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Size() > visual.MaxQueryImageBytes {
				return errors.New("query image exceeds 20 MiB")
			}
			image, err = io.ReadAll(io.LimitReader(file, visual.MaxQueryImageBytes+1))
			if err != nil {
				return err
			}
			if int64(len(image)) > visual.MaxQueryImageBytes {
				return errors.New("query image exceeds 20 MiB")
			}
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		var after, before *time.Time
		if multimodalSearchAfter != "" {
			parsed, parseErr := time.Parse("2006-01-02", multimodalSearchAfter)
			if parseErr != nil {
				return usageErr(cmd, errors.New("--after must use YYYY-MM-DD"))
			}
			after = &parsed
		}
		if multimodalSearchBefore != "" {
			parsed, parseErr := time.Parse("2006-01-02", multimodalSearchBefore)
			if parseErr != nil {
				return usageErr(cmd, errors.New("--before must use YYYY-MM-DD"))
			}
			before = &parsed
		}
		if after != nil && before != nil && !after.Before(*before) {
			return usageErr(cmd, errors.New("--after must be before --before"))
		}
		response, err := client.SearchVisualAttachmentsFiltered(cmd.Context(), daemonclient.VisualSearchOptions{
			Text: text, Image: image, Limit: multimodalSearchLimit, Cursor: multimodalSearchCursor,
			SenderPersonID: multimodalSenderPersonID, SourceID: multimodalSearchSourceID,
			PersonID: multimodalPersonID, ParticipantID: multimodalParticipantID, Directions: directions,
			MessageID: multimodalSearchMessageID, Filename: multimodalSearchFilename,
			MIMEPrefix: multimodalSearchMIME, After: after, Before: before,
		})
		if err != nil {
			return err
		}
		if multimodalSearchJSON {
			return printJSON(response)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "RANK\tDATE\tFILE\tMESSAGE\tSCORE")
		for _, result := range response.Results {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%.4f\n", result.Rank,
				result.SentAt.Format("2006-01-02"), result.Filename, result.MessageID, result.Score)
		}
		if response.NextCursor != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "next cursor:", response.NextCursor)
		}
		return w.Flush()
	},
}

var multimodalStatusCmd = &cobra.Command{
	Use: multimodalStatusSubcommand, Short: "Show visual attachment embedding status", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		// The operator status view includes per-format coverage; the daemon
		// serializes the underlying archive scan.
		status, err := client.VisualStatusWithCoverage(cmd.Context())
		if err != nil {
			return err
		}
		if multimodalStatusJSON {
			return printJSON(status)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Generation #%d: %s\nCurrent: %d/%d  stale: %d  retryable: %d  journal lag: %d\n",
			status.Generation.ID, status.Generation.State, status.Current, status.Eligible,
			status.Stale, status.Retryable, status.JournalLag)
		if err != nil {
			return fmt.Errorf("write visual status: %w", err)
		}
		return nil
	},
}

var multimodalBuildCmd = &cobra.Command{
	Use: multimodalBuildSubcommand, Short: "Consent and build visual attachment embeddings", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !multimodalBuildYes {
			return usageErr(cmd, errors.New("hosted visual processing sends eligible attachment bytes and bounded message context to the configured provider; pass --yes to continue"))
		}
		return runVisualBuildLoop(cmd, true)
	},
}

var multimodalResumeCmd = &cobra.Command{
	Use: cmdUseResume, Short: "Resume a consented visual attachment build", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return runVisualBuildLoop(cmd, false) },
}

func runVisualBuildLoop(cmd *cobra.Command, consent bool) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	first := true
	for {
		var status *visual.Status
		if first && consent {
			status, err = client.ConsentVisualBuildPass(cmd.Context())
		} else {
			status, err = client.RunVisualBuildPass(cmd.Context())
		}
		first = false
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "visual embeddings: %d/%d current, %d stale, journal lag %d\n",
			status.Current, status.Eligible, status.Stale, status.JournalLag)
		if status.Generation.State == "active" && status.JournalLag == 0 && status.Stale == 0 && status.Converged == status.ConvergenceTotal {
			return nil
		}
		if status.Retryable > 0 {
			return errors.New("visual build has retryable unavailable/provider work; fix the reported condition and retry or resume")
		}
	}
}

var multimodalRetryCmd = &cobra.Command{
	Use: "retry", Short: "Retry one visual attachment owner", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if multimodalRetryMessageID <= 0 || strings.TrimSpace(multimodalRetryHash) == "" {
			return usageErr(cmd, errors.New("--message and --hash are required"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		status, err := client.RetryVisualOwner(cmd.Context(), multimodalRetryMessageID, multimodalRetryHash)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Retried message %d in generation #%d\n", multimodalRetryMessageID, status.Generation.ID)
		if err != nil {
			return fmt.Errorf("write visual retry status: %w", err)
		}
		return nil
	},
}

var multimodalRetireCmd = &cobra.Command{
	Use: "retire <generation-id>", Short: "Retire a visual generation and delete its vectors", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !multimodalRetireYes {
			return usageErr(cmd, errors.New("pass --yes to retire the visual generation and delete its vectors"))
		}
		generationID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || generationID <= 0 {
			return usageErr(cmd, errors.New("generation-id must be positive"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		return client.RetireVisualGeneration(cmd.Context(), generationID)
	},
}

func init() {
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchImage, "image", "", "Search using a local JPEG, PNG, or WebP")
	multimodalSearchCmd.Flags().IntVar(&multimodalSearchLimit, "limit", 20, "Maximum results")
	multimodalSearchCmd.Flags().BoolVar(&multimodalSearchJSON, "json", false, "Output JSON")
	multimodalSearchCmd.Flags().Int64Var(&multimodalSenderPersonID, "sender-person", 0, "Only attachments sent by this person ID")
	multimodalSearchCmd.Flags().Int64Var(&multimodalPersonID, "person", 0, "Only attachments related to this durable person ID")
	multimodalSearchCmd.Flags().Int64Var(&multimodalParticipantID, "participant", 0, "Only attachments related to this observed participant")
	multimodalSearchCmd.Flags().StringSliceVar(&multimodalDirections, "direction", nil, "Person relation: from_person, to_person, or group")
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchAfter, "after", "", "Only messages on or after YYYY-MM-DD")
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchBefore, "before", "", "Only messages before YYYY-MM-DD")
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchCursor, "cursor", "", "Continue from an opaque search cursor")
	multimodalSearchCmd.Flags().Int64Var(&multimodalSearchSourceID, "source", 0, "Only attachments from this source ID")
	multimodalSearchCmd.Flags().Int64Var(&multimodalSearchMessageID, "message", 0, "Only attachments owned by this message ID")
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchFilename, "filename", "", "Case-insensitive filename substring")
	multimodalSearchCmd.Flags().StringVar(&multimodalSearchMIME, "mime-prefix", "", "Case-insensitive MIME prefix")
	multimodalStatusCmd.Flags().BoolVar(&multimodalStatusJSON, "json", false, "Output JSON")
	multimodalBuildCmd.Flags().BoolVar(&multimodalBuildYes, "yes", false, "Confirm hosted processing and skip the disclosure prompt")
	multimodalRetryCmd.Flags().Int64Var(&multimodalRetryMessageID, "message", 0, "Owning message ID")
	multimodalRetryCmd.Flags().StringVar(&multimodalRetryHash, "hash", "", "Attachment SHA-256 hash")
	multimodalRetireCmd.Flags().BoolVar(&multimodalRetireYes, "yes", false, "Confirm retirement and vector deletion")
	multimodalCmd.AddCommand(multimodalSearchCmd, multimodalStatusCmd, multimodalBuildCmd, multimodalResumeCmd, multimodalRetryCmd, multimodalRetireCmd)
	rootCmd.AddCommand(multimodalCmd)
}
