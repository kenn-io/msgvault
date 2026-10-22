package pocket

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type archiveSource struct {
	account Account
	records []Recording
	details map[string]Recording
	fail    map[string]error
	read    []string
	onRead  func(string)
	listErr error
	pages   map[int]Page
}

func (s *archiveSource) CurrentAccount(context.Context) (Account, error) { return s.account, nil }
func (s *archiveSource) ListRecordings(_ context.Context, page int) (Page, error) {
	if s.listErr != nil {
		return Page{}, s.listErr
	}
	if s.pages != nil {
		return s.pages[page], nil
	}
	return Page{Page: page, Total: len(s.records), Recordings: s.records}, nil
}
func (s *archiveSource) Recording(ctx context.Context, id string) (Recording, error) {
	s.read = append(s.read, id)
	if s.onRead != nil {
		s.onRead(id)
	}
	if ctx.Err() != nil {
		return Recording{}, ctx.Err()
	}
	if err := s.fail[id]; err != nil {
		return Recording{}, err
	}
	if rec, ok := s.details[id]; ok {
		return rec, nil
	}
	for _, rec := range s.records {
		if rec.ID == id {
			return rec, nil
		}
	}
	return Recording{}, errors.New("recording not found")
}

func testRecording(t *testing.T, raw string) Recording {
	t.Helper()
	rec, err := DecodeRecording([]byte(raw))
	require.NoError(t, err)
	return rec
}
func testArchive(t *testing.T, records ...Recording) (*Importer, *store.Store, *archiveSource) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source := &archiveSource{account: Account{Email: "owner@example.com", UserID: "user-a"}, records: records, fail: map[string]error{}}
	_, err := RegisterSource(st, "personal", source.account)
	require.NoError(t, err)
	return NewImporter(st, source), st, source
}
func archiveOptions() ImportOptions {
	return ImportOptions{Identifier: "personal", AccountEmail: "owner@example.com"}
}
func archivedContent(t *testing.T, st *store.Store, sourceID int64, id string) (int64, meetingcontent.Content) {
	t.Helper()
	ids, err := st.MessageExistsBatch(sourceID, []string{id})
	require.NoError(t, err)
	require.NotZero(t, ids[id])
	raw, err := st.GetMessageRaw(ids[id])
	require.NoError(t, err)
	return ids[id], meetingcontent.Decode("pocket_json", raw, nil)
}

func TestPocketImportEditsAndProjects(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), first.MeetingsAdded)
	id, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Roadmap decision", content.Summary.Text)
	requirements.Len(content.Actions, 1)
	unchanged, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Zero(unchanged.MeetingsAdded + unchanged.MeetingsUpdated)
	src.records[0] = testRecording(t, strings.ReplaceAll(recordingFixture, "Roadmap decision", "Updated decision"))
	updated, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)
	sameID, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal(id, sameID)
	assertions.Equal("Updated decision", content.Summary.Text)
	ids := []int64{id}
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 1 << 20})
	requirements.NoError(err)
	assertions.Contains(packet.Content, "Updated decision")
	assertions.Contains(packet.Content, "Decide roadmap")
	if st.FTS5Available() {
		_, total, err := st.SearchMessagesContext(t.Context(), "Updated", 0, 10)
		requirements.NoError(err)
		assertions.Equal(int64(1), total)
	}
}

func TestPocketImportPreservesUnavailableAndClearsAuthoritativeEmpty(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	src.records[0] = testRecording(t, `{"id":"rec-a","duration":60,"transcript":[],"transcript_error":"processing failed","summarizations":{"a":{"processingStatus":"processing","v2":{"summary":{"markdown":""},"actionItems":{"actionItems":[]}}}}}`)
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	_, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Roadmap decision", content.Summary.Text)
	requirements.Len(content.Transcript.Segments, 1)
	requirements.Len(content.Actions, 1)
	src.records[0] = testRecording(t, `{"id":"rec-a","transcript":[],"summarizations":{"a":{"processingStatus":"completed","v2":{"summary":{"markdown":"New summary"}}}}}`)
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	_, content = archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("New summary", content.Summary.Text)
	assertions.Equal(meetingcontent.StateEmpty, content.Transcript.State)
	requirements.Len(content.Actions, 1)
	src.records[0] = testRecording(t, `{"id":"rec-a","transcript":[],"summarizations":{"a":{"processingStatus":"completed","v2":{"summary":{"markdown":""},"actionItems":{"actionItems":[]}}}}}`)
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	_, content = archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal(meetingcontent.StateEmpty, content.Summary.State)
	assertions.Empty(content.Actions)
}

func TestPocketImportUpdatesCompletedSectionsWhenTranscriptFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)

	src.records[0] = testRecording(t, `{"id":"rec-a","duration":60,"transcript":{"unexpected":"shape"},"transcript_error":"processing failed","summarizations":{"sum-b":{"processingStatus":"completed","updatedAt":"2026-09-03T10:00:00Z","v2":{"summary":{"markdown":"Updated decision"},"actionItems":{"actionItems":[{"id":"task-b","title":"Send update","status":"TODO","isCompleted":false,"is_completed":false}]}}}}}`)
	updated, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)

	_, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Updated decision", content.Summary.Text)
	requirements.Len(content.Transcript.Segments, 1)
	assertions.Equal("Decide roadmap", content.Transcript.Segments[0].Text)
	requirements.Len(content.Actions, 1)
	assertions.Equal("Send update", content.Actions[0].Title)
}

func TestPocketImportFillsPartialDetailFromListMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	src.details = map[string]Recording{"rec-a": testRecording(t, recordingFixture)}
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	initialID, _ := archivedContent(t, st, first.SourceID, "rec-a")
	initial, err := st.GetMessageContext(t.Context(), initialID)
	requirements.NoError(err)
	assertions.Equal("Planning", initial.Subject)
	assertions.Equal("owner@example.com", initial.FromEmail)

	listMetadata := testRecording(t, `{"id":"rec-a","title":"Updated list title","recording_at":"2026-09-05T10:00:00Z","duration":120,"recorded_by":{"email":"owner@example.com","user_id":"user-a","display_name":"Updated organizer"},"transcript":[]}`)
	src.records[0] = listMetadata
	src.details["rec-a"] = testRecording(t, `{"id":"rec-a","transcript":[]}`)
	opts := archiveOptions()
	opts.StartedAfter = listMetadata.StartedAt.Add(-time.Minute)
	updated, err := imp.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)

	id, content := archivedContent(t, st, first.SourceID, "rec-a")
	message, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Updated list title", message.Subject)
	assertions.True(listMetadata.StartedAt.Equal(message.SentAt))
	assertions.Equal("owner@example.com", message.FromEmail)
	assertions.Equal("Updated organizer", message.FromName)
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(120.0, *content.DurationSeconds, 0)
}

func TestPocketImportPreservesOwnerWhenLaterRecordingOmitsIt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	original := strings.Replace(recordingFixture, `"user_id":"user-a"`, `"user_id":"user-a","display_name":"Organizer One"`, 1)
	imp, st, src := testArchive(t, testRecording(t, original))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	id, _ := archivedContent(t, st, first.SourceID, "rec-a")
	before, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("owner@example.com", before.FromEmail)
	assertions.Equal("Organizer One", before.FromName)
	beforeIsFromMe, err := st.GetMessageIsFromMe(id)
	requirements.NoError(err)
	assertions.True(beforeIsFromMe)
	beforeRecipients := make(map[string][]store.MessageRecipient)
	for _, recipientType := range []string{"from", "to", "cc", "bcc"} {
		beforeRecipients[recipientType], err = st.GetMessageRecipientsContext(t.Context(), id, recipientType)
		requirements.NoError(err)
	}
	requirements.Len(beforeRecipients["from"], 1)
	assertions.Equal("owner@example.com", beforeRecipients["from"][0].EmailAddress)
	assertions.Equal("Organizer One", beforeRecipients["from"][0].DisplayName)

	src.records[0] = testRecording(t, `{"id":"rec-a","recording_at":"2026-09-01T10:00:00Z"}`)
	updated, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)
	afterID, _ := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal(id, afterID)
	after, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(before.FromEmail, after.FromEmail)
	assertions.Equal(before.FromName, after.FromName)
	afterIsFromMe, err := st.GetMessageIsFromMe(id)
	requirements.NoError(err)
	assertions.Equal(beforeIsFromMe, afterIsFromMe)
	for _, recipientType := range []string{"from", "to", "cc", "bcc"} {
		afterRecipients, recipientErr := st.GetMessageRecipientsContext(t.Context(), id, recipientType)
		requirements.NoError(recipientErr)
		assertions.Equal(beforeRecipients[recipientType], afterRecipients)
	}
}

func TestPocketImportUpdatesOrganizerFromListingWhenDetailOmitsIt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	listing := testRecording(t, `{"id":"rec-a","title":"Planning","recording_at":"2026-09-01T10:00:00Z","duration":60,"recorded_by":{"email":"owner@example.com","user_id":"user-a","display_name":"Organizer One"},"transcript":[]}`)
	imp, st, src := testArchive(t, listing)
	src.details = map[string]Recording{"rec-a": testRecording(t, `{"id":"rec-a","transcript":[]}`)}
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	id, _ := archivedContent(t, st, first.SourceID, "rec-a")
	initial, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Organizer One", initial.FromName)

	src.records[0] = testRecording(t, `{"id":"rec-a","title":"Planning","recording_at":"2026-09-01T10:00:00Z","duration":60,"recorded_by":{"email":"owner@example.com","user_id":"user-a","display_name":"Organizer Two"},"transcript":[]}`)
	updated, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)
	current, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Organizer Two", current.FromName)
	fromRecipients, err := st.GetMessageRecipientsContext(t.Context(), id, "from")
	requirements.NoError(err)
	requirements.Len(fromRecipients, 1)
	assertions.Equal("Organizer Two", fromRecipients[0].DisplayName)
}

func TestPocketImportPreservesRecordingDateWhenLaterResponseHasOnlyCreationDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	id, _ := archivedContent(t, st, first.SourceID, "rec-a")
	initial, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	wantStartedAt := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	assertions.True(wantStartedAt.Equal(initial.SentAt))

	src.records[0] = testRecording(t, `{"id":"rec-a","created_at":"2026-09-10T10:00:00Z","transcript":[]}`)
	updated, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsUpdated)
	current, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.True(wantStartedAt.Equal(current.SentAt))
}

func TestPocketImportAfterFilterUsesReconciledStartedAt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	id, _ := archivedContent(t, st, first.SourceID, "rec-a")
	src.read = nil

	src.records[0] = testRecording(t, `{"id":"rec-a","title":"Updated list title","created_at":"2026-09-10T10:00:00Z","transcript":[]}`)
	src.details = map[string]Recording{"rec-a": testRecording(t, `{"id":"rec-a","created_at":"2026-09-10T10:00:00Z","transcript":[],"summarizations":{"sum-b":{"processingStatus":"completed","updatedAt":"2026-09-10T10:00:00Z","v2":{"summary":{"markdown":"Out-of-window update"}}}}}`)}
	opts := archiveOptions()
	opts.StartedAfter = time.Date(2026, time.September, 5, 0, 0, 0, 0, time.UTC)
	updated, err := imp.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.Equal(int64(1), updated.MeetingsProcessed)
	assertions.Zero(updated.MeetingsUpdated)
	assertions.Equal([]string{"rec-a"}, src.read)

	message, err := st.GetMessageContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Planning", message.Subject)
	assertions.True(time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC).Equal(message.SentAt))
	_, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Roadmap decision", content.Summary.Text)
}

func TestPocketImportAfterFilterRecoversDatesFromDetailsAndArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	existing := strings.Replace(recordingFixture, `"recording_at":"2026-09-01T10:00:00Z"`, `"recording_at":"2026-09-06T10:00:00Z"`, 1)
	imp, st, src := testArchive(t, testRecording(t, existing))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	existingID, _ := archivedContent(t, st, first.SourceID, "rec-a")
	src.read = nil
	src.records = []Recording{
		testRecording(t, `{"id":"rec-a","title":"Updated listing title","transcript":[]}`),
		testRecording(t, `{"id":"rec-b","title":"New listing title","transcript":[]}`),
	}
	src.details = map[string]Recording{
		"rec-a": testRecording(t, `{"id":"rec-a","transcript":[],"summarizations":{"new":{"processingStatus":"completed","updatedAt":"2026-09-10T10:00:00Z","v2":{"summary":{"markdown":"Updated archived summary"}}}}}`),
		"rec-b": testRecording(t, `{"id":"rec-b","recording_at":"2026-09-07T10:00:00Z","transcript":[],"summarizations":{"new":{"processingStatus":"completed","updatedAt":"2026-09-10T10:00:00Z","v2":{"summary":{"markdown":"New detail summary"}}}}}`),
	}
	opts := archiveOptions()
	opts.StartedAfter = time.Date(2026, time.September, 5, 0, 0, 0, 0, time.UTC)
	updated, err := imp.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.Equal(int64(2), updated.MeetingsProcessed)
	assertions.Equal(int64(1), updated.MeetingsAdded)
	assertions.Equal(int64(1), updated.MeetingsUpdated)
	assertions.ElementsMatch([]string{"rec-a", "rec-b"}, src.read)

	archived, err := st.GetMessageContext(t.Context(), existingID)
	requirements.NoError(err)
	assertions.True(time.Date(2026, time.September, 6, 10, 0, 0, 0, time.UTC).Equal(archived.SentAt))
	_, existingContent := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Updated archived summary", existingContent.Summary.Text)
	newID, newContent := archivedContent(t, st, first.SourceID, "rec-b")
	requirements.NotZero(newID)
	assertions.Equal("New detail summary", newContent.Summary.Text)
	newMessage, err := st.GetMessageContext(t.Context(), newID)
	requirements.NoError(err)
	assertions.True(time.Date(2026, time.September, 7, 10, 0, 0, 0, time.UTC).Equal(newMessage.SentAt))
}

func TestPocketImportRejectsOtherAccountBeforeWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	src.account.UserID = "user-b"
	src.records[0] = testRecording(t, strings.Replace(recordingFixture, `"user_id":"user-a"`, `"user_id":"user-b"`, 1))
	_, err := imp.Import(t.Context(), archiveOptions())
	requirements.ErrorContains(err, "pocket source identity does not match the authenticated account")
	sources, err := st.ListSources(SourceType)
	requirements.NoError(err)
	requirements.Len(sources, 1)
	rows, err := st.MessageExistsBatch(sources[0].ID, []string{"rec-a"})
	requirements.NoError(err)
	assertions.Empty(rows)
	_, err = RegisterSource(st, "personal", src.account)
	requirements.Error(err)
}

func TestPocketImportRotatesFailedRecordsAcrossSuccessfulRuns(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var records []Recording
	for i := range 3 {
		records = append(records, testRecording(t, fmt.Sprintf(`{"id":"rec-%d","recording_at":"2026-09-%02dT10:00:00Z","transcript":[]}`, i, 3-i)))
	}
	imp, st, src := testArchive(t, records...)
	src.fail["rec-0"] = errors.New("synthetic contract failure")
	opts := archiveOptions()
	opts.Limit = 1
	_, err := imp.Import(t.Context(), opts)
	requirements.Error(err)
	second, err := imp.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.Equal(int64(1), second.MeetingsAdded)
	_, err = imp.Import(t.Context(), opts)
	requirements.NoError(err)
	_, err = imp.Import(t.Context(), opts)
	requirements.Error(err)
	assertions.Equal([]string{"rec-0", "rec-1", "rec-2", "rec-0"}, src.read)
	last, err := st.GetLastSuccessfulSync(second.SourceID)
	requirements.NoError(err)
	assertions.Equal(store.SyncStatusCompleted, last.Status)
	latest, err := st.GetLatestSync(second.SourceID)
	requirements.NoError(err)
	assertions.Equal(store.SyncStatusFailed, latest.Status)
	assertions.NotEqual(last.ID, latest.ID)
}

func TestPocketImportPartialFailurePreservesCommittedDataAndCheckpoint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture), testRecording(t, `{"id":"rec-b","created_at":"2026-08-01T10:00:00Z","transcript":[]}`))
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	last, err := st.GetLastSuccessfulSync(first.SourceID)
	requirements.NoError(err)
	src.records[0] = testRecording(t, strings.ReplaceAll(recordingFixture, "Roadmap decision", "Partial update"))
	src.fail["rec-b"] = errors.New("synthetic failure")
	partial, err := imp.Import(t.Context(), archiveOptions())
	requirements.Error(err)
	assertions.Equal(int64(1), partial.MeetingsUpdated)
	_, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Partial update", content.Summary.Text)
	after, err := st.GetLastSuccessfulSync(first.SourceID)
	requirements.NoError(err)
	assertions.Equal(last.ID, after.ID)
	checkpoint, err := st.GetLatestCheckpointedSyncByType(first.SourceID, SourceType)
	requirements.NoError(err)
	assertions.NotEmpty(checkpoint.CursorBefore.String)
	before := checkpoint.CursorBefore.String
	src.listErr = errors.New("enumeration failed")
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.Error(err)
	checkpoint, err = st.GetLatestCheckpointedSyncByType(first.SourceID, SourceType)
	requirements.NoError(err)
	assertions.JSONEq(before, checkpoint.CursorBefore.String)
}

func TestPocketImportOwnerMetadataAndPagination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture))
	src.records[0] = testRecording(t, strings.ReplaceAll(recordingFixture, "user-a", "user-b"))
	_, err := imp.Import(t.Context(), archiveOptions())
	requirements.Error(err)
	src.records[0] = testRecording(t, recordingFixture)
	src.pages = map[int]Page{1: {Page: 1, Total: 1, HasMore: true, Recordings: src.records}, 2: {Page: 2, Total: 1}}
	first, err := imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	src.pages[2] = Page{Page: 2, Total: 1, Recordings: src.records}
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.Error(err)
	src.pages = nil
	src.records = nil
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.NoError(err)
	_, content := archivedContent(t, st, first.SourceID, "rec-a")
	assertions.Equal("Roadmap decision", content.Summary.Text)
}

func TestPocketEvidenceBudget(t *testing.T) {
	rec := testRecording(t, `{"id":"r","transcript":[]}`)
	_, err := makeSnapshot(1, Account{Email: "owner@example.com", UserID: "u"}, rec, meetingcontent.Content{Summary: meetingcontent.Section{State: meetingcontent.StateAvailable, Text: strings.Repeat("x", maxEvidenceBytes)}})
	require.Error(t, err)
}

func TestPocketEvidenceRejectsUnknownVersion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	rec := testRecording(t, `{"id":"rec-a","transcript":[]}`)
	content, err := Normalize(rec)
	requirements.NoError(err)
	snapshot, err := makeSnapshot(1, Account{Email: "owner@example.com", UserID: "user-a"}, rec, content)
	requirements.NoError(err)
	decoded := meetingcontent.Decode("pocket_json", snapshot.Raw, nil)
	assertions.Equal(meetingcontent.StateEmpty, decoded.Transcript.State)

	var stored evidence
	requirements.NoError(json.Unmarshal(snapshot.Raw, &stored))
	stored.Version = 2
	futureVersion, err := json.Marshal(stored, json.Deterministic(true))
	requirements.NoError(err)
	future := meetingcontent.Decode("pocket_json", futureVersion, nil)
	assertions.Equal(meetingcontent.StateUnavailable, future.Summary.State)
	assertions.Equal("invalid_raw", future.Summary.Reason)
}

func TestPocketImportCancellationAndStateRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	imp, st, src := testArchive(t, testRecording(t, recordingFixture), testRecording(t, `{"id":"rec-b","created_at":"2026-08-01T10:00:00Z","transcript":[]}`))
	ctx, cancel := context.WithCancel(t.Context())
	src.onRead = func(id string) {
		if id == "rec-b" {
			cancel()
		}
	}
	summary, err := imp.Import(ctx, archiveOptions())
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(int64(1), summary.MeetingsAdded)
	run, err := st.GetLatestSync(summary.SourceID)
	requirements.NoError(err)
	assertions.Equal(store.SyncStatusFailed, run.Status)
	assertions.Equal(int64(2), run.MessagesProcessed)
	assertions.NotEmpty(run.CursorBefore.String)
	_, content := archivedContent(t, st, summary.SourceID, "rec-a")
	assertions.Equal("Roadmap decision", content.Summary.Text)
	src.onRead = nil
	state, err := imp.loadState(summary.SourceID, false)
	requirements.NoError(err)
	assertions.Equal(int64(2), state.Sequence)
	runID, err := st.StartSync(summary.SourceID, SourceType)
	requirements.NoError(err)
	requirements.NoError(st.FailSyncWithCheckpoint(runID, "synthetic invalid state", &store.Checkpoint{PageToken: `{"schema_version":9}`}))
	_, err = imp.Import(t.Context(), archiveOptions())
	requirements.ErrorContains(err, "--full")
	opts := archiveOptions()
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	requirements.NoError(err)
}

func TestPocketImportFullFilteredTraversalKeepsFairness(t *testing.T) {
	var records []Recording
	for n := range 3 {
		records = append(records, testRecording(t, fmt.Sprintf(`{"id":"r-%d","created_at":"2026-09-%02dT10:00:00Z","transcript":[]}`, n, 3-n)))
	}
	imp, _, src := testArchive(t, records...)
	opts := archiveOptions()
	opts.Full = true
	opts.Limit = 1
	opts.StartedAfter = records[2].StartedAt
	for range 3 {
		_, err := imp.Import(t.Context(), opts)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"r-0", "r-1", "r-2"}, src.read)
}

func TestPocketImportDoesNotInventOrganizer(t *testing.T) {
	imp, st, _ := testArchive(t, testRecording(t, `{"id":"r","transcript":[{"speaker":"Speaker 1","text":"Hello","start":0}]}`))
	sum, err := imp.Import(t.Context(), archiveOptions())
	require.NoError(t, err)
	id, _ := archivedContent(t, st, sum.SourceID, "r")
	for _, recipientType := range []string{"from", "to", "cc", "bcc"} {
		recipients, err := st.GetMessageRecipientsContext(t.Context(), id, recipientType)
		require.NoError(t, err)
		assert.Empty(t, recipients, "recipient type %s", recipientType)
	}
}
