package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	if len(opt) > 1 {
		t.Fatalf("newTestEnv: at most one *Options allowed, got %d", len(opt))
	}

	tmpDir, err := os.MkdirTemp("", "msgvault-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	dbPath := filepath.Join(tmpDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

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
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	if err := e.Store.UpdateSourceSyncCursor(source.ID, historyID); err != nil {
		t.Fatalf("UpdateSourceSyncCursor: %v", err)
	}
	source.SyncCursor = sql.NullString{String: historyID, Valid: true}
	return source
}

// CreateSource creates a source without setting a sync cursor (for full sync tests).
func (e *TestEnv) CreateSource(t *testing.T) *store.Source {
	t.Helper()
	source, err := e.Store.GetOrCreateSource("gmail", e.Mock.Profile.EmailAddress)
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
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
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	return summary
}

// runIncrementalSync runs an incremental sync and fails the test on error.
// Looks up the Gmail source for testEmail (all sync tests use "gmail" type).
func runIncrementalSync(t *testing.T, env *TestEnv) *gmail.SyncSummary {
	t.Helper()
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	if err != nil {
		t.Fatalf("look up source for incremental sync: %v", err)
	}
	summary, err := env.Syncer.Incremental(env.Context, source)
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	return summary
}

// WantSummary specifies expected SyncSummary values. Nil fields are not checked.
type WantSummary struct {
	Added   *int64
	Errors  *int64
	Skipped *int64
	Found   *int64
}

// intPtr returns a pointer to an int64 value for use in WantSummary.
func intPtr(v int64) *int64 { return &v }

// assertSummary checks SyncSummary fields against expected values.
// Only non-nil fields in want are checked.
func assertSummary(t *testing.T, s *gmail.SyncSummary, want WantSummary) {
	t.Helper()
	if want.Added != nil && s.MessagesAdded != *want.Added {
		t.Errorf("expected %d messages added, got %d", *want.Added, s.MessagesAdded)
	}
	if want.Errors != nil && s.Errors != *want.Errors {
		t.Errorf("expected %d errors, got %d", *want.Errors, s.Errors)
	}
	if want.Skipped != nil && s.MessagesSkipped != *want.Skipped {
		t.Errorf("expected %d messages skipped, got %d", *want.Skipped, s.MessagesSkipped)
	}
	if want.Found != nil && s.MessagesFound != *want.Found {
		t.Errorf("expected %d messages found, got %d", *want.Found, s.MessagesFound)
	}
}

// mustStats calls GetStats and fails on error.
func mustStats(t *testing.T, st *store.Store) *store.Stats {
	t.Helper()
	stats, err := st.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	return stats
}

// assertMockCalls verifies the expected number of API calls on the mock.
// Pass -1 to skip checking a particular call count.
func assertMockCalls(t *testing.T, env *TestEnv, profile, labels, messages int) {
	t.Helper()
	if profile >= 0 && env.Mock.ProfileCalls != profile {
		t.Errorf("profile calls: got %d, want %d", env.Mock.ProfileCalls, profile)
	}
	if labels >= 0 && env.Mock.LabelsCalls != labels {
		t.Errorf("labels calls: got %d, want %d", env.Mock.LabelsCalls, labels)
	}
	if messages >= 0 && len(env.Mock.GetMessageCalls) != messages {
		t.Errorf("message fetches: got %d, want %d", len(env.Mock.GetMessageCalls), messages)
	}
}

// assertListMessagesCalls verifies the number of ListMessages API calls (pagination).
func assertListMessagesCalls(t *testing.T, env *TestEnv, want int) {
	t.Helper()
	if env.Mock.ListMessagesCalls != want {
		t.Errorf("ListMessages calls: got %d, want %d", env.Mock.ListMessagesCalls, want)
	}
}

// assertMessageCount checks the message count in the store.
func assertMessageCount(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	stats := mustStats(t, st)
	if stats.MessageCount != want {
		t.Errorf("expected %d messages in db, got %d", want, stats.MessageCount)
	}
}

// assertAttachmentCount checks the attachment count in the store.
func assertAttachmentCount(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	stats := mustStats(t, st)
	if stats.AttachmentCount != want {
		t.Errorf("expected %d attachments in db, got %d", want, stats.AttachmentCount)
	}
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
	if err != nil {
		t.Fatalf("InspectRecipientCount(%s, %s): %v", sourceMessageID, recipType, err)
	}
	if count != want {
		t.Errorf("expected %d %s recipients for %s, got %d", want, recipType, sourceMessageID, count)
	}
}

// assertDisplayName checks the display name for a recipient of a message.
func assertDisplayName(t *testing.T, st *store.Store, sourceMessageID, recipType, email, want string) {
	t.Helper()
	displayName, err := st.InspectDisplayName(sourceMessageID, recipType, email)
	if err != nil {
		t.Fatalf("InspectDisplayName(%s, %s, %s): %v", sourceMessageID, recipType, email, err)
	}
	if displayName != want {
		t.Errorf("expected display name %q for %s/%s/%s, got %q", want, sourceMessageID, recipType, email, displayName)
	}
}

// assertDeletedFromSource checks whether a message has deleted_from_source_at set.
func assertDeletedFromSource(t *testing.T, st *store.Store, sourceMessageID string, wantDeleted bool) {
	t.Helper()
	deleted, err := st.InspectDeletedFromSource(sourceMessageID)
	if err != nil {
		t.Fatalf("InspectDeletedFromSource(%s): %v", sourceMessageID, err)
	}
	if wantDeleted && !deleted {
		t.Errorf("%s should have deleted_from_source_at set", sourceMessageID)
	}
	if !wantDeleted && deleted {
		t.Errorf("%s should NOT have deleted_from_source_at set", sourceMessageID)
	}
}

// assertBodyContains checks that a message's body_text contains the given substring.
func assertBodyContains(t *testing.T, st *store.Store, sourceMessageID, substr string) {
	t.Helper()
	bodyText, err := st.InspectBodyText(sourceMessageID)
	if err != nil {
		t.Fatalf("InspectBodyText(%s): %v", sourceMessageID, err)
	}
	if !strings.Contains(bodyText, substr) {
		t.Errorf("expected body of %s to contain %q, got: %s", sourceMessageID, substr, bodyText)
	}
}

// assertRawDataExists checks that raw MIME data exists for a message.
func assertRawDataExists(t *testing.T, st *store.Store, sourceMessageID string) {
	t.Helper()
	exists, err := st.InspectRawDataExists(sourceMessageID)
	if err != nil {
		t.Fatalf("InspectRawDataExists(%s): %v", sourceMessageID, err)
	}
	if !exists {
		t.Errorf("expected raw MIME data to be preserved for %s", sourceMessageID)
	}
}

// assertThreadSourceID checks the source_conversation_id for a message's thread.
func assertThreadSourceID(t *testing.T, st *store.Store, sourceMessageID, wantThreadID string) {
	t.Helper()
	threadSourceID, err := st.InspectThreadSourceID(sourceMessageID)
	if err != nil {
		t.Fatalf("InspectThreadSourceID(%s): %v", sourceMessageID, err)
	}
	if threadSourceID != wantThreadID {
		t.Errorf("expected thread source_conversation_id = %q for %s, got %q", wantThreadID, sourceMessageID, threadSourceID)
	}
}

// assertDateFallback checks that sent_at equals internal_date and contains expected substrings.
func assertDateFallback(t *testing.T, st *store.Store, sourceMessageID, wantDatePart, wantTimePart string) {
	t.Helper()
	sentAt, internalDate, err := st.InspectMessageDates(sourceMessageID)
	if err != nil {
		t.Fatalf("InspectMessageDates(%s): %v", sourceMessageID, err)
	}
	if sentAt == "" {
		t.Errorf("%s: sent_at should not be empty", sourceMessageID)
	}
	if internalDate == "" {
		t.Errorf("%s: internal_date should not be empty", sourceMessageID)
	}
	if sentAt != internalDate {
		t.Errorf("%s: sent_at (%q) should equal internal_date (%q)", sourceMessageID, sentAt, internalDate)
	}
	if !strings.Contains(sentAt, wantDatePart) || !strings.Contains(sentAt, wantTimePart) {
		t.Errorf("%s: sent_at = %q, expected to contain %s %s", sourceMessageID, sentAt, wantDatePart, wantTimePart)
	}
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
	if err != nil {
		t.Fatalf("assertMessageHasLabel(%s, %s): %v", sourceMessageID, sourceLabelID, err)
	}
	if count == 0 {
		t.Errorf("message %s should have label %s", sourceMessageID, sourceLabelID)
	}
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
	if err != nil {
		t.Fatalf("assertMessageNotHasLabel(%s, %s): %v", sourceMessageID, sourceLabelID, err)
	}
	if count != 0 {
		t.Errorf("message %s should NOT have label %s", sourceMessageID, sourceLabelID)
	}
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
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return count
}
