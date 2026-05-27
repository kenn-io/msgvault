// Package storetest provides a StoreFixture and helpers for tests that
// exercise the Store layer through its public API.
package storetest

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var globalCounter atomic.Int64

// Fixture holds common test state for store-level tests.
type Fixture struct {
	T          *testing.T
	Store      *store.Store
	Source     *store.Source
	ConvID     int64
	msgCounter atomic.Int64
}

// New creates a Fixture with a fresh test database, one source
// ("test@example.com") and one default conversation.
func New(t *testing.T) *Fixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err, "setup: GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "default-thread", "Default Thread")
	require.NoError(t, err, "setup: EnsureConversation")
	return &Fixture{T: t, Store: st, Source: source, ConvID: convID}
}

// CreateMessage inserts a minimal message and returns its internal ID.
func (f *Fixture) CreateMessage(sourceMessageID string) int64 {
	f.T.Helper()
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: sourceMessageID,
		MessageType:     "email",
		SizeEstimate:    1000,
	})
	require.NoError(f.T, err, "CreateMessage")
	return id
}

// CreateMessages inserts count messages with IDs "msg-0" .. "msg-(count-1)".
func (f *Fixture) CreateMessages(count int) []int64 {
	f.T.Helper()
	ids := make([]int64, 0, count)
	for i := range count {
		ids = append(ids, f.CreateMessage(fmt.Sprintf("msg-%d", i)))
	}
	return ids
}

// EnsureLabels creates labels and returns a map of sourceLabelID → internal ID.
func (f *Fixture) EnsureLabels(labels map[string]string, typ string) map[string]int64 {
	f.T.Helper()
	result := make(map[string]int64, len(labels))
	for sourceLabelID, name := range labels {
		require.NotEmptyf(f.T, name, "EnsureLabels: label name is required (sourceLabelID=%q)", sourceLabelID)
		require.NotEmpty(f.T, sourceLabelID, "EnsureLabels: sourceLabelID is required")
		lid, err := f.Store.EnsureLabel(f.Source.ID, sourceLabelID, name, typ)
		require.NoError(f.T, err, "EnsureLabel "+sourceLabelID)
		result[sourceLabelID] = lid
	}
	return result
}

// EnsureParticipant creates or gets a participant and returns its ID.
func (f *Fixture) EnsureParticipant(email, name, domain string) int64 {
	f.T.Helper()
	pid, err := f.Store.EnsureParticipant(email, name, domain)
	require.NoError(f.T, err, "EnsureParticipant "+email)
	return pid
}

// StartSync starts a sync run and returns its ID.
func (f *Fixture) StartSync() int64 {
	f.T.Helper()
	syncID, err := f.Store.StartSync(f.Source.ID, "full")
	require.NoError(f.T, err, "StartSync")
	require.NotZero(f.T, syncID, "sync ID should be non-zero")
	return syncID
}

// --- Query helpers ---

// MessageFields holds a subset of message columns for test verification.
type MessageFields struct {
	Subject        string
	Snippet        string
	HasAttachments bool
}

// GetMessageFields returns selected fields of a message by ID.
func (f *Fixture) GetMessageFields(msgID int64) MessageFields {
	f.T.Helper()
	var mf MessageFields
	err := f.Store.DB().QueryRowContext(f.T.Context(),
		f.Store.Rebind("SELECT subject, snippet, has_attachments FROM messages WHERE id = ?"), msgID,
	).Scan(&mf.Subject, &mf.Snippet, &mf.HasAttachments)
	require.NoError(f.T, err, "GetMessageFields")
	return mf
}

// GetMessageBody returns the body_text and body_html for a message.
func (f *Fixture) GetMessageBody(msgID int64) (sql.NullString, sql.NullString) {
	f.T.Helper()
	var bodyText, bodyHTML sql.NullString
	err := f.Store.DB().QueryRowContext(f.T.Context(),
		f.Store.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), msgID,
	).Scan(&bodyText, &bodyHTML)
	require.NoError(f.T, err, "GetMessageBody")
	return bodyText, bodyHTML
}

// GetSyncRun returns the status and error_message for a sync run by ID.
func (f *Fixture) GetSyncRun(syncID int64) (status, errorMsg string) {
	f.T.Helper()
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT status, error_message FROM sync_runs WHERE id = ?"), syncID,
	).Scan(&status, &errorMsg)
	require.NoError(f.T, err, "GetSyncRun")
	return status, errorMsg
}

// GetSingleLabelID returns the label_id for a message that should have exactly one label.
func (f *Fixture) GetSingleLabelID(msgID int64) int64 {
	f.T.Helper()
	var labelID int64
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT label_id FROM message_labels WHERE message_id = ?"), msgID,
	).Scan(&labelID)
	require.NoError(f.T, err, "GetSingleLabelID")
	return labelID
}

// GetSingleRecipientID returns the participant_id for a message+type that should have exactly one recipient.
func (f *Fixture) GetSingleRecipientID(msgID int64, typ string) int64 {
	f.T.Helper()
	var pid int64
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = ?"), msgID, typ,
	).Scan(&pid)
	require.NoError(f.T, err, "GetSingleRecipientID")
	return pid
}

// --- Assertion helpers ---

// AssertLabelCount asserts the number of labels attached to a message.
func (f *Fixture) AssertLabelCount(msgID int64, want int) {
	f.T.Helper()
	var count int
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM message_labels WHERE message_id = ?"), msgID).Scan(&count)
	require.NoError(f.T, err, "count message_labels")
	assert.Equal(f.T, want, count, "message_labels count")
}

// AssertMessageHasLabel asserts that a message is linked to a specific label ID.
func (f *Fixture) AssertMessageHasLabel(msgID, labelID int64) {
	f.T.Helper()
	var count int
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?"),
		msgID, labelID,
	).Scan(&count)
	require.NoError(f.T, err, "check message_labels")
	assert.Equalf(f.T, 1, count, "message %d should have label %d", msgID, labelID)
}

// AssertRecipientCount asserts the number of recipients of a given type for a message.
func (f *Fixture) AssertRecipientCount(msgID int64, typ string, want int) {
	f.T.Helper()
	var count int
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = ?"), msgID, typ).Scan(&count)
	require.NoError(f.T, err, "count message_recipients")
	assert.Equalf(f.T, want, count, "message_recipients(%s) count", typ)
}

// AssertMessageDeleted asserts that a message has been marked as deleted.
func (f *Fixture) AssertMessageDeleted(msgID int64) {
	f.T.Helper()
	var deletedAt sql.NullTime
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT deleted_from_source_at FROM messages WHERE id = ?"), msgID).Scan(&deletedAt)
	require.NoError(f.T, err, "check deleted_from_source_at")
	assert.True(f.T, deletedAt.Valid, "deleted_from_source_at should be set")
}

// AssertMessageNotDeleted asserts that a message has NOT been marked as deleted.
func (f *Fixture) AssertMessageNotDeleted(msgID int64) {
	f.T.Helper()
	var deletedAt sql.NullTime
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT deleted_from_source_at FROM messages WHERE id = ?"), msgID).Scan(&deletedAt)
	require.NoError(f.T, err, "check deleted_from_source_at")
	assert.False(f.T, deletedAt.Valid, "deleted_from_source_at should be NULL")
}

// AssertActiveSync asserts there is an active sync with the given ID and status.
func (f *Fixture) AssertActiveSync(wantID int64, wantStatus string) {
	f.T.Helper()
	active, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(f.T, err, "GetActiveSync")
	require.NotNil(f.T, active, "expected active sync, got nil")
	assert.Equal(f.T, wantID, active.ID, "active sync ID")
	assert.Equal(f.T, wantStatus, active.Status, "active sync status")
}

// AssertNoActiveSync asserts there is no active sync for the fixture's source.
func (f *Fixture) AssertNoActiveSync() {
	f.T.Helper()
	active, err := f.Store.GetActiveSync(f.Source.ID)
	require.ErrorIs(f.T, err, store.ErrSyncRunNotFound, "GetActiveSync")
	assert.Nilf(f.T, active, "expected no active sync, got %+v", active)
}

// --- MessageBuilder ---

// MessageBuilder provides a fluent API for constructing store.Message values.
type MessageBuilder struct {
	msg store.Message
}

// NewMessage creates a builder with sensible defaults.
// NewMessage creates a builder with sensible defaults. It uses a global counter
// to generate unique SourceMessageID values; prefer Fixture.NewMessage for
// per-test deterministic IDs.
func NewMessage(sourceID, convID int64) *MessageBuilder {
	return &MessageBuilder{
		msg: store.Message{
			ConversationID:  convID,
			SourceID:        sourceID,
			SourceMessageID: fmt.Sprintf("test-msg-%d", globalCounter.Add(1)),
			MessageType:     "email",
			SizeEstimate:    1000,
		},
	}
}

// NewMessage creates a builder using the fixture's per-test counter for
// deterministic SourceMessageID values (fixture-msg-1, fixture-msg-2, ...).
// Uses a distinct prefix from the package-level NewMessage to avoid collisions.
func (f *Fixture) NewMessage() *MessageBuilder {
	return &MessageBuilder{
		msg: store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: fmt.Sprintf("fixture-msg-%d", f.msgCounter.Add(1)),
			MessageType:     "email",
			SizeEstimate:    1000,
		},
	}
}

func (b *MessageBuilder) WithSourceMessageID(id string) *MessageBuilder {
	b.msg.SourceMessageID = id
	return b
}

func (b *MessageBuilder) WithSubject(s string) *MessageBuilder {
	b.msg.Subject = sql.NullString{String: s, Valid: true}
	return b
}

func (b *MessageBuilder) WithSnippet(s string) *MessageBuilder {
	b.msg.Snippet = sql.NullString{String: s, Valid: true}
	return b
}

func (b *MessageBuilder) WithSize(n int64) *MessageBuilder {
	b.msg.SizeEstimate = n
	return b
}

func (b *MessageBuilder) WithSentAt(t time.Time) *MessageBuilder {
	b.msg.SentAt = sql.NullTime{Time: t, Valid: true}
	return b
}

func (b *MessageBuilder) WithReceivedAt(t time.Time) *MessageBuilder {
	b.msg.ReceivedAt = sql.NullTime{Time: t, Valid: true}
	return b
}

func (b *MessageBuilder) WithInternalDate(t time.Time) *MessageBuilder {
	b.msg.InternalDate = sql.NullTime{Time: t, Valid: true}
	return b
}

// WithAttachmentCount sets the attachment count and HasAttachments flag.
func (b *MessageBuilder) WithAttachmentCount(count int) *MessageBuilder {
	b.msg.HasAttachments = count > 0
	b.msg.AttachmentCount = count
	return b
}

func (b *MessageBuilder) WithIsFromMe(v bool) *MessageBuilder {
	b.msg.IsFromMe = v
	return b
}

// Build returns the constructed Message.
func (b *MessageBuilder) Build() *store.Message {
	m := b.msg
	return &m
}

// Create inserts the message into the store and returns its internal ID.
func (b *MessageBuilder) Create(t *testing.T, st *store.Store) int64 {
	t.Helper()
	m := b.msg
	id, err := st.UpsertMessage(&m)
	require.NoError(t, err, "MessageBuilder.Create")
	return id
}
