package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/query"
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
	Accounts           []query.AccountInfo
	TimeGranularity    query.TimeGranularity
	Messages           []query.MessageSummary
	DrillFilter        *query.MessageFilter
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
	targets, err := c.resolveDeletionTargets(ctx)
	if err != nil {
		return nil, err
	}

	if len(targets) == 0 {
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

	description := c.buildManifestDescription(ctx)
	manifest := deletion.NewManifestForSource(description, gmailIDs, source)
	manifest.CreatedBy = "tui"

	c.applyManifestFilters(manifest, ctx)

	return manifest, nil
}

// resolveGmailIDs converts selections (aggregate keys and message IDs) into Gmail IDs.
func (c *ActionController) resolveDeletionTargets(dctx DeletionContext) ([]query.DeletionTarget, error) {
	targetsByMessageID := make(map[int64]query.DeletionTarget)
	ctx := context.Background()

	// From selected aggregates - resolve to Gmail IDs via query engine
	if len(dctx.AggregateSelection) > 0 {
		for key := range dctx.AggregateSelection {
			filter := c.buildFilterForAggregate(key, dctx)

			targets, err := c.queries.GetDeletionTargetsByFilter(ctx, filter)
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

// buildFilterForAggregate constructs a MessageFilter for a single aggregate key.
func (c *ActionController) buildFilterForAggregate(key string, dctx DeletionContext) query.MessageFilter {
	// Start with drill-down filter as base (preserves parent context)
	// Use Clone() to deep-copy the filter, preventing shared map mutation.
	var filter query.MessageFilter
	if dctx.DrillFilter != nil {
		filter = dctx.DrillFilter.Clone()
	}
	if dctx.AccountFilter != nil {
		filter.SourceID = dctx.AccountFilter
	}
	// TUI deletion is an email/Gmail workflow. Keep aggregate resolution
	// inside Email mode even though the shared messages dataset also contains
	// meetings and text records.
	filter.MessageType = emailMessageType

	switch dctx.AggregateViewType {
	case query.ViewSenders:
		filter.Sender = key
	case query.ViewRecipients:
		filter.Recipient = key
	case query.ViewDomains:
		filter.Domain = key
	case query.ViewLabels:
		filter.Label = key
	case query.ViewLists:
		filter.ListID = key
	case query.ViewTime:
		filter.TimeRange.Period = key
		filter.TimeRange.Granularity = dctx.TimeGranularity
	default:
		// SenderNames / RecipientNames / Count are not drill-down targets here.
	}
	return filter
}

// buildManifestDescription generates a human-readable description for the manifest.
func (c *ActionController) buildManifestDescription(ctx DeletionContext) string {
	var description string
	if len(ctx.AggregateSelection) == 1 {
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
		stats := c.exportSelectedAttachments(zipFilename, attachmentsDir, selectedAttachments)
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
	outputDir := c.attachmentOutputDir
	if outputDir == "" {
		baseDir := c.dataDir
		if baseDir == "" {
			var err error
			baseDir, err = os.UserCacheDir()
			if err != nil {
				return export.ExportedFile{}, fmt.Errorf("get user cache directory: %w", err)
			}
			baseDir = filepath.Join(baseDir, "msgvault")
		}
		outputDir = filepath.Join(baseDir, "downloads")
	}
	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return export.ExportedFile{}, fmt.Errorf("resolve output directory: %w", err)
	}
	if err := fileutil.SecureMkdirAll(absOutputDir, 0o700); err != nil {
		return export.ExportedFile{}, fmt.Errorf("create output directory: %w", err)
	}
	result := export.AttachmentsToDirWithOpener(absOutputDir, []query.AttachmentInfo{att}, c.attachmentOpener())
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
