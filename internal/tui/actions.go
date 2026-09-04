package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/textutil"
)

// ExportResultMsg is returned when attachment export completes.
type ExportResultMsg struct {
	Title  string
	Result string
	Err    error
}

// DeletionContext bundles the parameters needed for staging deletions.
type DeletionContext struct {
	AggregateSelection map[string]bool
	MessageSelection   map[int64]bool
	AggregateViewType  query.ViewType
	AccountFilter      *int64
	SourceIDs          []int64
	Accounts           []query.AccountInfo
	TimeGranularity    query.TimeGranularity
	Messages           []query.MessageSummary
	DrillFilter        *query.MessageFilter
	AllMatches         bool
	SearchQuery        string
	SearchMode         searchModeKind
	MatchFilter        query.MessageFilter
	AggregateMatchKey  *string
}

type allMatchesManifestProvenance struct {
	Scope       string                   `json:"scope"`
	SearchQuery string                   `json:"search_query,omitempty"`
	SearchMode  string                   `json:"search_mode,omitempty"`
	MatchFilter allMatchesManifestFilter `json:"match_filter"`
}

type allMatchesManifestFilter struct {
	Sender                string     `json:"sender,omitempty"`
	SenderName            string     `json:"sender_name,omitempty"`
	Recipient             string     `json:"recipient,omitempty"`
	RecipientName         string     `json:"recipient_name,omitempty"`
	Domain                string     `json:"domain,omitempty"`
	Label                 string     `json:"label,omitempty"`
	ListID                string     `json:"list_id,omitempty"`
	MessageType           string     `json:"message_type,omitempty"`
	ConversationID        *int64     `json:"conversation_id,omitempty"`
	EmptyValueTargets     []string   `json:"empty_value_targets,omitempty"`
	TimePeriod            string     `json:"time_period,omitempty"`
	TimeGranularity       string     `json:"time_granularity,omitempty"`
	SourceID              *int64     `json:"source_id,omitempty"`
	SourceIDs             []int64    `json:"source_ids,omitempty"`
	After                 *time.Time `json:"after,omitempty"`
	Before                *time.Time `json:"before,omitempty"`
	WithAttachmentsOnly   bool       `json:"attachments_only,omitempty"`
	HideDeletedFromSource bool       `json:"hide_deleted_from_source,omitempty"`
}

// ActionController handles business logic for actions like deletion and export,
// keeping domain operations out of the TUI Model.
type ActionController struct {
	queries             query.Engine
	deletions           *deletion.Manager
	dataDir             string
	manifestSaver       DeletionManifestSaver
	attachmentReader    AttachmentReader
	attachmentOutputDir string
	openTarget          func(context.Context, string) error
	markUntrusted       func(string) error
}

// DeletionManifestSaver saves a staged deletion manifest.
type DeletionManifestSaver interface {
	SaveManifest(manifest *deletion.Manifest) error
}

// AttachmentReader opens attachment content by content hash.
type AttachmentReader interface {
	OpenAttachment(ctx context.Context, contentHash string) (io.ReadCloser, error)
}

// ActionControllerOptions configures external action dependencies.
type ActionControllerOptions struct {
	DataDir             string
	Deletions           *deletion.Manager
	ManifestSaver       DeletionManifestSaver
	AttachmentReader    AttachmentReader
	AttachmentOutputDir string
	OpenTarget          func(context.Context, string) error
}

// NewActionController creates a new action controller.
// If deletions is nil, the manager will be lazily initialized on first use.
func NewActionController(queries query.Engine, dataDir string, deletions *deletion.Manager) *ActionController {
	return NewActionControllerWithOptions(queries, ActionControllerOptions{
		DataDir:   dataDir,
		Deletions: deletions,
	})
}

// NewActionControllerWithOptions creates an action controller with explicit dependencies.
func NewActionControllerWithOptions(queries query.Engine, opts ActionControllerOptions) *ActionController {
	return &ActionController{
		queries:             queries,
		deletions:           opts.Deletions,
		dataDir:             opts.DataDir,
		manifestSaver:       opts.ManifestSaver,
		attachmentReader:    opts.AttachmentReader,
		attachmentOutputDir: opts.AttachmentOutputDir,
		openTarget:          opts.OpenTarget,
	}
}

// SaveManifest initializes the deletion manager if needed and saves the manifest.
func (c *ActionController) SaveManifest(manifest *deletion.Manifest) error {
	if c.manifestSaver != nil {
		return c.manifestSaver.SaveManifest(manifest)
	}
	if c.deletions == nil {
		deletionsDir := filepath.Join(c.dataDir, "deletions")
		mgr, err := deletion.NewManager(deletionsDir)
		if err != nil {
			return err
		}
		c.deletions = mgr
	}
	return c.deletions.SaveManifest(manifest)
}

// StageForDeletion prepares messages for deletion based on selection.
func (c *ActionController) StageForDeletion(ctx DeletionContext) (*deletion.Manifest, error) {
	return c.StageForDeletionContext(context.Background(), ctx)
}

// StageForDeletionContext prepares messages for deletion and lets callers
// cancel long-running all-match resolution.
func (c *ActionController) StageForDeletionContext(
	ctx context.Context,
	dctx DeletionContext,
) (*deletion.Manifest, error) {
	dctx = normalizeEmptyDeletionSearch(dctx)
	targets, err := c.resolveDeletionTargets(ctx, dctx)
	if err != nil {
		return nil, err
	}

	if len(targets) == 0 {
		if dctx.AllMatches {
			return nil, errors.New("no deletable messages match the current filter or search")
		}
		return nil, errors.New("no messages selected")
	}
	source, err := deletion.SourceReferenceForTargets(targets)
	if errors.Is(err, deletion.ErrMultipleDeletionSources) {
		return nil, errors.New("selected messages span multiple sources; press 'a' to filter by account, then stage again")
	}
	if err != nil {
		return nil, err
	}
	gmailIDs := deletion.SourceMessageIDs(targets)

	description := c.buildManifestDescription(dctx)
	manifest := deletion.NewManifestForSource(description, gmailIDs, source)
	manifest.CreatedBy = "tui"

	c.applyManifestFilters(manifest, dctx)
	if dctx.AllMatches {
		searchMode := ""
		if dctx.SearchQuery != "" {
			if dctx.AggregateMatchKey != nil {
				searchMode = string(query.DeletionSearchAggregate)
			} else {
				searchMode = deletionSearchModeName(dctx.SearchMode)
			}
		}
		rawFilter, marshalErr := json.Marshal(allMatchesManifestProvenance{
			Scope:       "all_matches",
			SearchQuery: dctx.SearchQuery,
			SearchMode:  searchMode,
			MatchFilter: manifestMatchFilter(dctx.MatchFilter),
		})
		if marshalErr != nil {
			return nil, fmt.Errorf("record deletion match provenance: %w", marshalErr)
		}
		manifest.RawFilter = rawFilter
	}

	return manifest, nil
}

func normalizeEmptyDeletionSearch(dctx DeletionContext) DeletionContext {
	if !dctx.AllMatches {
		return dctx
	}
	queryString := strings.TrimSpace(dctx.SearchQuery)
	if queryString == "" {
		dctx.SearchQuery = ""
		return dctx
	}
	parsed := search.Parse(queryString)
	if parsed.Err() == nil && search.Format(parsed) == "" {
		dctx.SearchQuery = ""
	}
	return dctx
}

func manifestMatchFilter(filter query.MessageFilter) allMatchesManifestFilter {
	emptyTargets := make([]string, 0, len(filter.EmptyValueTargets))
	for target, matchesEmpty := range filter.EmptyValueTargets {
		if matchesEmpty {
			emptyTargets = append(emptyTargets, target.String())
		}
	}
	sort.Strings(emptyTargets)
	return allMatchesManifestFilter{
		Sender: filter.Sender, SenderName: filter.SenderName,
		Recipient: filter.Recipient, RecipientName: filter.RecipientName,
		Domain: filter.Domain, Label: filter.Label, ListID: filter.ListID, MessageType: filter.MessageType,
		ConversationID: filter.ConversationID, EmptyValueTargets: emptyTargets,
		TimePeriod: filter.TimeRange.Period, TimeGranularity: filter.TimeRange.Granularity.String(),
		SourceID: filter.SourceID, SourceIDs: append([]int64(nil), filter.SourceIDs...),
		After: filter.After, Before: filter.Before,
		WithAttachmentsOnly:   filter.WithAttachmentsOnly,
		HideDeletedFromSource: filter.HideDeletedFromSource,
	}
}

// resolveGmailIDs converts selections (aggregate keys and message IDs) into Gmail IDs.
func (c *ActionController) resolveDeletionTargets(ctx context.Context, dctx DeletionContext) ([]query.DeletionTarget, error) {
	if dctx.SourceIDs != nil && len(dctx.SourceIDs) == 0 {
		return []query.DeletionTarget{}, nil
	}
	if dctx.AllMatches {
		return c.resolveAllMatchingDeletionTargets(ctx, dctx)
	}
	targetsByMessageID := make(map[int64]query.DeletionTarget)

	// From selected aggregates - resolve to Gmail IDs via query engine
	if len(dctx.AggregateSelection) > 0 {
		for key := range dctx.AggregateSelection {
			filter := c.buildFilterForAggregate(key, dctx)

			var targets []query.DeletionTarget
			var err error
			if dctx.SearchQuery != "" {
				resolver, ok := c.queries.(query.DeletionTargetAggregateSearchResolver)
				if !ok {
					return nil, errors.New("this query engine cannot resolve the displayed aggregate search safely")
				}
				targets, err = resolver.GetDeletionTargetsByAggregateSearch(
					ctx, dctx.SearchQuery, filter, dctx.AggregateViewType, key,
				)
			} else {
				targets, err = c.queries.GetDeletionTargetsByFilter(ctx, filter)
			}
			if err != nil {
				return nil, fmt.Errorf("error loading messages: %w", err)
			}
			for _, target := range targets {
				targetsByMessageID[target.MessageID] = target
			}
		}
	}

	// From selected message IDs
	if len(dctx.MessageSelection) > 0 {
		accounts := make(map[int64]query.AccountInfo, len(dctx.Accounts))
		for _, account := range dctx.Accounts {
			accounts[account.ID] = account
		}
		for _, msg := range dctx.Messages {
			if dctx.MessageSelection[msg.ID] {
				account, ok := accounts[msg.SourceID]
				if !ok {
					return nil, fmt.Errorf("selected message %d has no source metadata", msg.ID)
				}
				targetsByMessageID[msg.ID] = query.DeletionTarget{
					MessageID: msg.ID, SourceID: msg.SourceID, SourceType: account.SourceType,
					SourceIdentifier: account.Identifier, SourceMessageID: msg.SourceMessageID,
				}
			}
		}
	}

	targets := make([]query.DeletionTarget, 0, len(targetsByMessageID))
	for _, target := range targetsByMessageID {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].MessageID < targets[j].MessageID })
	return targets, nil
}

func (c *ActionController) resolveAllMatchingDeletionTargets(
	ctx context.Context,
	dctx DeletionContext,
) ([]query.DeletionTarget, error) {
	filter := emailScopedMessageFilter(dctx.MatchFilter)
	filter.Pagination = query.Pagination{}
	if dctx.SearchQuery == "" {
		targets, err := c.queries.GetDeletionTargetsByFilter(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("error loading messages: %w", err)
		}
		return targets, nil
	}
	if dctx.AggregateMatchKey != nil {
		resolver, ok := c.queries.(query.DeletionTargetAggregateSearchResolver)
		if !ok {
			return nil, errors.New("this query engine cannot resolve the displayed aggregate search safely")
		}
		targets, err := resolver.GetDeletionTargetsByAggregateSearch(
			ctx, dctx.SearchQuery, filter, dctx.AggregateViewType, *dctx.AggregateMatchKey,
		)
		if err != nil {
			return nil, fmt.Errorf("error loading messages: %w", err)
		}
		return targets, nil
	}
	if dctx.SearchMode == searchModeSemantic {
		return nil, errors.New("semantic search is ranked and bounded; switch to fast or deep search to stage every match")
	}
	parsed := search.Parse(dctx.SearchQuery)
	if err := parsed.Err(); err != nil {
		return nil, fmt.Errorf("invalid search query: %w", err)
	}
	resolver, ok := c.queries.(query.DeletionTargetSearchResolver)
	if !ok {
		return nil, errors.New("this query engine cannot resolve every search match safely")
	}
	mode := query.DeletionSearchFast
	if dctx.SearchMode == searchModeDeep {
		mode = query.DeletionSearchDeep
	}
	targets, err := resolver.GetDeletionTargetsBySearch(ctx, parsed, filter, mode)
	if err != nil {
		return nil, fmt.Errorf("error loading messages: %w", err)
	}
	return targets, nil
}

func deletionSearchModeName(mode searchModeKind) string {
	switch mode {
	case searchModeDeep:
		return string(query.DeletionSearchDeep)
	case searchModeSemantic:
		return "semantic"
	default:
		return string(query.DeletionSearchFast)
	}
}

// buildFilterForAggregate constructs a MessageFilter for a single aggregate key.
func (c *ActionController) buildFilterForAggregate(key string, dctx DeletionContext) query.MessageFilter {
	var filter query.MessageFilter
	if dctx.AllMatches || dctx.SearchQuery != "" {
		// Search-aware aggregate staging starts with the same complete scope
		// used to render the aggregate, then adds the selected row below.
		filter = dctx.MatchFilter.Clone()
	} else if dctx.DrillFilter != nil {
		// Selected aggregate staging preserves its parent drill-down context.
		filter = dctx.DrillFilter.Clone()
	}
	filter.WithAttachmentsOnly = filter.WithAttachmentsOnly || dctx.MatchFilter.WithAttachmentsOnly
	filter.HideDeletedFromSource = filter.HideDeletedFromSource || dctx.MatchFilter.HideDeletedFromSource
	filter.SourceID = nil
	filter.SourceIDs = nil
	if dctx.SourceIDs != nil {
		filter.SourceIDs = append(make([]int64, 0, len(dctx.SourceIDs)), dctx.SourceIDs...)
	} else if dctx.AccountFilter != nil {
		filter.SourceID = dctx.AccountFilter
	}
	// TUI deletion is an email/Gmail workflow. Keep aggregate resolution
	// inside Email mode even though the shared messages dataset also contains
	// meetings and text records.
	filter.MessageType = emailMessageType

	switch dctx.AggregateViewType {
	case query.ViewSenders:
		filter.Sender = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewSenders)
		}
	case query.ViewSenderNames:
		filter.SenderName = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewSenderNames)
		}
	case query.ViewRecipients:
		filter.Recipient = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewRecipients)
		}
	case query.ViewRecipientNames:
		filter.RecipientName = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewRecipientNames)
		}
	case query.ViewDomains:
		filter.Domain = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewDomains)
		}
	case query.ViewLabels:
		filter.Label = key
		if key == "" {
			filter.SetEmptyTarget(query.ViewLabels)
		}
	case query.ViewLists:
		filter.ListID = key
	case query.ViewTime:
		filter.TimeRange.Period = key
		filter.TimeRange.Granularity = dctx.TimeGranularity
	default:
		// Count is a sentinel, not a drill-down target.
	}
	return filter
}

// buildManifestDescription generates a human-readable description for the manifest.
func (c *ActionController) buildManifestDescription(ctx DeletionContext) string {
	var description string
	if ctx.AllMatches && ctx.AggregateMatchKey != nil {
		description = fmt.Sprintf("%s-%s", ctx.AggregateViewType.String(), *ctx.AggregateMatchKey)
	} else if ctx.AllMatches && ctx.SearchQuery != "" {
		description = "search-all-matches"
	} else if ctx.AllMatches {
		description = "all-matches"
	} else if len(ctx.AggregateSelection) == 1 {
		for key := range ctx.AggregateSelection {
			description = fmt.Sprintf("%s-%s", ctx.AggregateViewType.String(), key)
			break
		}
	} else if len(ctx.AggregateSelection) > 1 {
		description = fmt.Sprintf("%s-multiple(%d)", ctx.AggregateViewType.String(), len(ctx.AggregateSelection))
	} else if len(ctx.MessageSelection) > 0 {
		description = fmt.Sprintf("messages-multiple(%d)", len(ctx.MessageSelection))
	} else {
		description = "selection"
	}

	// Store the full description; truncation is a display-only concern applied
	// by the table views, so JSON and detail output carry the complete value.
	return description
}

// applyManifestFilters populates the manifest's filter metadata from the context.
func (c *ActionController) applyManifestFilters(m *deletion.Manifest, ctx DeletionContext) {
	// Set account filter
	if ctx.AccountFilter != nil {
		for _, acc := range ctx.Accounts {
			if acc.ID == *ctx.AccountFilter {
				m.Filters.Account = acc.Identifier
				break
			}
		}
	} else if len(ctx.Accounts) == 1 {
		m.Filters.Account = ctx.Accounts[0].Identifier
	} else if len(ctx.SourceIDs) == 1 {
		for _, acc := range ctx.Accounts {
			if acc.ID == ctx.SourceIDs[0] {
				m.Filters.Account = acc.Identifier
				break
			}
		}
	}

	if ctx.AllMatches {
		applyMessageFilterToManifest(m, ctx.MatchFilter)
		return
	}

	// Set context filters from all selected aggregates
	if len(ctx.AggregateSelection) > 0 {
		keys := make([]string, 0, len(ctx.AggregateSelection))
		for key := range ctx.AggregateSelection {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		switch ctx.AggregateViewType {
		case query.ViewSenders:
			m.Filters.Senders = keys
		case query.ViewRecipients:
			m.Filters.Recipients = keys
		case query.ViewDomains:
			m.Filters.SenderDomains = keys
		case query.ViewLabels:
			m.Filters.Labels = keys
		case query.ViewLists:
			m.Filters.ListIDs = keys
		default:
			// SenderNames / RecipientNames / Time / Count don't map to manifest filters.
		}
	}
}

func applyMessageFilterToManifest(m *deletion.Manifest, filter query.MessageFilter) {
	if filter.Sender != "" {
		m.Filters.Senders = []string{filter.Sender}
	}
	if filter.Recipient != "" {
		m.Filters.Recipients = []string{filter.Recipient}
	}
	if filter.Domain != "" {
		m.Filters.SenderDomains = []string{filter.Domain}
	}
	if filter.Label != "" {
		m.Filters.Labels = []string{filter.Label}
	}
	if filter.ListID != "" {
		m.Filters.ListIDs = []string{filter.ListID}
	}
	after, before := filter.After, filter.Before
	if filter.TimeRange.Period != "" {
		if periodAfter, periodBefore, ok := query.ParseTimePeriodBounds(filter.TimeRange.Period); ok {
			after, before = &periodAfter, &periodBefore
		}
	}
	if after != nil {
		m.Filters.After = after.Format(time.RFC3339)
	}
	if before != nil {
		m.Filters.Before = before.Format(time.RFC3339)
	}
}

// ExportAttachments performs the export logic.
func (c *ActionController) ExportAttachments(detail *query.MessageDetail, selection map[int]bool) tea.Cmd {
	if detail == nil || len(detail.Attachments) == 0 {
		return nil
	}

	var selectedAttachments []query.AttachmentInfo
	for i, att := range detail.Attachments {
		if selection[i] {
			selectedAttachments = append(selectedAttachments, att)
		}
	}

	if len(selectedAttachments) == 0 {
		return nil
	}

	attachmentsDir := filepath.Join(c.dataDir, "attachments")
	subject := detail.Subject
	if subject == "" {
		subject = "attachments"
	}
	subject = export.SanitizeFilename(subject)
	if len(subject) > 50 {
		subject = subject[:50]
	}
	zipFilename := fmt.Sprintf("%s_%d.zip", subject, detail.ID)

	return func() tea.Msg {
		outputDir, err := c.attachmentOutputDirectory()
		if err != nil {
			return ExportResultMsg{Title: "Export Failed", Err: err}
		}
		stats := c.exportSelectedAttachments(filepath.Join(outputDir, zipFilename), attachmentsDir, selectedAttachments)
		msg := ExportResultMsg{Title: "Export Complete", Result: export.FormatExportResult(stats)}
		// Only set Err for true failures: write errors or zero exported files.
		// Partial success (some files exported, some errors) should show the
		// detailed Result which includes both the success info and error list.
		if stats.WriteError || stats.Count == 0 {
			msg.Err = errors.New("export failed")
			msg.Title = "Export Failed"
		}
		return msg
	}
}

// DownloadAttachment writes one exact attachment into the local output
// directory without overwriting an existing file.
func (c *ActionController) DownloadAttachment(att query.AttachmentInfo) tea.Cmd {
	return func() tea.Msg {
		exported, err := c.exportOneAttachment(att)
		if err != nil {
			msg := ExportResultMsg{Title: "Download Failed", Err: err}
			if exported.Path != "" {
				msg.Result = fmt.Sprintf("%v\n\nThe attachment was downloaded to:\n%s", err, exported.Path)
			}
			return msg
		}
		return ExportResultMsg{
			Title: "Download Complete",
			Result: fmt.Sprintf("Downloaded %s (%s)\n\nSaved to:\n%s",
				filepath.Base(exported.Path), export.FormatBytesLong(exported.Size), exported.Path),
		}
	}
}

// OpenAttachment opens a URL-backed attachment directly, or first downloads a
// content-backed attachment locally and then hands it to the OS default app.
func (c *ActionController) OpenAttachment(att query.AttachmentInfo) tea.Cmd {
	return func() tea.Msg {
		target := att.URL
		var savedPath string
		if target != "" {
			parsed, err := url.Parse(target)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
				return ExportResultMsg{Title: "Open Failed", Err: errors.New("attachment URL must use http or https")}
			}
		} else {
			if err := validateAttachmentOpen(att); err != nil {
				return ExportResultMsg{Title: "Open Blocked", Err: err}
			}
			exported, err := c.exportOneAttachment(att)
			if err != nil {
				msg := ExportResultMsg{Title: "Open Failed", Err: err}
				if exported.Path != "" {
					msg.Result = fmt.Sprintf("%v\n\nThe attachment was downloaded to:\n%s", err, exported.Path)
				}
				return msg
			}
			target = exported.Path
			savedPath = exported.Path
		}

		opener := c.openTarget
		if opener == nil {
			opener = openSystemTarget
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := opener(ctx, target); err != nil {
			openErr := fmt.Errorf("open attachment: %w", err)
			if savedPath != "" {
				return ExportResultMsg{
					Title:  "Open Failed",
					Result: fmt.Sprintf("%v\n\nThe attachment was downloaded to:\n%s", openErr, savedPath),
					Err:    openErr,
				}
			}
			return ExportResultMsg{Title: "Open Failed", Err: openErr}
		}
		result := "Opened " + attachmentDisplayName(att)
		if savedPath != "" {
			result += "\n\nSaved to:\n" + savedPath
		}
		return ExportResultMsg{Title: "Attachment Opened", Result: result}
	}
}

func (c *ActionController) exportOneAttachment(att query.AttachmentInfo) (export.ExportedFile, error) {
	if att.URL != "" {
		return export.ExportedFile{}, errors.New("URL-backed attachments can be opened but not downloaded")
	}
	outputDir, err := c.attachmentOutputDirectory()
	if err != nil {
		return export.ExportedFile{}, err
	}
	result := export.AttachmentsToDirWithOpener(outputDir, []query.AttachmentInfo{att}, c.attachmentOpener())
	if len(result.Files) == 1 {
		exported := result.Files[0]
		marker := c.markUntrusted
		if marker == nil {
			marker = markAttachmentUntrusted
		}
		if err := marker(exported.Path); err != nil {
			return exported, fmt.Errorf("mark downloaded attachment as untrusted: %w", err)
		}
		return exported, nil
	}
	if len(result.Errors) > 0 {
		return export.ExportedFile{}, errors.New(result.Errors[0])
	}
	return export.ExportedFile{}, errors.New("attachment was not downloaded")
}

func (c *ActionController) attachmentOutputDirectory() (string, error) {
	outputDir := c.attachmentOutputDir
	if outputDir == "" {
		baseDir := c.dataDir
		if baseDir == "" {
			var err error
			baseDir, err = os.UserCacheDir()
			if err != nil {
				return "", fmt.Errorf("get user cache directory: %w", err)
			}
			baseDir = filepath.Join(baseDir, "msgvault")
		}
		outputDir = filepath.Join(baseDir, "exports")
	}
	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	if err := fileutil.SecureMkdirAll(absOutputDir, 0o700); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	return absOutputDir, nil
}

func (c *ActionController) attachmentOpener() export.AttachmentOpener {
	if c.attachmentReader != nil {
		return func(contentHash string) (io.ReadCloser, error) {
			return c.attachmentReader.OpenAttachment(context.Background(), contentHash)
		}
	}
	attachmentsDir := filepath.Join(c.dataDir, "attachments")
	return func(contentHash string) (io.ReadCloser, error) {
		path, err := export.StoragePath(attachmentsDir, contentHash)
		if err != nil {
			return nil, err
		}
		return os.Open(path)
	}
}

func attachmentDisplayName(att query.AttachmentInfo) string {
	name := filepath.Base(att.Filename)
	if name == "" || name == "." {
		if att.URL != "" {
			name = att.URL
		} else {
			name = att.ContentHash
		}
	}
	return textutil.SanitizeTerminal(name)
}

func (c *ActionController) exportSelectedAttachments(
	zipFilename string,
	attachmentsDir string,
	attachments []query.AttachmentInfo,
) export.ExportStats {
	if c.attachmentReader == nil {
		return export.Attachments(zipFilename, attachmentsDir, attachments)
	}
	return export.AttachmentsWithOpener(zipFilename, attachments, func(contentHash string) (io.ReadCloser, error) {
		return c.attachmentReader.OpenAttachment(context.Background(), contentHash)
	})
}
