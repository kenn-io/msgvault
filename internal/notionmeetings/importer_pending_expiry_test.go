package notionmeetings

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cases enter real importer/Store state through a successful archive,
// then take the header paths that bypass matched hydration. Provider data is
// synthetic; the clock advances to an observable retry deadline, without sleeps.
func TestImporterExpiresTranscriptPendingBeforeEarlyReturn(t *testing.T) {
	for _, outcome := range []string{"missing", "forbidden", "malformed", "not_selected", "trash", "archived", "matched_control"} {
		t.Run(outcome, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			meeting := hydrationMeeting()
			meeting.MeetingNotes.Children.TranscriptBlockID = ""
			installMeetingHeader(t, source.fakeHydrationSource, meeting)
			source.query.Results = []MeetingNote{meeting}
			noSearch := false
			opts := ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch}
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			require.Equal(int64(1), first.MeetingsAdded)
			before, err := imp.loadState(t.Context(), first.SourceID)
			require.NoError(err)
			require.Len(before.Pending, 1)
			deadline := before.Pending[0].RetryUntil
			clock, err := time.Parse(time.RFC3339, deadline)
			require.NoError(err)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				"SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), first.SourceID, meeting.ID).Scan(&messageID))
			oldRaw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			oldBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)

			source.query.Results = nil
			switch outcome {
			case "missing":
				source.blockErrs = map[string]error{meeting.ID: &APIError{Kind: ErrProvider, Status: 404}}
			case "forbidden":
				source.blockErrs = map[string]error{meeting.ID: &APIError{Kind: ErrReadContent, Status: 403}}
			case "malformed":
				source.blocks[meeting.ID] = &Block{Raw: json.RawMessage(`{"object":"block","id":"different-meeting","type":"meeting_notes"}`)}
			case "not_selected":
				meeting.CreatedBy.ID = "other-human"
				meeting.MeetingNotes.CalendarEvent.Attendees = nil
				installMeetingHeader(t, source.fakeHydrationSource, meeting)
			case "trash":
				meeting.InTrash = true
				installMeetingHeader(t, source.fakeHydrationSource, meeting)
			case "archived":
				meeting.Archived = true
				installMeetingHeader(t, source.fakeHydrationSource, meeting)
			}
			next := NewImporter(st, source)
			next.now = func() time.Time { return clock }
			summary, err := next.Import(t.Context(), opts)
			require.NoError(err)
			after, err := NewImporter(st, source).loadState(t.Context(), first.SourceID)
			require.NoError(err)
			assert.Empty(after.Pending, "expiry must not require a successful matched header and hydration")
			// Retaining the original deadline prevents a missing/unknown end from
			// silently starting another retry window on a later healthy fetch.
			assert.Equal(deadline, after.TranscriptRetryUntil[meeting.ID])
			require.Contains(after.Known, meeting.ID)
			assert.Equal(before.Known[meeting.ID].SnapshotSHA256, after.Known[meeting.ID].SnapshotSHA256)
			if outcome != "matched_control" {
				assert.Equal(before.Known[meeting.ID].LastSuccessAt, after.Known[meeting.ID].LastSuccessAt)
			}
			assert.Zero(summary.MeetingsAdded)
			assert.Zero(summary.MeetingsUpdated)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				"SELECT COUNT(*) FROM messages WHERE source_id=?"), first.SourceID).Scan(&count))
			assert.Equal(1, count)
			actualRaw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			assert.Equal(oldRaw, actualRaw)
			actualBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			assert.Equal(oldBody, actualBody)
		})
	}
}

func TestImporterDefersAbandonedTranscriptRetryWithinWindow(t *testing.T) {
	for _, outcome := range []string{"not_selected", "trash", "archived", "fetch_error_control"} {
		t.Run(outcome, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			meeting := hydrationMeeting()
			meeting.MeetingNotes.Children.TranscriptBlockID = ""
			installMeetingHeader(t, source.fakeHydrationSource, meeting)
			source.query.Results = []MeetingNote{meeting}
			noSearch := false
			opts := ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch}
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			before, err := imp.loadState(t.Context(), first.SourceID)
			require.NoError(err)
			require.Len(before.Pending, 1)
			clock, err := time.Parse(time.RFC3339, before.Pending[0].NextAttemptAt)
			require.NoError(err)
			deadline, err := time.Parse(time.RFC3339, before.Pending[0].RetryUntil)
			require.NoError(err)
			source.query.Results = nil
			switch outcome {
			case "not_selected":
				meeting.CreatedBy.ID = "other-human"
				meeting.MeetingNotes.CalendarEvent.Attendees = nil
			case "trash":
				meeting.InTrash = true
			case "archived":
				meeting.Archived = true
			case "fetch_error_control":
				source.blockErrs = map[string]error{meeting.ID: &APIError{Kind: ErrProvider, Status: 404}}
			}
			installMeetingHeader(t, source.fakeHydrationSource, meeting)
			next := NewImporter(st, source)
			next.now = func() time.Time { return clock }
			_, err = next.Import(t.Context(), opts)
			require.NoError(err)
			after, err := next.loadState(t.Context(), first.SourceID)
			require.NoError(err)
			require.Contains(after.Known, meeting.ID)
			assert.Equal(before.Known[meeting.ID].LastSuccessAt, after.Known[meeting.ID].LastSuccessAt)
			assert.Equal(before.Pending[0].RetryUntil, after.TranscriptRetryUntil[meeting.ID])
			if len(after.Pending) > 0 {
				require.Len(after.Pending, 1)
				due, err := time.Parse(time.RFC3339, after.Pending[0].NextAttemptAt)
				require.NoError(err)
				assert.True(due.After(clock), "retained pending work must respect its retry cadence")
				assert.False(due.After(deadline), "deferral must not extend the retry window")
			}
		})
	}
}

func TestDuePendingSelectionIsIndependentOfKnownRefresh(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	state := newSyncState()
	for i := range 40 {
		state.Known[fmt.Sprintf("known-%02d", i)] = knownMeeting{}
	}
	state.Known["pending-z"] = knownMeeting{}
	state.Pending = []pendingTranscript{
		{BlockID: "pending-z", NextAttemptAt: now.Add(-time.Hour).Format(time.RFC3339), RetryUntil: now.Add(time.Hour).Format(time.RFC3339)},
		{BlockID: "later", NextAttemptAt: now.Add(time.Hour).Format(time.RFC3339), RetryUntil: now.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	assert.NotContains(t, state.refreshIDs(32), "pending-z")
	assert.Equal(t, []string{"pending-z"}, state.duePendingIDs(now, 32))
}

func TestImporterMigratesLegacyPendingOverflow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	source.query.Results = nil
	now := imp.now()
	state := newSyncState()
	state.Version = 1
	deadline := now.Add(24 * time.Hour).Format(time.RFC3339)
	for i := range 205 {
		id := fmt.Sprintf("legacy-%03d", i)
		state.Known[id] = knownMeeting{}
		state.Pending = append(state.Pending, pendingTranscript{BlockID: id, NextAttemptAt: now.Add(time.Hour).Format(time.RFC3339), RetryUntil: deadline})
	}
	cursor, err := json.Marshal(state)
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	run, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), run, string(cursor)))
	noSearch := false
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch})
	require.NoError(err)
	recovered, err := imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	assert.Len(recovered.Pending, 200)
	assert.Len(recovered.PendingBacklog, 5)
	assert.Len(recovered.Known, 205)
	assert.Len(recovered.TranscriptRetryUntil, 205)
	assert.Equal(deadline, recovered.TranscriptRetryUntil["legacy-204"])
	assert.Equal(syncStateVersion, recovered.Version)
	imp.now = func() time.Time { return now.Add(time.Hour) }
	opts := ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	recovered, err = imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	require.Len(recovered.PendingBacklog, 5)
	assert.NotEqual("legacy-204", recovered.PendingBacklog[4].BlockID)
	assert.Equal(deadline, recovered.TranscriptRetryUntil["legacy-204"])
}

func TestBacklogRetryMovesToActiveWithoutDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	state := newSyncState()
	deadline := now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	state.PendingBacklog = []pendingTranscript{{BlockID: "meeting-1", NextAttemptAt: now.Format(time.RFC3339), RetryUntil: deadline}}
	state.initialize()
	state.updateTranscriptPending("meeting-1", &HydratedMeeting{Discovery: hydrationMeeting()}, true, now)
	require.Len(state.Pending, 1)
	assert.Empty(state.PendingBacklog)
	cursor, err := state.marshal()
	require.NoError(err)
	var recovered syncState
	require.NoError(json.Unmarshal([]byte(cursor), &recovered))
	require.NoError(recovered.validate())
	assert.Equal(deadline, recovered.Pending[0].RetryUntil)
}

type interruptFirstRetrySource struct {
	*discoveryFixture
}

func (s *interruptFirstRetrySource) RetrieveBlock(ctx context.Context, id string) (*Block, error) {
	if id == "retry-a" {
		return nil, ErrRequestBudget
	}
	return s.discoveryFixture.RetrieveBlock(ctx, id)
}

func TestInterruptedDueRetryDoesNotBlockNextMeeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := broadFixture(t)
	source.query.Results = nil
	meeting := hydrationMeeting()
	meeting.ID = "retry-b"
	meeting.MeetingNotes.Children.TranscriptBlockID = ""
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	imp.client = &interruptFirstRetrySource{discoveryFixture: source}
	now := imp.now()
	state := newSyncState()
	deadline := now.Add(24 * time.Hour).Format(time.RFC3339)
	for _, id := range []string{"retry-a", "retry-b"} {
		state.Known[id] = knownMeeting{}
		state.Pending = append(state.Pending, pendingTranscript{BlockID: id, NextAttemptAt: now.Format(time.RFC3339), RetryUntil: deadline})
	}
	state.initialize()
	cursor, err := state.marshal()
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	run, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), run, cursor))
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	first, err := imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	assert.Equal(deadline, first.TranscriptRetryUntil["retry-a"])
	assert.False(pendingDue(first.Pending[0], now))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages WHERE source_message_id = ?", meeting.ID).Scan(&count))
	assert.Equal(1, count)
}
