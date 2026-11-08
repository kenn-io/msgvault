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
	newNotionUsersClient = func(baseURL, token string) notionmeetings.UserSource {
		return notionmeetings.NewClient(baseURL, token)
	}
	rebuildNotionMeetingsCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildNotionMeetingsCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const notionMeetingsConfigHint = `Add to your config.toml:

  [[notion_meetings]]
  identifier = "notion-personal"      # stable label for this identity
  account_email = "you@example.com"   # primary identity for relationships
  token = "ntn_..."                   # read-only Notion integration token
  enabled = true
  # schedule = "15 */6 * * *"         # optional daemon schedule`

func notionMeetingsSources(cfg *config.Config) meetingSources[config.NotionMeetingsSource] {
	sources := meetingSources[config.NotionMeetingsSource]{
		table: "notion_meetings", hint: notionMeetingsConfigHint,
	}
	if cfg != nil {
		sources.configured, sources.lookup = cfg.NotionMeetings, cfg.GetNotionMeetingsSource
		sources.identifier = func(s config.NotionMeetingsSource) string { return s.Identifier }
	}
	return sources
}

func resolveNotionMeetingsSources(args []string, probe bool, cfg *config.Config) ([]config.NotionMeetingsSource, error) {
	sources := notionMeetingsSources(cfg)
	if !probe {
		return sources.selected(args)
	}
	source, err := sources.one(args)
	if err != nil {
		return nil, err
	}
	return []config.NotionMeetingsSource{*source}, nil
}

type notionMeetingsQuerySource interface {
	QueryMeetingNotes(ctx context.Context, limit int) (*notionmeetings.QueryResult, error)
	RetrieveBlock(ctx context.Context, blockID string) (*notionmeetings.Block, error)
	RetrieveBlockChildren(ctx context.Context, blockID, cursor string) (*notionmeetings.BlockPage, error)
	ListUsers(ctx context.Context, cursor string) (*notionmeetings.UserPage, error)
}

// Probe content independently of optional attendee lookup. The latter is sampled
// separately so probing a meeting does not resolve every attendee's identity.
type notionProbeContent struct{ notionMeetingsQuerySource }

func (notionProbeContent) ListUsers(context.Context, string) (*notionmeetings.UserPage, error) {
	return nil, notionmeetings.ErrUserInformation
}

type notionProbeError struct {
	stage string
	cause error
}

func (e *notionProbeError) Error() string {
	reason := "request unavailable"
	switch {
	case errors.Is(e.cause, notionmeetings.ErrUnauthorized):
		reason = "integration token is invalid or expired"
	case errors.Is(e.cause, notionmeetings.ErrMeetingAccess):
		reason = "Meeting Notes access is unavailable"
	case errors.Is(e.cause, notionmeetings.ErrReadContent):
		reason = "Read Content access is unavailable"
	case errors.Is(e.cause, notionmeetings.ErrRateLimited):
		reason = "provider retry budget exhausted"
	case errors.Is(e.cause, notionmeetings.ErrMalformedResponse):
		reason = "malformed response"
	case errors.Is(e.cause, context.Canceled):
		reason = "operation canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		reason = "operation deadline exceeded"
	}
	return e.stage + ": " + reason
}
func (e *notionProbeError) Unwrap() error { return e.cause }

func runNotionMeetingsProbe(ctx context.Context, out io.Writer, client notionMeetingsQuerySource, users notionmeetings.UserSource) error {
	return runNotionMeetingsProbeWithTimeout(ctx, out, client, users, 2*time.Minute)
}

func runNotionMeetingsProbeWithTimeout(ctx context.Context, out io.Writer, client notionMeetingsQuerySource, users notionmeetings.UserSource, timeout time.Duration) error {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = fmt.Fprintln(out, "Notion AI Meeting Notes access probe:")
	result, err := client.QueryMeetingNotes(ctx, 1)
	if err != nil {
		if !errors.Is(err, notionmeetings.ErrUnauthorized) {
			_, _ = fmt.Fprintln(out, "  Credentials: not verified by meeting query")
		}
		_, _ = fmt.Fprintln(out, "  Meeting query: unavailable")
		_, _ = fmt.Fprintln(out, "  Sample content: not tested")
		return &notionProbeError{stage: "probe Notion meeting query", cause: err}
	}
	if result == nil {
		return &notionProbeError{stage: "probe Notion meeting query", cause: notionmeetings.ErrMalformedResponse}
	}
	_, _ = fmt.Fprintln(out, "  Credentials: query verified")
	_, _ = fmt.Fprintln(out, "  Meeting query: available")
	_, _ = fmt.Fprintf(out, "  Returned meetings: %d\n", len(result.Results))
	_, _ = fmt.Fprintln(out, "  Coverage: sample only; sync coverage was not measured")
	var sampled *notionmeetings.HydratedMeeting
	if len(result.Results) == 0 {
		_, _ = fmt.Fprintln(out, "  Read Content: not tested (no visible meeting)")
	} else {
		sampled, err = notionmeetings.NewHydrator(notionProbeContent{client}).Hydrate(ctx, result.Results[0])
		if err != nil {
			return &notionProbeError{stage: "check sample content", cause: err}
		}
		_, _ = fmt.Fprintln(out, "  Read Content: available")
		children := sampled.Discovery.MeetingNotes.Children
		incomplete := (children.SummaryBlockID != "" && (!sampled.SummaryTree.Complete || sampled.SummaryTree.Filtered)) ||
			(children.NotesBlockID != "" && (!sampled.NotesTree.Complete || sampled.NotesTree.Filtered)) ||
			(children.TranscriptBlockID != "" && (!sampled.TranscriptTree.Complete || sampled.TranscriptTree.Filtered))
		if incomplete {
			_, _ = fmt.Fprintln(out, "  Sample content: incomplete (some meeting sections could not be read)")
		} else if strings.TrimSpace(sampled.Transcript) == "" {
			_, _ = fmt.Fprintln(out, "  Transcript: pending")
		}
	}
	if users == nil {
		page, err := client.ListUsers(ctx, "")
		if parentCtx.Err() != nil {
			return parentCtx.Err()
		}
		if err != nil || page == nil {
			_, _ = fmt.Fprintln(out, "  User Information: unavailable (attendees remain display-only unless a users token is configured)")
		} else {
			_, _ = fmt.Fprintln(out, "  User Information: available")
		}
		return nil
	}
	attendeeID := ""
	if sampled != nil {
		for _, id := range sampled.Discovery.MeetingNotes.CalendarEvent.Attendees {
			if strings.TrimSpace(id) != "" {
				attendeeID = id
				break
			}
		}
	}
	if attendeeID == "" {
		_, _ = fmt.Fprintln(out, "  Users token: untested (no visible attendee ID)")
		return nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, notionmeetings.UserLookupTimeout)
	defer cancelLookup()
	user, err := users.RetrieveUser(lookupCtx, attendeeID)
	if parentCtx.Err() != nil {
		return parentCtx.Err()
	}
	if err != nil || user == nil {
		_, _ = fmt.Fprintln(out, "  Users token: unavailable (attendees remain display-only)")
	} else if user.Person.EmailVerified && strings.TrimSpace(user.Person.Email) != "" {
		_, _ = fmt.Fprintln(out, "  Users token: verified email available for sampled attendee")
	} else {
		_, _ = fmt.Fprintln(out, "  Users token: no verified email for sampled attendee (check Read user information including email addresses)")
	}
	return nil
}

func configuredNotionClients(source config.NotionMeetingsSource) (notionmeetings.Source, notionmeetings.UserSource) {
	var users notionmeetings.UserSource
	if token := strings.TrimSpace(source.UsersToken); token != "" {
		users = newNotionUsersClient(notionmeetings.DefaultBaseURL, token)
	}
	return newNotionMeetingsClient(notionmeetings.DefaultBaseURL, source.Token), users
}

var addNotionMeetingsCmd = &cobra.Command{
	Use:   "add-notion-meetings [identifier]",
	Short: "Register and validate a Notion AI Meeting Notes source",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		source, err := notionMeetingsSources(cfg).one(args)
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
		client, users := configuredNotionClients(*source)
		if err := runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(), client, users); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeNotionMeetings,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), st, state); err != nil {
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
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveNotionMeetingsSources(args, syncNotionMeetingsProbe, cfg)
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
			client, users := configuredNotionClients(sources[0])
			return runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(), client, users)
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		var pendingWrites int64
		for _, source := range sources {
			accountEmail, _ := source.EffectiveAccountEmail()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Notion meetings for %s\n\n", source.Identifier)
			client, users := configuredNotionClients(source)
			importer := notionmeetings.NewImporter(st, client).WithUserSource(users)
			summary, importErr := importer.Import(cmd.Context(), notionmeetings.ImportOptions{
				Identifier: source.Identifier, AccountEmail: accountEmail,
				Full: syncNotionMeetingsFull || !after.IsZero(), Limit: syncNotionMeetingsLimit,
				CreatedAfter: after,
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			if summary != nil {
				pendingWrites += summary.MeetingsAdded + summary.MeetingsUpdated
			}
			run := meetingSyncRun{
				provider: "notion meetings", identifier: source.Identifier,
				writes: pendingWrites, err: importErr,
			}
			if err := run.finish(func() error {
				return rebuildNotionMeetingsCacheAfterWrite(dbPath, state)
			}); err != nil {
				return err
			}
			writeNotionMeetingsSummary(cmd.OutOrStdout(), summary)
		}
		return rebuildNotionMeetingsCacheAfterWrite(dbPath, state)
	},
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
		_, _ = fmt.Fprintln(out, "  Coverage: partial (some meetings or sections remain incomplete)")
	}
}

func runConfiguredNotionMeetingsSync(ctx context.Context, st *store.Store, source config.NotionMeetingsSource) error {
	notRegistered := fmt.Errorf(
		"notion meeting source %q is not registered; run msgvault add-notion-meetings %s first",
		source.Identifier, source.Identifier)
	registered, err := requireRegisteredMeetingSource(
		ctx,
		st, notionmeetings.SourceType, source.Identifier, notRegistered,
	)
	if err != nil {
		return recordSyncAttempt(ctx, notionmeetings.SourceType, source.Identifier, err)
	}
	if registered.MergedIntoSourceID != 0 {
		return nil
	}
	if strings.TrimSpace(source.Token) == "" {
		return recordSyncAttempt(ctx, notionmeetings.SourceType, source.Identifier, fmt.Errorf("notion meeting source %q has no token", source.Identifier))
	}
	accountEmail, err := source.EffectiveAccountEmail()
	if err != nil {
		return recordSyncAttempt(ctx, notionmeetings.SourceType, source.Identifier, err)
	}
	client, users := configuredNotionClients(source)
	importer := notionmeetings.NewImporter(st, client).WithUserSource(users)
	summary, importErr := importer.Import(ctx, notionmeetings.ImportOptions{
		Identifier: source.Identifier, AccountEmail: accountEmail,
	})
	var writes int64
	if summary != nil {
		writes = summary.MeetingsAdded + summary.MeetingsUpdated
	}
	run := meetingSyncRun{
		provider: "notion meetings", identifier: source.Identifier, writes: writes, err: importErr,
	}
	_ = recordSyncAttempt(ctx, notionmeetings.SourceType, source.Identifier, importErr)
	return run.finishScheduled(ctx, "notion-meetings:"+source.Identifier,
		rebuildNotionMeetingsCacheAfterScheduledSync)
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
	rootCmd.AddCommand(addManualSyncCacheFlags(syncNotionMeetingsCmd))
}
