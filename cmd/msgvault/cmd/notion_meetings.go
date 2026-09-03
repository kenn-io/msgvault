package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncNotionMeetingsLimit int
	syncNotionMeetingsAfter string
	syncNotionMeetingsFull  bool
	syncNotionMeetingsProbe bool
)

var (
	newNotionMeetingsClient = func(baseURL, token string) notionmeetings.Source {
		return notionmeetings.NewClient(baseURL, token)
	}
	rebuildNotionMeetingsCacheAfterWrite         = rebuildCacheAfterWrite
	rebuildNotionMeetingsCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const notionMeetingsConfigHint = `Add to your config.toml:

  [[notion_meetings]]
  identifier = "notion-personal"      # stable label for this identity
  account_email = "you@example.com"   # primary identity for relationships
  token = "ntn_..."                   # read-only Notion integration token
  enabled = true
  # schedule = "15 */6 * * *"         # optional daemon schedule`

func resolveNotionMeetingsSource(args []string) (*config.NotionMeetingsSource, error) {
	if len(cfg.NotionMeetings) == 0 {
		return nil, errors.New("no [[notion_meetings]] sources configured\n\n" + notionMeetingsConfigHint)
	}
	if len(args) > 0 {
		source := cfg.GetNotionMeetingsSource(args[0])
		if source == nil {
			identifiers := make([]string, 0, len(cfg.NotionMeetings))
			for _, candidate := range cfg.NotionMeetings {
				identifiers = append(identifiers, candidate.Identifier)
			}
			return nil, fmt.Errorf("no [[notion_meetings]] entry with identifier %q (configured: %s)",
				args[0], strings.Join(identifiers, ", "))
		}
		return source, nil
	}
	if len(cfg.NotionMeetings) > 1 {
		return nil, errors.New("multiple [[notion_meetings]] sources configured; pass an identifier")
	}
	source := cfg.NotionMeetings[0]
	return &source, nil
}

func resolveNotionMeetingsSources(args []string, probe bool) ([]config.NotionMeetingsSource, error) {
	if probe || len(args) > 0 || len(cfg.NotionMeetings) == 1 {
		source, err := resolveNotionMeetingsSource(args)
		if err != nil {
			return nil, err
		}
		return []config.NotionMeetingsSource{*source}, nil
	}
	if len(cfg.NotionMeetings) == 0 {
		return nil, errors.New("no [[notion_meetings]] sources configured\n\n" + notionMeetingsConfigHint)
	}
	return cfg.NotionMeetings, nil
}

type notionMeetingsQuerySource interface {
	QueryMeetingNotes(ctx context.Context, limit int) (*notionmeetings.QueryResult, error)
	RetrieveBlock(ctx context.Context, blockID string) (*notionmeetings.Block, error)
	RetrievePageMarkdown(ctx context.Context, pageID string, includeTranscript bool) (*notionmeetings.MarkdownPage, error)
	ListUsers(ctx context.Context, cursor string) (*notionmeetings.UserPage, error)
}

func runNotionMeetingsProbe(ctx context.Context, out io.Writer, client notionMeetingsQuerySource) error {
	result, err := client.QueryMeetingNotes(ctx, 1)
	if err != nil {
		return fmt.Errorf("probe Notion AI Meeting Notes access: %w", err)
	}
	if result == nil {
		return errors.New("probe Notion AI Meeting Notes access returned no response")
	}
	_, _ = fmt.Fprintln(out, "Notion AI Meeting Notes probe succeeded.")
	_, _ = fmt.Fprintf(out, "  Returned meetings: %d\n", len(result.Results))
	_, _ = fmt.Fprintf(out, "  Partial coverage: %t\n", result.HasMore)
	if len(result.Results) == 0 {
		_, _ = fmt.Fprintln(out, "  Read Content: not tested (no visible meeting)")
	} else {
		meeting := result.Results[0]
		block, retrieveErr := client.RetrieveBlock(ctx, meeting.ID)
		if retrieveErr != nil {
			return fmt.Errorf("probe Notion meeting block access: %w", retrieveErr)
		}
		if block == nil {
			return errors.New("probe Notion meeting block access returned no response")
		}
		pageID := strings.TrimSpace(meeting.Parent.PageID)
		if pageID == "" {
			pageID = strings.TrimSpace(block.Parent.PageID)
		}
		if pageID == "" {
			return errors.New("probe result has no parent page ID")
		}
		if _, err := client.RetrievePageMarkdown(ctx, pageID, true); err != nil {
			return fmt.Errorf("probe Notion Read Content access: %w", err)
		}
		_, _ = fmt.Fprintln(out, "  Read Content: available")
	}
	if _, err := client.ListUsers(ctx, ""); errors.Is(err, notionmeetings.ErrUserInformation) {
		_, _ = fmt.Fprintln(out, "  User Information: unavailable (attendees remain display-only)")
	} else if errors.Is(err, notionmeetings.ErrRateLimited) {
		_, _ = fmt.Fprintf(out, "  User Information: unavailable (error: %v; attendees remain display-only)\n", err)
	} else if err != nil {
		return fmt.Errorf("probe Notion User Information access: %w", err)
	} else {
		_, _ = fmt.Fprintln(out, "  User Information: available")
	}
	return nil
}

var addNotionMeetingsCmd = &cobra.Command{
	Use:   "add-notion-meetings [identifier]",
	Short: "Register and validate a Notion AI Meeting Notes source",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		source, err := resolveNotionMeetingsSource(args)
		if err != nil {
			return err
		}
		accountEmail, err := source.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if strings.TrimSpace(source.Token) == "" {
			return fmt.Errorf("[[notion_meetings]] entry %q has no token\n\n%s", source.Identifier, notionMeetingsConfigHint)
		}
		client := newNotionMeetingsClient(notionmeetings.DefaultBaseURL, source.Token)
		if err := runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(), client); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeNotionMeetings,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrations(st); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nNotion meeting source %s registered.\n", source.Identifier)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Run: msgvault sync-notion-meetings %s\n", source.Identifier)
		return nil
	},
}

var syncNotionMeetingsCmd = &cobra.Command{
	Use:   "sync-notion-meetings [identifier]",
	Short: "Sync Notion AI Meeting Notes",
	Long: `Sync the latest visible Notion AI Meeting Notes into the canonical meeting archive.

Notion currently returns at most 50 attendee-visible meetings and does not
provide a discovery cursor. Every run checks that visible window. --after is
a local visible-set filter. --limit caps discovery work but not due transcript
maintenance. --probe validates access without printing meeting content.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveNotionMeetingsSources(args, syncNotionMeetingsProbe)
		if err != nil {
			return err
		}

		var after time.Time
		if syncNotionMeetingsAfter != "" {
			parsed, err := time.Parse(time.DateOnly, syncNotionMeetingsAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncNotionMeetingsAfter, err))
			}
			after = parsed.UTC()
		}
		for _, source := range sources {
			if strings.TrimSpace(source.Token) == "" {
				return fmt.Errorf("[[notion_meetings]] entry %q has no token", source.Identifier)
			}
			if _, err := source.EffectiveAccountEmail(); err != nil {
				return err
			}
		}
		if syncNotionMeetingsProbe {
			source := sources[0]
			return runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(),
				newNotionMeetingsClient(notionmeetings.DefaultBaseURL, source.Token))
		}

		st, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		pendingWrites := &notionmeetings.ImportSummary{}
		for _, source := range sources {
			accountEmail, _ := source.EffectiveAccountEmail()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Notion meetings for %s\n\n", source.Identifier)
			importer := notionmeetings.NewImporter(st,
				newNotionMeetingsClient(notionmeetings.DefaultBaseURL, source.Token))
			summary, importErr := importer.Import(cmd.Context(), notionmeetings.ImportOptions{
				Identifier: source.Identifier, AccountEmail: accountEmail,
				Full: syncNotionMeetingsFull || !after.IsZero(), Limit: syncNotionMeetingsLimit,
				CreatedAfter: after,
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			accumulateNotionMeetingsWrites(pendingWrites, summary)
			if err := finishNotionMeetingsImport(source.Identifier, pendingWrites, importErr,
				func() error { return rebuildNotionMeetingsCacheAfterWrite(dbPath) }); err != nil {
				return err
			}
			writeNotionMeetingsSummary(cmd.OutOrStdout(), summary)
		}
		return rebuildNotionMeetingsCacheAfterWrite(dbPath)
	},
}

func accumulateNotionMeetingsWrites(total, current *notionmeetings.ImportSummary) {
	if total == nil || current == nil {
		return
	}
	total.MeetingsAdded += current.MeetingsAdded
	total.MeetingsUpdated += current.MeetingsUpdated
}

func finishNotionMeetingsImport(identifier string, summary *notionmeetings.ImportSummary, importErr error, refresh func() error) error {
	if importErr == nil {
		return nil
	}
	var refreshErr error
	if summary != nil && summary.MeetingsAdded+summary.MeetingsUpdated > 0 && refresh != nil {
		refreshErr = refresh()
	}
	return errors.Join(fmt.Errorf("notion meetings sync %s failed: %w", identifier, importErr), refreshErr)
}

func writeNotionMeetingsSummary(out io.Writer, summary *notionmeetings.ImportSummary) {
	_, _ = fmt.Fprintln(out, "\nNotion meetings sync complete!")
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	if summary.MaintenanceRetries > 0 {
		_, _ = fmt.Fprintf(out, "  Maintenance items: %d\n", summary.MaintenanceRetries)
	}
	if summary.PartialCoverage {
		_, _ = fmt.Fprintln(out, "  Coverage: partial (Notion reports more than the visible 50-item window)")
	}
}

func runConfiguredNotionMeetingsSync(ctx context.Context, st *store.Store, source config.NotionMeetingsSource) error {
	if _, err := st.GetSourceByTypeAndIdentifier(notionmeetings.SourceType, source.Identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("notion meeting source %q is not registered; run msgvault add-notion-meetings %s first",
				source.Identifier, source.Identifier)
		}
		return err
	}
	if strings.TrimSpace(source.Token) == "" {
		return fmt.Errorf("notion meeting source %q has no token", source.Identifier)
	}
	accountEmail, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	importer := notionmeetings.NewImporter(st,
		newNotionMeetingsClient(notionmeetings.DefaultBaseURL, source.Token))
	summary, importErr := importer.Import(ctx, notionmeetings.ImportOptions{
		Identifier: source.Identifier, AccountEmail: accountEmail,
	})
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error {
		return rebuildNotionMeetingsCacheAfterScheduledSync(refreshCtx, "notion-meetings:"+source.Identifier)
	}
	if err := finishNotionMeetingsImport(source.Identifier, summary, importErr, refresh); err != nil {
		return err
	}
	return refresh()
}

func init() {
	syncNotionMeetingsCmd.Flags().IntVar(&syncNotionMeetingsLimit, "limit", 0,
		"max visible meetings hydrated and verified per run; due transcript maintenance is additional (0 = unlimited)")
	syncNotionMeetingsCmd.Flags().StringVar(&syncNotionMeetingsAfter, "after", "",
		"local visible-set lower bound (YYYY-MM-DD; implies --full)")
	syncNotionMeetingsCmd.Flags().BoolVar(&syncNotionMeetingsFull, "full", false,
		"force selected snapshots through archive upsert instead of skipping matching checksums")
	syncNotionMeetingsCmd.Flags().BoolVar(&syncNotionMeetingsProbe, "probe", false,
		"validate capabilities and result shape without printing meeting content")
	rootCmd.AddCommand(addNotionMeetingsCmd)
	rootCmd.AddCommand(syncNotionMeetingsCmd)
}
