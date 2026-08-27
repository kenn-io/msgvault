// Package peoplebrowser defines the transport-neutral backend used by the
// People TUI. Implementations may use HTTP or another storage boundary.
package peoplebrowser

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// Backend provides the People directory, profile, and activity operations.
type Backend interface {
	Search(ctx context.Context, request SearchRequest) (*SearchPage, error)
	Complete(ctx context.Context, request CompletionRequest) (*CompletionPage, error)
	GetContact(ctx context.Context, participantID int64) (*query.PersonSummary, error)
	Promote(ctx context.Context, participantID int64) (*store.Person, error)
	ListAttributes(ctx context.Context, personID int64) (*Attributes, error)
	CreateField(ctx context.Context, field NewField) (*store.AttributeDefinition, error)
	SetAttribute(ctx context.Context, request SetAttributeRequest) (*store.PersonAttributeWrite, error)
	AppendNote(ctx context.Context, request AppendNoteRequest) (*store.PersonAttributeWrite, error)
	ListInboxes(ctx context.Context, participantID int64) (*query.PersonInboxResponse, error)
	ListConversations(ctx context.Context, filter query.TextFilter) (*ConversationPage, error)
	ListConversationMessages(ctx context.Context, conversationID int64, filter query.TextFilter) (*ConversationMessagePage, error)
	ListMeetings(ctx context.Context, request ContactPageRequest) (*MessagePage, error)
	ListFiles(ctx context.Context, request ContactPageRequest) (*FilePage, error)
	ListActivity(ctx context.Context, request ActivityPageRequest) (*ActivityPage, error)
	RelationshipCalendar(ctx context.Context, request CalendarRequest) (*query.RelationshipCalendarResponse, error)
	GetMessage(ctx context.Context, messageID int64) (*query.MessageDetail, error)
}

// ProfileLister exposes the deliberately small, unpaginated durable profile
// set to consumers that merge curated identities with analytical contacts.
type ProfileLister interface {
	ListProfiles(ctx context.Context) ([]store.Person, error)
}

type CalendarRequest struct {
	ParticipantID int64
	Year          int
	Timezone      string
}

type CompletionRequest struct {
	Query string
	Limit int
}

type CompletionPage struct {
	Rows          []CompletionRow
	CacheRevision string
}

type CompletionRow struct {
	ParticipantID int64
	DisplayLabel  string
	Kind          query.PeopleCompletionKind
	Value         string
	Source        string
}

type SearchRequest struct {
	Query  string
	Cursor string
	Limit  int
}

type SearchPage struct {
	Rows          []query.PersonSummary
	TotalCount    int64
	NextCursor    string
	CacheRevision string
}

type Attributes struct {
	PersonID int64
	Groups   []AttributeGroup
}

type AttributeGroup struct {
	Definition store.AttributeDefinition
	Current    []store.PersonAttributeValue
}

type NewField struct {
	Label       string
	Kind        FieldKind
	Cardinality store.AttributeCardinality
}

type DefinitionInput struct {
	Label       string
	ValueType   store.AttributeValueType
	FieldType   store.AttributeFieldType
	Cardinality store.AttributeCardinality
}

type SetAttributeRequest struct {
	PersonID        int64
	Slug            string
	Value           store.AttributeValue
	Ordinal         *int64
	ExpectedValueID *int64
	Source          store.Provenance
	Actor           string
}

type AppendNoteRequest struct {
	PersonID int64
	Text     string
	Source   store.Provenance
	Actor    string
}

type ContactPageRequest struct {
	ParticipantID int64
	Cursor        string
	Offset        int
	Limit         int
}

type MessagePage struct {
	Rows          []query.MessageSummary
	NextCursor    string
	CacheRevision string
}

// ConversationPage is one transport-neutral offset page from the text API.
// CacheRevision is optional because not every text backend exposes a snapshot
// revision; when present, callers must not stitch pages across revisions.
type ConversationPage struct {
	Rows          []query.ConversationRow
	NextOffset    int
	Complete      bool
	CacheRevision string
}

// ConversationMessagePage is one metadata-only timeline page. Full message
// bodies are loaded through GetMessage after an explicit selection.
type ConversationMessagePage struct {
	Rows          []query.MessageSummary
	NextOffset    int
	Complete      bool
	CacheRevision string
}

type FilePage struct {
	Rows          []query.FileRow
	TotalCount    int64
	NextCursor    string
	CacheRevision string
}

type ActivityPage struct {
	Rows          []query.EntryRow
	TotalCount    int64
	NextCursor    string
	CacheRevision string
}

type ActivityPageRequest = ContactPageRequest

var ErrStaleValue = errors.New("person attribute value changed")

// ErrContactNotFound reports that the observed participant disappeared or was
// merged between directory selection and a contact refresh.
var ErrContactNotFound = errors.New("people contact no longer exists")

type StaleValueError struct {
	CurrentValueID int64
	CurrentValue   *store.PersonAttributeValue
}

func (StaleValueError) Error() string { return ErrStaleValue.Error() }
func (StaleValueError) Unwrap() error { return ErrStaleValue }
