package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

const testEmail = "test@example.com"

type TestEnv struct {
	Store   *store.Store
	Mock    *gmail.MockAPI
	Syncer  *Syncer
	TmpDir  string
	Context context.Context
}

func newTestEnv(t *testing.T, opt ...*Options) *TestEnv {
	t.Helper()

	require.LessOrEqual(t, len(opt), 1, "newTestEnv: at most one *Options allowed")

	tmpDir, err := os.MkdirTemp("", "msgvault-test-*")
	require.NoError(t, err, "create temp dir")
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(t, st.InitSchema(), "init schema")

	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{
		EmailAddress:  testEmail,
		MessagesTotal: 0,
		HistoryID:     1000,
	}

	var o *Options
	if len(opt) > 0 {
		o = opt[0]
	}

	return &TestEnv{
		Store:   st,
		Mock:    mock,
		Syncer:  New(mock, st, o),
		TmpDir:  tmpDir,
		Context: context.Background(),
	}
}

// CreateSourceWithHistory creates a source and sets its sync cursor for incremental sync tests.
// The returned Source has SyncCursor populated so it can be passed directly to Incremental().
func (e *TestEnv) CreateSourceWithHistory(t *testing.T, historyID string) *store.Source {
	t.Helper()
	source, err := e.Store.GetOrCreateSource("gmail", e.Mock.Profile.EmailAddress)
	require.NoError(t, err, "GetOrCreateSource")
	require.NoError(t, e.Store.UpdateSourceSyncCursor(source.ID, historyID), "UpdateSourceSyncCursor")
	source.SyncCursor = sql.NullString{String: historyID, Valid: true}
	return source
}

// CreateSource creates a source without setting a sync cursor (for full sync tests).
func (e *TestEnv) CreateSource(t *testing.T) *store.Source {
	t.Helper()
	source, err := e.Store.GetOrCreateSource("gmail", e.Mock.Profile.EmailAddress)
	require.NoError(t, err, "GetOrCreateSource")
	return source
}

// SetOptions replaces the Syncer with one configured by the given modifier function.
func (e *TestEnv) SetOptions(t *testing.T, mod func(*Options)) {
	t.Helper()
	opts := DefaultOptions()
	mod(opts)
	e.Syncer = New(e.Mock, e.Store, opts)
}

// SetHistory configures mock history records and the target history ID for incremental sync tests.
func (e *TestEnv) SetHistory(historyID uint64, records ...gmail.HistoryRecord) {
	e.Mock.Profile.HistoryID = historyID
	e.Mock.HistoryRecords = records
	e.Mock.HistoryID = historyID
}

// seedMessages sets the profile totals/historyID and adds messages to the mock.
func seedMessages(env *TestEnv, total int64, historyID uint64, msgs ...string) {
	env.Mock.Profile.MessagesTotal = total
	env.Mock.Profile.HistoryID = historyID
	for _, id := range msgs {
		env.Mock.AddMessage(id, testMIME(), []string{"INBOX"})
	}
}

// runFullSync runs a full sync and fails the test on error.
func runFullSync(t *testing.T, env *TestEnv) *gmail.SyncSummary {
	t.Helper()
	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(t, err, "full sync")
	return summary
}

// runIncrementalSync runs an incremental sync and fails the test on error.
// Looks up the Gmail source for testEmail (all sync tests use "gmail" type).
func runIncrementalSync(t *testing.T, env *TestEnv) *gmail.SyncSummary {
	t.Helper()
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(t, err, "look up source for incremental sync")
	summary, err := env.Syncer.Incremental(env.Context, source)
	require.NoError(t, err, "incremental sync")
	return summary
}

// WantSummary specifies expected SyncSummary values. Nil fields are not checked.
type WantSummary struct {
	Added   *int64
	Errors  *int64
	Skipped *int64
	Found   *int64
}

// assertSummary checks SyncSummary fields against expected values.
// Only non-nil fields in want are checked.
func assertSummary(t *testing.T, s *gmail.SyncSummary, want WantSummary) {
	t.Helper()
	if want.Added != nil {
		assert.Equal(t, *want.Added, s.MessagesAdded, "messages added")
	}
	if want.Errors != nil {
		assert.Equal(t, *want.Errors, s.Errors, "errors")
	}
	if want.Skipped != nil {
		assert.Equal(t, *want.Skipped, s.MessagesSkipped, "messages skipped")
	}
	if want.Found != nil {
		assert.Equal(t, *want.Found, s.MessagesFound, "messages found")
	}
}

// mustStats calls GetStats and fails on error.
func mustStats(t *testing.T, st *store.Store) *store.Stats {
	t.Helper()
	stats, err := st.GetStats()
	require.NoError(t, err, "GetStats")
	return stats
}

// assertMockCalls verifies the expected number of API calls on the mock.
// Pass -1 to skip checking a particular call count.
func assertMockCalls(t *testing.T, env *TestEnv, profile, labels, messages int) {
	t.Helper()
	if profile >= 0 {
		assert.Equal(t, profile, env.Mock.ProfileCalls, "profile calls")
	}
	if labels >= 0 {
		assert.Equal(t, labels, env.Mock.LabelsCalls, "labels calls")
	}
	if messages >= 0 {
		assert.Len(t, env.Mock.GetMessageCalls, messages, "message fetches")
	}
}

// assertListMessagesCalls verifies the number of ListMessages API calls (pagination).
func assertListMessagesCalls(t *testing.T, env *TestEnv, want int) {
	t.Helper()
	assert.Equal(t, want, env.Mock.ListMessagesCalls, "ListMessages calls")
}

// assertMessageCount checks the message count in the store.
func assertMessageCount(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	stats := mustStats(t, st)
	assert.Equal(t, want, stats.MessageCount, "messages in db")
}

// assertAttachmentCount checks the attachment count in the store.
func assertAttachmentCount(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	stats := mustStats(t, st)
	assert.Equal(t, want, stats.AttachmentCount, "attachments in db")
}

// withAttachmentsDir creates a syncer with an attachments directory and returns the dir path.
func withAttachmentsDir(t *testing.T, env *TestEnv) string {
	t.Helper()
	attachDir := filepath.Join(env.TmpDir, "attachments")
	env.Syncer = New(env.Mock, env.Store, &Options{AttachmentsDir: attachDir})
	return attachDir
}

// assertRecipientCount checks the count of recipients of a given type for a message.
func assertRecipientCount(t *testing.T, st *store.Store, sourceMessageID, recipType string, want int) {
	t.Helper()
	count, err := st.InspectRecipientCount(sourceMessageID, recipType)
	require.NoError(t, err, "InspectRecipientCount(%s, %s)", sourceMessageID, recipType)
	assert.Equal(t, want, count, "%s recipients for %s", recipType, sourceMessageID)
}

// assertDisplayName checks the display name for a recipient of a message.
func assertDisplayName(t *testing.T, st *store.Store, sourceMessageID, recipType, email, want string) {
	t.Helper()
	displayName, err := st.InspectDisplayName(sourceMessageID, recipType, email)
	require.NoError(t, err, "InspectDisplayName(%s, %s, %s)", sourceMessageID, recipType, email)
	assert.Equal(t, want, displayName, "display name for %s/%s/%s", sourceMessageID, recipType, email)
}

// assertDeletedFromSource checks whether a message has deleted_from_source_at set.
func assertDeletedFromSource(t *testing.T, st *store.Store, sourceMessageID string, wantDeleted bool) {
	t.Helper()
	deleted, err := st.InspectDeletedFromSource(sourceMessageID)
	require.NoError(t, err, "InspectDeletedFromSource(%s)", sourceMessageID)
	assert.Equal(t, wantDeleted, deleted, "deleted_from_source_at for %s", sourceMessageID)
}

// assertBodyContains checks that a message's body_text contains the given substring.
func assertBodyContains(t *testing.T, st *store.Store, sourceMessageID, substr string) {
	t.Helper()
	bodyText, err := st.InspectBodyText(sourceMessageID)
	require.NoError(t, err, "InspectBodyText(%s)", sourceMessageID)
	assert.Contains(t, bodyText, substr, "body of %s", sourceMessageID)
}

// assertRawDataExists checks that raw MIME data exists for a message.
func assertRawDataExists(t *testing.T, st *store.Store, sourceMessageID string) {
	t.Helper()
	exists, err := st.InspectRawDataExists(sourceMessageID)
	require.NoError(t, err, "InspectRawDataExists(%s)", sourceMessageID)
	assert.True(t, exists, "expected raw MIME data to be preserved for %s", sourceMessageID)
}

// assertThreadSourceID checks the source_conversation_id for a message's thread.
func assertThreadSourceID(t *testing.T, st *store.Store, sourceMessageID, wantThreadID string) {
	t.Helper()
	threadSourceID, err := st.InspectThreadSourceID(sourceMessageID)
	require.NoError(t, err, "InspectThreadSourceID(%s)", sourceMessageID)
	assert.Equal(t, wantThreadID, threadSourceID, "thread source_conversation_id for %s", sourceMessageID)
}

// assertDateFallback checks that sent_at equals internal_date and contains expected substrings.
func assertDateFallback(t *testing.T, st *store.Store, sourceMessageID, wantDatePart, wantTimePart string) {
	t.Helper()
	sentAt, internalDate, err := st.InspectMessageDates(sourceMessageID)
	require.NoError(t, err, "InspectMessageDates(%s)", sourceMessageID)
	assert.NotEmpty(t, sentAt, "%s: sent_at should not be empty", sourceMessageID)
	assert.NotEmpty(t, internalDate, "%s: internal_date should not be empty", sourceMessageID)
	assert.Equal(t, internalDate, sentAt, "%s: sent_at should equal internal_date", sourceMessageID)
	assert.Contains(t, sentAt, wantDatePart, "%s: sent_at should contain date part", sourceMessageID)
	assert.Contains(t, sentAt, wantTimePart, "%s: sent_at should contain time part", sourceMessageID)
}

// assertMessageHasLabel checks that a message has a specific label (by source_label_id).
func assertMessageHasLabel(t *testing.T, st *store.Store, sourceMessageID, sourceLabelID string) {
	t.Helper()
	var count int
	err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN messages m ON ml.message_id = m.id
		JOIN labels l ON ml.label_id = l.id
		WHERE m.source_message_id = ? AND l.source_label_id = ?
	`, sourceMessageID, sourceLabelID).Scan(&count)
	require.NoError(t, err, "assertMessageHasLabel(%s, %s)", sourceMessageID, sourceLabelID)
	assert.NotZero(t, count, "message %s should have label %s", sourceMessageID, sourceLabelID)
}

// assertMessageNotHasLabel checks that a message does NOT have a specific label.
func assertMessageNotHasLabel(t *testing.T, st *store.Store, sourceMessageID, sourceLabelID string) {
	t.Helper()
	var count int
	err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN messages m ON ml.message_id = m.id
		JOIN labels l ON ml.label_id = l.id
		WHERE m.source_message_id = ? AND l.source_label_id = ?
	`, sourceMessageID, sourceLabelID).Scan(&count)
	require.NoError(t, err, "assertMessageNotHasLabel(%s, %s)", sourceMessageID, sourceLabelID)
	assert.Zero(t, count, "message %s should NOT have label %s", sourceMessageID, sourceLabelID)
}

// History event builders — construct gmail.HistoryRecord values succinctly.

func historyAdded(id string) gmail.HistoryRecord {
	return gmail.HistoryRecord{
		MessagesAdded: []gmail.HistoryMessage{
			{Message: gmail.MessageID{ID: id, ThreadID: "thread_" + id}},
		},
	}
}

func historyDeleted(id string) gmail.HistoryRecord {
	return gmail.HistoryRecord{
		MessagesDeleted: []gmail.HistoryMessage{
			{Message: gmail.MessageID{ID: id, ThreadID: "thread_" + id}},
		},
	}
}

func historyLabelAdded(id string, labels ...string) gmail.HistoryRecord {
	return gmail.HistoryRecord{
		LabelsAdded: []gmail.HistoryLabelChange{
			{
				Message:  gmail.MessageID{ID: id, ThreadID: "thread_" + id},
				LabelIDs: labels,
			},
		},
	}
}

func historyLabelRemoved(id string, labels ...string) gmail.HistoryRecord {
	return gmail.HistoryRecord{
		LabelsRemoved: []gmail.HistoryLabelChange{
			{
				Message:  gmail.MessageID{ID: id, ThreadID: "thread_" + id},
				LabelIDs: labels,
			},
		},
	}
}

// seedPagedMessages adds `total` messages to the mock distributed across pages of `pageSize`.
// Message IDs use the given prefix: prefix1, prefix2, etc.
func seedPagedMessages(env *TestEnv, total int, pageSize int, prefix string) {
	env.Mock.Profile.MessagesTotal = int64(total)
	var pages [][]string
	var page []string
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("%s%d", prefix, i)
		env.Mock.AddMessage(id, testMIME(), []string{"INBOX"})
		page = append(page, id)
		if len(page) == pageSize {
			pages = append(pages, page)
			page = nil
		}
	}
	if len(page) > 0 {
		pages = append(pages, page)
	}
	env.Mock.MessagePages = pages
}

// countFiles counts regular files recursively under dir.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	var count int
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	require.NoError(t, err, "WalkDir(%s)", dir)
	return count
}
