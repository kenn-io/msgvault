package export

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	personresolver "go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

// MessageSchema names the provider-neutral JSONL message export contract.
const MessageSchema = "msgvault-message-export/1"

// MessageSourceSelector selects one archive source by type and identifier,
// as list-accounts --json reports them.
type MessageSourceSelector struct {
	SourceType string `json:"source_type"`
	Identifier string `json:"identifier"`
}

// MessagesRequest is a validated export window and its filters.
type MessagesRequest struct {
	Start        time.Time
	End          time.Time
	MessageTypes []string
	Sources      []MessageSourceSelector
	PersonID     *int64
	// Scope, when non-nil, limits the export to these source IDs, as a host
	// that authorizes sources supplies them. A selected source outside it is
	// not found, and an empty scope exports no records.
	Scope []int64
}

// ParseMessagesRequest validates export bounds and normalizes filters. The
// error text names the CLI flags because both the CLI and the API return it.
func ParseMessagesRequest(start, end string, messageTypes, sources []string, personID *int64) (MessagesRequest, error) {
	startTime, endTime, err := parseMessageBounds(start, end)
	if err != nil {
		return MessagesRequest{}, err
	}
	types, err := normalizeMessageTypes(messageTypes)
	if err != nil {
		return MessagesRequest{}, err
	}
	selectors, err := normalizeMessageSelectors(sources)
	if err != nil {
		return MessagesRequest{}, err
	}
	return MessagesRequest{Start: startTime, End: endTime, MessageTypes: types, Sources: selectors, PersonID: personID}, nil
}

// MessagesExport holds a read snapshot whose sources and person scope have
// been resolved, so preflight errors surface before any output is written.
type MessagesExport struct {
	ctx      context.Context
	release  func()
	store    *store.Store
	request  MessagesRequest
	filter   store.MessageExportFilter
	personID *int64
	empty    bool
}

// OpenMessages starts a read snapshot and resolves the request's sources and
// person scope. Close releases the snapshot.
func OpenMessages(ctx context.Context, st *store.Store, request MessagesRequest) (*MessagesExport, error) {
	ctx, release, err := st.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	sourceIDs, err := resolveMessageSources(ctx, st, request.Sources, request.Scope)
	if err != nil {
		release()
		return nil, err
	}
	empty := false
	if request.Scope != nil && len(request.Sources) == 0 {
		sourceIDs = slices.DeleteFunc(slices.Clone(request.Scope), func(id int64) bool { return id <= 0 })
		empty = len(sourceIDs) == 0
	}
	var scope *personscope.Scope
	var personID *int64
	if request.PersonID != nil && !empty {
		resolved, err := personresolver.Resolve(ctx, st, personresolver.Reference{Kind: personresolver.ReferencePerson, ID: *request.PersonID}, nil)
		switch {
		case err == nil:
			scope = &resolved.Scope
			personID = &resolved.PersonID
		case request.Scope != nil && (errors.Is(err, store.ErrPersonNotFound) || errors.Is(err, personresolver.ErrEmptyPopulation)):
			// People are archive-wide. A scoped caller gets the same empty
			// export for a missing person, or one with no messages anywhere,
			// as for one outside its sources.
			empty = true
		default:
			release()
			return nil, fmt.Errorf("resolve --person-id: %w", err)
		}
	}
	if request.Scope != nil {
		// A merged person resolves to another ID, which a scoped caller
		// must not learn.
		personID = request.PersonID
	}
	return &MessagesExport{
		ctx:     ctx,
		release: release,
		store:   st,
		request: request,
		filter: store.MessageExportFilter{
			PersonScope:  scope,
			Start:        request.Start,
			End:          request.End,
			SourceIDs:    sourceIDs,
			MessageTypes: request.MessageTypes,
		},
		personID: personID,
		empty:    empty,
	}, nil
}

// Close releases the read snapshot.
func (e *MessagesExport) Close() { e.release() }

// Write streams the manifest, source, conversation, and message records, then
// the completion record. A stream without the completion record is truncated.
func (e *MessagesExport) Write(w io.Writer, msgvaultVersion string) error {
	encoder := jsontext.NewEncoder(w, jsontext.EscapeForHTML(false))
	if err := json.MarshalEncode(encoder, messagesManifest{
		RecordType:      "manifest",
		Schema:          MessageSchema,
		MsgvaultVersion: msgvaultVersion,
		Window:          messagesWindow{Start: e.request.Start, End: e.request.End},
		Filters: messagesFilters{
			PersonID:     e.personID,
			MessageTypes: e.request.MessageTypes,
			Sources:      e.request.Sources,
		},
	}, json.Deterministic(true)); err != nil {
		return fmt.Errorf("encode message export manifest: %w", err)
	}
	var counts store.MessageExportCounts
	if !e.empty {
		var err error
		counts, err = e.store.ExportMessages(e.ctx, e.filter, messagesJSONLSink{encoder: encoder})
		if err != nil {
			return fmt.Errorf("export messages: %w", err)
		}
	}
	if err := json.MarshalEncode(encoder, messagesComplete{
		RecordType: "complete",
		Counts: messagesCounts{
			Sources:       counts.Sources,
			Conversations: counts.Conversations,
			Messages:      counts.Messages,
		},
	}, json.Deterministic(true)); err != nil {
		return fmt.Errorf("encode message export completion: %w", err)
	}
	return nil
}

type messagesWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type messagesFilters struct {
	PersonID     *int64                  `json:"person_id,omitzero"`
	MessageTypes []string                `json:"message_types"`
	Sources      []MessageSourceSelector `json:"sources"`
}

type messagesManifest struct {
	RecordType      string          `json:"record_type"`
	Schema          string          `json:"schema"`
	MsgvaultVersion string          `json:"msgvault_version"`
	Window          messagesWindow  `json:"window"`
	Filters         messagesFilters `json:"filters"`
}

type messagesSourceRecord struct {
	RecordType           string     `json:"record_type"`
	SourceType           string     `json:"source_type"`
	Identifier           string     `json:"identifier"`
	DisplayName          string     `json:"display_name"`
	LastSuccessfulSyncAt *time.Time `json:"last_successful_sync_at"`
}

type messagesConversationRecord struct {
	RecordType       string                              `json:"record_type"`
	SourceType       string                              `json:"source_type"`
	SourceIdentifier string                              `json:"source_identifier"`
	ID               string                              `json:"id"`
	Title            string                              `json:"title"`
	ConversationType store.MessageExportConversationType `json:"conversation_type"`
	ParentID         *string                             `json:"parent_id"`
}

type messagesAuthorRecord struct {
	DisplayName string `json:"display_name"`
	Address     string `json:"address"`
}

type messagesMessageRecord struct {
	RecordType        string                `json:"record_type"`
	SourceType        string                `json:"source_type"`
	SourceIdentifier  string                `json:"source_identifier"`
	ID                string                `json:"id"`
	ConversationID    string                `json:"conversation_id"`
	MessageType       string                `json:"message_type"`
	Subject           string                `json:"subject"`
	Text              string                `json:"text"`
	Author            *messagesAuthorRecord `json:"author"`
	OccurredAt        time.Time             `json:"occurred_at"`
	DeletedFromSource bool                  `json:"deleted_from_source"`
}

type messagesComplete struct {
	RecordType string         `json:"record_type"`
	Counts     messagesCounts `json:"counts"`
}

type messagesCounts struct {
	Sources       int `json:"sources"`
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
}

type messagesJSONLSink struct {
	encoder *jsontext.Encoder
}

func (s messagesJSONLSink) Source(source store.MessageExportSource) error {
	return json.MarshalEncode(s.encoder, messagesSourceRecord{
		RecordType:           "source",
		SourceType:           source.SourceType,
		Identifier:           source.Identifier,
		DisplayName:          source.DisplayName,
		LastSuccessfulSyncAt: source.LastSuccessfulSyncAt,
	}, json.Deterministic(true))
}

func (s messagesJSONLSink) Conversation(conversation store.MessageExportConversation) error {
	return json.MarshalEncode(s.encoder, messagesConversationRecord{
		RecordType:       "conversation",
		SourceType:       conversation.SourceType,
		SourceIdentifier: conversation.SourceIdentifier,
		ID:               conversation.ID,
		Title:            conversation.Title,
		ConversationType: conversation.ConversationType,
		ParentID:         conversation.ParentID,
	}, json.Deterministic(true))
}

func (s messagesJSONLSink) Message(message store.MessageExportMessage) error {
	var author *messagesAuthorRecord
	if message.Author != nil {
		author = &messagesAuthorRecord{DisplayName: message.Author.DisplayName, Address: message.Author.Address}
	}
	return json.MarshalEncode(s.encoder, messagesMessageRecord{
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
	}, json.Deterministic(true))
}

func parseMessageBounds(startValue, endValue string) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339, startValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid --start %q (expected RFC3339): %w", startValue, err)
	}
	end, err := time.Parse(time.RFC3339, endValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid --end %q (expected RFC3339): %w", endValue, err)
	}
	start, end = start.UTC(), end.UTC()
	if start.IsZero() {
		// The store reads the zero time as a missing bound.
		return time.Time{}, time.Time{}, fmt.Errorf("invalid --start %q: use a time after %s", startValue, time.Time{}.Format(time.RFC3339))
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("--start must be before --end")
	}
	return start, end, nil
}

func normalizeMessageTypes(values []string) ([]string, error) {
	normalized := append([]string(nil), values...)
	for _, value := range normalized {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("--message-type must be non-empty")
		}
	}
	sort.Strings(normalized)
	return compactSortedStrings(normalized), nil
}

func normalizeMessageSelectors(values []string) ([]MessageSourceSelector, error) {
	selectors := make([]MessageSourceSelector, 0, len(values))
	for _, value := range values {
		sourceType, identifier, ok := strings.Cut(value, ":")
		if !ok || strings.TrimSpace(sourceType) == "" || strings.TrimSpace(identifier) == "" {
			return nil, fmt.Errorf("invalid --source %q (expected non-empty type:identifier)", value)
		}
		selectors = append(selectors, MessageSourceSelector{SourceType: sourceType, Identifier: identifier})
	}
	sort.Slice(selectors, func(i, j int) bool {
		if selectors[i].SourceType != selectors[j].SourceType {
			return selectors[i].SourceType < selectors[j].SourceType
		}
		return selectors[i].Identifier < selectors[j].Identifier
	})
	return compactMessageSelectors(selectors), nil
}

// resolveMessageSources treats a source outside a non-nil scope as missing,
// so a scoped caller cannot learn that it exists.
func resolveMessageSources(ctx context.Context, st *store.Store, selectors []MessageSourceSelector, scope []int64) ([]int64, error) {
	sourceIDs := make([]int64, 0, len(selectors))
	for _, selector := range selectors {
		sources, err := st.ListSourcesContext(ctx, selector.SourceType)
		if err != nil {
			return nil, fmt.Errorf("list sources of type %q: %w", selector.SourceType, err)
		}
		var matched *store.Source
		for _, source := range sources {
			if source.Identifier == selector.Identifier && (scope == nil || slices.Contains(scope, source.ID)) {
				matched = source
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("source %s:%s %w", selector.SourceType, selector.Identifier, ErrMessageSourceNotFound)
		}
		sourceIDs = append(sourceIDs, matched.ID)
	}
	return sourceIDs, nil
}

// ErrMessageSourceNotFound marks a --source selector that names no archive source.
var ErrMessageSourceNotFound = errors.New("not found")

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

func compactMessageSelectors(selectors []MessageSourceSelector) []MessageSourceSelector {
	if len(selectors) == 0 {
		return []MessageSourceSelector{}
	}
	compacted := selectors[:1]
	for _, selector := range selectors[1:] {
		if selector != compacted[len(compacted)-1] {
			compacted = append(compacted, selector)
		}
	}
	return compacted
}
