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
	Search(context.Context, SearchRequest) (*SearchPage, error)
	Complete(context.Context, CompletionRequest) (*CompletionPage, error)
	GetContact(context.Context, int64) (*query.PersonSummary, error)
	Promote(context.Context, int64) (*store.Person, error)
	ListAttributes(context.Context, int64) (*Attributes, error)
	CreateField(context.Context, NewField) (*store.AttributeDefinition, error)
	SetAttribute(context.Context, SetAttributeRequest) (*store.PersonAttributeWrite, error)
	AppendNote(context.Context, AppendNoteRequest) (*store.PersonAttributeWrite, error)
	ListInboxes(context.Context, int64) (*query.PersonInboxResponse, error)
	ListConversations(context.Context, query.TextFilter) (*ConversationPage, error)
	ListConversationMessages(context.Context, int64, query.TextFilter) (*ConversationMessagePage, error)
	ListMeetings(context.Context, ContactPageRequest) (*MessagePage, error)
	ListFiles(context.Context, ContactPageRequest) (*FilePage, error)
	ListActivity(context.Context, ActivityPageRequest) (*ActivityPage, error)
	GetMessage(context.Context, int64) (*query.MessageDetail, error)
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
}

type AppendNoteRequest struct {
	PersonID int64
	Text     string
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
