package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const messageExportSchema = "msgvault-message-export/1"

type exportMessagesDeps struct {
	openStore func() (*store.Store, func(), error)
}

type exportMessagesOptions struct {
	Start        string
	End          string
	Format       string
	MessageTypes []string
	Sources      []string
}

type exportMessagesSourceSelector struct {
	SourceType string `json:"source_type"`
	Identifier string `json:"identifier"`
}

type exportMessagesWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type exportMessagesFilters struct {
	MessageTypes []string                       `json:"message_types"`
	Sources      []exportMessagesSourceSelector `json:"sources"`
}

type exportMessagesManifest struct {
	RecordType      string                `json:"record_type"`
	Schema          string                `json:"schema"`
	MsgvaultVersion string                `json:"msgvault_version"`
	Window          exportMessagesWindow  `json:"window"`
	Filters         exportMessagesFilters `json:"filters"`
}

type exportMessagesSourceRecord struct {
	RecordType           string     `json:"record_type"`
	SourceType           string     `json:"source_type"`
	Identifier           string     `json:"identifier"`
	DisplayName          string     `json:"display_name"`
	LastSuccessfulSyncAt *time.Time `json:"last_successful_sync_at"`
}

type exportMessagesConversationRecord struct {
	RecordType       string                              `json:"record_type"`
	SourceType       string                              `json:"source_type"`
	SourceIdentifier string                              `json:"source_identifier"`
	ID               string                              `json:"id"`
	Title            string                              `json:"title"`
	ConversationType store.MessageExportConversationType `json:"conversation_type"`
	ParentID         *string                             `json:"parent_id"`
}

type exportMessagesAuthorRecord struct {
	DisplayName string `json:"display_name"`
	Address     string `json:"address"`
}

type exportMessagesMessageRecord struct {
	RecordType        string                      `json:"record_type"`
	SourceType        string                      `json:"source_type"`
	SourceIdentifier  string                      `json:"source_identifier"`
	ID                string                      `json:"id"`
	ConversationID    string                      `json:"conversation_id"`
	MessageType       string                      `json:"message_type"`
	Subject           string                      `json:"subject"`
	Text              string                      `json:"text"`
	Author            *exportMessagesAuthorRecord `json:"author"`
	OccurredAt        time.Time                   `json:"occurred_at"`
	DeletedFromSource bool                        `json:"deleted_from_source"`
}

type exportMessagesComplete struct {
	RecordType string               `json:"record_type"`
	Counts     exportMessagesCounts `json:"counts"`
}

type exportMessagesCounts struct {
	Sources       int `json:"sources"`
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
}

type exportMessagesJSONLSink struct {
	encoder *json.Encoder
}

func (s exportMessagesJSONLSink) Source(source store.MessageExportSource) error {
	return s.encoder.Encode(exportMessagesSourceRecord{
		RecordType:           "source",
		SourceType:           source.SourceType,
		Identifier:           source.Identifier,
		DisplayName:          source.DisplayName,
		LastSuccessfulSyncAt: source.LastSuccessfulSyncAt,
	})
}

func (s exportMessagesJSONLSink) Conversation(
	conversation store.MessageExportConversation,
) error {
	return s.encoder.Encode(exportMessagesConversationRecord{
		RecordType:       "conversation",
		SourceType:       conversation.SourceType,
		SourceIdentifier: conversation.SourceIdentifier,
		ID:               conversation.ID,
		Title:            conversation.Title,
		ConversationType: conversation.ConversationType,
		ParentID:         conversation.ParentID,
	})
}

func (s exportMessagesJSONLSink) Message(message store.MessageExportMessage) error {
	var author *exportMessagesAuthorRecord
	if message.Author != nil {
		author = &exportMessagesAuthorRecord{
			DisplayName: message.Author.DisplayName,
			Address:     message.Author.Address,
		}
	}
	return s.encoder.Encode(exportMessagesMessageRecord{
		RecordType:        "message",
		SourceType:        message.SourceType,
		SourceIdentifier:  message.SourceIdentifier,
		ID:                message.ID,
		ConversationID:    message.ConversationID,
		MessageType:       message.MessageType,
		Subject:           message.Subject,
		Text:              message.Text,
		Author:            author,
		OccurredAt:        message.OccurredAt,
		DeletedFromSource: message.DeletedFromSource,
	})
}

func defaultExportMessagesDeps() exportMessagesDeps {
	return exportMessagesDeps{openStore: openWritableStoreAndInitForIngest}
}

func newExportMessagesCmd(deps exportMessagesDeps) *cobra.Command {
	cmd := newExportMessagesLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newExportMessagesLocalCmd(deps exportMessagesDeps) *cobra.Command {
	opts := exportMessagesOptions{Format: "jsonl"}
	cmd := &cobra.Command{
		Use:          "export-messages",
		Short:        "Export a bounded message window as provider-neutral JSONL",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExportMessages(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Start, "start", "", "inclusive RFC3339 lower bound")
	cmd.Flags().StringVar(&opts.End, "end", "", "exclusive RFC3339 upper bound")
	cmd.Flags().StringArrayVar(
		&opts.MessageTypes, "message-type", nil, "exact message type to include (repeatable)",
	)
	cmd.Flags().StringArrayVar(
		&opts.Sources, "source", nil, "typed source selector type:identifier (repeatable)",
	)
	cmd.Flags().StringVar(&opts.Format, "format", "jsonl", "output format (jsonl)")
	return cmd
}

func runExportMessages(
	cmd *cobra.Command,
	deps exportMessagesDeps,
	opts exportMessagesOptions,
) error {
	start, end, err := parseMessageExportBounds(opts.Start, opts.End)
	if err != nil {
		return err
	}
	if opts.Format != "jsonl" {
		return fmt.Errorf("unsupported --format %q (expected jsonl)", opts.Format)
	}
	messageTypes, err := normalizeMessageExportTypes(opts.MessageTypes)
	if err != nil {
		return err
	}
	selectors, err := normalizeMessageExportSelectors(opts.Sources)
	if err != nil {
		return err
	}
	if deps.openStore == nil {
		return errors.New("open message archive is not configured")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()

	sourceIDs, err := resolveMessageExportSources(st, selectors)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(exportMessagesManifest{
		RecordType:      "manifest",
		Schema:          messageExportSchema,
		MsgvaultVersion: Version,
		Window:          exportMessagesWindow{Start: start, End: end},
		Filters: exportMessagesFilters{
			MessageTypes: messageTypes,
			Sources:      selectors,
		},
	}); err != nil {
		return fmt.Errorf("encode message export manifest: %w", err)
	}

	counts, err := st.ExportMessages(cmd.Context(), store.MessageExportFilter{
		Start:        start,
		End:          end,
		SourceIDs:    sourceIDs,
		MessageTypes: messageTypes,
	}, exportMessagesJSONLSink{encoder: encoder})
	if err != nil {
		return fmt.Errorf("export messages: %w", err)
	}
	if err := encoder.Encode(exportMessagesComplete{
		RecordType: "complete",
		Counts: exportMessagesCounts{
			Sources:       counts.Sources,
			Conversations: counts.Conversations,
			Messages:      counts.Messages,
		},
	}); err != nil {
		return fmt.Errorf("encode message export completion: %w", err)
	}
	return nil
}

func parseMessageExportBounds(startValue, endValue string) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339, startValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"invalid --start %q (expected RFC3339): %w", startValue, err,
		)
	}
	end, err := time.Parse(time.RFC3339, endValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"invalid --end %q (expected RFC3339): %w", endValue, err,
		)
	}
	start = start.UTC()
	end = end.UTC()
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("--start must be before --end")
	}
	return start, end, nil
}

func normalizeMessageExportTypes(values []string) ([]string, error) {
	normalized := append([]string(nil), values...)
	for _, value := range normalized {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("--message-type must be non-empty")
		}
	}
	sort.Strings(normalized)
	return compactSortedStrings(normalized), nil
}

func normalizeMessageExportSelectors(
	values []string,
) ([]exportMessagesSourceSelector, error) {
	selectors := make([]exportMessagesSourceSelector, 0, len(values))
	for _, value := range values {
		sourceType, identifier, ok := strings.Cut(value, ":")
		if !ok || strings.TrimSpace(sourceType) == "" || strings.TrimSpace(identifier) == "" {
			return nil, fmt.Errorf(
				"invalid --source %q (expected non-empty type:identifier)", value,
			)
		}
		selectors = append(selectors, exportMessagesSourceSelector{
			SourceType: sourceType,
			Identifier: identifier,
		})
	}
	sort.Slice(selectors, func(i, j int) bool {
		if selectors[i].SourceType != selectors[j].SourceType {
			return selectors[i].SourceType < selectors[j].SourceType
		}
		return selectors[i].Identifier < selectors[j].Identifier
	})
	return compactMessageExportSelectors(selectors), nil
}

func resolveMessageExportSources(
	st *store.Store,
	selectors []exportMessagesSourceSelector,
) ([]int64, error) {
	sourceIDs := make([]int64, 0, len(selectors))
	for _, selector := range selectors {
		sources, err := st.ListSources(selector.SourceType)
		if err != nil {
			return nil, fmt.Errorf(
				"list sources of type %q: %w", selector.SourceType, err,
			)
		}
		var matched *store.Source
		for _, source := range sources {
			if source.Identifier == selector.Identifier {
				matched = source
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf(
				"source %s:%s not found", selector.SourceType, selector.Identifier,
			)
		}
		sourceIDs = append(sourceIDs, matched.ID)
	}
	return sourceIDs, nil
}

func compactSortedStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	compacted := values[:1]
	for _, value := range values[1:] {
		if value != compacted[len(compacted)-1] {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func compactMessageExportSelectors(
	selectors []exportMessagesSourceSelector,
) []exportMessagesSourceSelector {
	if len(selectors) == 0 {
		return []exportMessagesSourceSelector{}
	}
	compacted := selectors[:1]
	for _, selector := range selectors[1:] {
		last := compacted[len(compacted)-1]
		if selector != last {
			compacted = append(compacted, selector)
		}
	}
	return compacted
}

func init() {
	rootCmd.AddCommand(newExportMessagesCmd(defaultExportMessagesDeps()))
}
