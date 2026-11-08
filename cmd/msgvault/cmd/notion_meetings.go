package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sort"
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
  token_type = "pat"                  # infer verified human owner only for a PAT
  # user_id = "..."                   # required for other token modes
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

func runNotionMeetingsProbe(ctx context.Context, out io.Writer, client notionMeetingsQuerySource, users notionmeetings.UserSource, opts notionmeetings.ImportOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = notionmeetings.WithAttemptBudget(ctx, 512)

	_, _ = fmt.Fprintln(out, "Notion AI Meeting Notes access probe:")
	selfSource, ok := client.(interface {
		RetrieveSelf(ctx context.Context) (*notionmeetings.User, error)
	})
	if !ok {
		return &notionProbeError{stage: "probe Notion credentials", cause: notionmeetings.ErrUnknownIdentity}
	}
	self, err := selfSource.RetrieveSelf(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(out, "  Credentials: unavailable")
		return &notionProbeError{stage: "probe Notion credentials", cause: err}
	}
	_, _ = fmt.Fprintln(out, "  Credentials: self verified")
	identity, err := notionmeetings.ResolveIdentity(self, opts.TokenType, opts.UserID)
	if err != nil {
		_, _ = fmt.Fprintln(out, "  Identity: unknown (set token_type=pat or configure user_id)")
		return &notionProbeError{stage: "probe Notion identity", cause: err}
	}
	_, _ = fmt.Fprintf(out, "  Identity: %s\n", notionIdentityLabel(identity.Provenance))
	writeNotionScope(out, opts)
	result, err := client.QueryMeetingNotes(ctx, 1)
	if err != nil {
		_, _ = fmt.Fprintln(out, "  Meeting query: unavailable")
		if !errors.Is(err, notionmeetings.ErrMeetingAccess) && !errors.Is(err, notionmeetings.ErrReadContent) {
			return &notionProbeError{stage: "probe Notion meeting query", cause: err}
		}
		result = &notionmeetings.QueryResult{}
	} else {
		_, _ = fmt.Fprintln(out, "  Meeting query: available")
	}
	if result == nil {
		return &notionProbeError{stage: "probe Notion meeting query", cause: notionmeetings.ErrMalformedResponse}
	}
	_, _ = fmt.Fprintf(out, "  Returned meetings: %d\n", len(result.Results))
	_, _ = fmt.Fprintln(out, "  Coverage: sample only; sync coverage was not measured")
	var sampled *notionmeetings.HydratedMeeting
	sampleID := ""
	if len(result.Results) > 0 {
		sampleID = result.Results[0].ID
	} else if len(opts.MeetingBlockIDs) > 0 {
		sampleID = opts.MeetingBlockIDs[0]
	}
	if sampleID == "" {
		_, _ = fmt.Fprintln(out, "  Read Content: not tested (no visible meeting)")
	} else {
		block, loadErr := client.RetrieveBlock(ctx, sampleID)
		if loadErr != nil {
			return &notionProbeError{stage: "probe Notion sample header", cause: loadErr}
		}
		var meeting notionmeetings.MeetingNote
		if block == nil || json.Unmarshal(block.Raw, &meeting) != nil || meeting.ID != sampleID || meeting.Type != "meeting_notes" {
			return &notionProbeError{stage: "probe Notion sample header", cause: notionmeetings.ErrMalformedResponse}
		}
		matched := meeting.CreatedBy.ID == identity.UserID
		for _, id := range meeting.MeetingNotes.CalendarEvent.Attendees {
			if id == identity.UserID {
				matched = true
			}
		}
		if meeting.InTrash || meeting.Archived {
			_, _ = fmt.Fprintln(out, "  Sample hydration: not tested (archived or trash)")
		} else if !matched {
			_, _ = fmt.Fprintln(out, "  Sample hydration: not selected for connected human")
		} else {
			sampled, err = notionmeetings.NewHydrator(notionProbeContent{client}).HydrateBlock(ctx, block)
			if err != nil {
				_, _ = fmt.Fprintln(out, "  Sample hydration: unavailable")
				return &notionProbeError{stage: "probe Notion sample hydration", cause: err}
			}
			_, _ = fmt.Fprintln(out, "  Read Content: available")
			children := sampled.Discovery.MeetingNotes.Children
			incomplete := (children.SummaryBlockID != "" && (!sampled.SummaryTree.Complete || sampled.SummaryTree.Filtered)) || (children.NotesBlockID != "" && (!sampled.NotesTree.Complete || sampled.NotesTree.Filtered)) || (children.TranscriptBlockID != "" && (!sampled.TranscriptTree.Complete || sampled.TranscriptTree.Filtered))
			if incomplete {
				_, _ = fmt.Fprintln(out, "  Sample hydration: incomplete (some meeting sections could not be read)")
			} else if strings.TrimSpace(sampled.Transcript) == "" {
				_, _ = fmt.Fprintln(out, "  Transcript: pending")
			}
		}
	}

	if users == nil {
		page, err := client.ListUsers(ctx, "")
		if ctx.Err() != nil {
			return ctx.Err()
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
	if ctx.Err() != nil {
		return ctx.Err()
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

func notionImportOptions(source config.NotionMeetingsSource) notionmeetings.ImportOptions {
	accountEmail, _ := source.EffectiveAccountEmail()
	return notionmeetings.ImportOptions{Identifier: source.Identifier, AccountEmail: accountEmail, TokenType: source.TokenType, UserID: source.UserID, RootPageIDs: source.RootPageIDs, RootDataSourceIDs: source.RootDataSourceIDs, MeetingBlockIDs: source.MeetingBlockIDs, SearchAccessible: source.SearchAccessible}
}
func writeNotionScope(out io.Writer, opts notionmeetings.ImportOptions) {
	search := opts.SearchAccessible == nil || *opts.SearchAccessible
	_, _ = fmt.Fprintf(out, "  Scope: page roots: %d; data-source roots: %d; meeting IDs: %d; accessible search: %t\n", len(opts.RootPageIDs), len(opts.RootDataSourceIDs), len(opts.MeetingBlockIDs), search)
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
		if err := runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(), client, users, notionImportOptions(*source)); err != nil {
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
	Long: `Sync accessible Notion meeting blocks created by or attended by the configured human.

Search is best effort; this does not enumerate the entire workspace. Sync also
checks configured roots and remembered meeting IDs, resumes saved progress, and
refreshes old meetings.`,
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
			return runNotionMeetingsProbe(cmd.Context(), cmd.OutOrStdout(), client, users, notionImportOptions(sources[0]))
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		var pendingWrites int64
		for _, source := range sources {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Notion meetings for %s\n\n", source.Identifier)
			client, users := configuredNotionClients(source)
			importer := notionmeetings.NewImporter(st, client).WithUserSource(users)
			options := notionImportOptions(source)
			options.Full = syncNotionMeetingsFull
			options.Limit = syncNotionMeetingsLimit
			options.CreatedAfter = after
			options.Progress = func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }
			summary, importErr := importer.Import(cmd.Context(), options)
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

func notionIdentityLabel(provenance string) string {
	if provenance == "pat_owner" {
		return "PAT owner"
	}
	return strings.ReplaceAll(provenance, "_", " ")
}

func notionCoverageLabel(reason string) string {
	switch reason {
	case "query_cap":
		return "Query limit"
	case "inaccessible_subtree":
		return "Inaccessible content"
	default:
		if reason == "" {
			return "Unknown"
		}
		return strings.ToUpper(reason[:1]) + strings.ReplaceAll(reason[1:], "_", " ")
	}
}
func writeNotionMeetingsSummary(out io.Writer, summary *notionmeetings.ImportSummary) {
	_, _ = fmt.Fprintln(out, "\nNotion meetings sync complete!")
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	_, _ = fmt.Fprintf(out, "  Identity: %s\n", notionIdentityLabel(summary.IdentityProvenance))
	_, _ = fmt.Fprintf(out, "  Scope: page roots: %d; data-source roots: %d; meeting IDs: %d; accessible search: %t\n", summary.ScopeCounts["page_roots"], summary.ScopeCounts["data_source_roots"], summary.ScopeCounts["meeting_roots"], summary.ScopeCounts["accessible_search"] > 0)
	_, _ = fmt.Fprintf(out, "  Work remaining: %d\n", summary.WorkRemaining)
	reasons := make([]string, 0, len(summary.CoverageReasons))
	for reason := range summary.CoverageReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(out, "  %s: %d\n", notionCoverageLabel(reason), summary.CoverageReasons[reason])
	}

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
	_, err = source.EffectiveAccountEmail()
	if err != nil {
		return recordSyncAttempt(ctx, notionmeetings.SourceType, source.Identifier, err)
	}
	client, users := configuredNotionClients(source)
	importer := notionmeetings.NewImporter(st, client).WithUserSource(users)
	summary, importErr := importer.Import(ctx, notionImportOptions(source))
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
		"new meetings per run; existing meetings refresh separately (0 = unlimited)")
	syncNotionMeetingsCmd.Flags().StringVar(&syncNotionMeetingsAfter, "after", "",
		"new meetings from YYYY-MM-DD; existing meetings still refresh")
	syncNotionMeetingsCmd.Flags().BoolVar(&syncNotionMeetingsFull, "full", false,
		"force a limited refresh; rescan after pending work")
	syncNotionMeetingsCmd.Flags().BoolVar(&syncNotionMeetingsProbe, "probe", false,
		"validate capabilities and result shape without printing meeting content")
	rootCmd.AddCommand(addNotionMeetingsCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncNotionMeetingsCmd))
}
