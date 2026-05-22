package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
)

// ExportResultMsg is returned when attachment export completes.
type ExportResultMsg struct {
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
	queries   query.Engine
	deletions *deletion.Manager
	dataDir   string
}

// NewActionController creates a new action controller.
// If deletions is nil, the manager will be lazily initialized on first use.
func NewActionController(queries query.Engine, dataDir string, deletions *deletion.Manager) *ActionController {
	return &ActionController{
		queries:   queries,
		deletions: deletions,
		dataDir:   dataDir,
	}
}

// SaveManifest initializes the deletion manager if needed and saves the manifest.
func (c *ActionController) SaveManifest(manifest *deletion.Manifest) error {
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
	gmailIDs, err := c.resolveGmailIDs(ctx)
	if err != nil {
		return nil, err
	}

	if len(gmailIDs) == 0 {
		return nil, fmt.Errorf("no messages selected")
	}

	description := c.buildManifestDescription(ctx)
	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "tui"

	c.applyManifestFilters(manifest, ctx)

	return manifest, nil
}

// resolveGmailIDs converts selections (aggregate keys and message IDs) into Gmail IDs.
func (c *ActionController) resolveGmailIDs(dctx DeletionContext) ([]string, error) {
	gmailIDSet := make(map[string]bool)
	ctx := context.Background()

	// From selected aggregates - resolve to Gmail IDs via query engine
	if len(dctx.AggregateSelection) > 0 {
		for key := range dctx.AggregateSelection {
			filter := c.buildFilterForAggregate(key, dctx)

			ids, err := c.queries.GetGmailIDsByFilter(ctx, filter)
			if err != nil {
				return nil, fmt.Errorf("error loading messages: %v", err)
			}
			for _, id := range ids {
				gmailIDSet[id] = true
			}
		}
	}

	// From selected message IDs
	if len(dctx.MessageSelection) > 0 {
		for _, msg := range dctx.Messages {
			if dctx.MessageSelection[msg.ID] {
				gmailIDSet[msg.SourceMessageID] = true
			}
		}
	}

	gmailIDs := make([]string, 0, len(gmailIDSet))
	for id := range gmailIDSet {
		gmailIDs = append(gmailIDs, id)
	}
	return gmailIDs, nil
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

	switch dctx.AggregateViewType {
	case query.ViewSenders:
		filter.Sender = key
	case query.ViewRecipients:
		filter.Recipient = key
	case query.ViewDomains:
		filter.Domain = key
	case query.ViewLabels:
		filter.Label = key
	case query.ViewTime:
		filter.TimeRange.Period = key
		filter.TimeRange.Granularity = dctx.TimeGranularity
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

	if len(description) > 30 {
		description = description[:30]
	}
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
		stats := export.Attachments(zipFilename, attachmentsDir, selectedAttachments)
		msg := ExportResultMsg{Result: export.FormatExportResult(stats)}
		// Only set Err for true failures: write errors or zero exported files.
		// Partial success (some files exported, some errors) should show the
		// detailed Result which includes both the success info and error list.
		if stats.WriteError || stats.Count == 0 {
			msg.Err = fmt.Errorf("export failed")
		}
		return msg
	}
}
