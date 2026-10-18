package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func recordNote(t *testing.T, st *Store, key, raw, at string) (int64, int64) {
	t.Helper()
	requirements := require.New(t)
	data, id := projectionFixture(t, st, key, "meeting_json", raw)
	parsed, err := time.Parse(time.RFC3339, at)
	requirements.NoError(err)
	data.Message.SentAt = sql.NullTime{Time: parsed, Valid: true}
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	return id, data.Message.SourceID
}

func TestMeetingRecordGroupsItemsActionsRooms(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", `{"id":"event","iCalUID":"planning@example.org","location":"Room A","description":"Agenda","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com","responseStatus":"accepted"},{"email":"room@example.org","resource":true}]}`)
	note, _ := recordNote(t, st, "notes", meetingProjectionRaw, "2027-03-14T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), note))
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("evt:%d", event))
	requirements.NoError(err)
	requirements.NotNil(record.Occurrence)
	requirements.Len(record.Invited, 1)
	requirements.Len(record.Rooms, 1)
	assertions.Equal("guest@example.com", record.Invited[0].Email)
	assertions.Equal("room@example.org", record.Rooms[0].Email)
	requirements.Len(record.Items, 1)
	assertions.Equal(note, record.Items[0].MessageID)
	assertions.Equal("user", record.Items[0].Origin)
	requirements.NotNil(record.Items[0].Content)
	requirements.Len(record.ActionItems, 1)
	assertions.Equal("Send brief", record.ActionItems[0].Action.Title)
	assertions.Equal(note, record.ActionItems[0].Meeting.MessageID)
	requirements.NotNil(record.EventDetail)
	assertions.Equal("Agenda", record.EventDetail.Description)
	assertions.Equal("Room A", record.EventDetail.Location)
}

func TestMeetingRecordTranscriptOnlyMsgRef(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note, _ := recordNote(t, st, "standalone", `{"summary_text":"Standalone summary","transcript":"Recorded evidence"}`, "2027-03-14T10:00:00Z")
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note))
	requirements.NoError(err)
	assertions.Nil(record.Occurrence)
	assertions.Equal(fmt.Sprintf("msg:%d", note), record.Ref)
	requirements.Len(record.Items, 1)
	requirements.NotNil(record.Items[0].Content)
	requirements.Len(record.Items[0].Content.Meetings, 1)
	assertions.Equal("Recorded evidence", record.Items[0].Content.Meetings[0].Content.Transcript.Text)
	assertions.Equal(fmt.Sprintf("/api/v1/messages/%d", note), record.Items[0].FollowUp)
}

func TestMeetingRecordAnchorOutOfScopeNotFound(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("event", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note, _ := recordNote(t, st, "note", meetingProjectionRaw, "2027-03-14T10:00:00Z")
	for _, ref := range []string{fmt.Sprintf("evt:%d", event), fmt.Sprintf("msg:%d", note), "msg:99999"} {
		_, err := st.GetMeetingRecordContext(t.Context(), SourceScope{}, ref)
		requirements.ErrorIs(err, ErrNotFound)
	}
	for _, ref := range []string{"wrong", "msg:0", "evt:-1", "msg:1@20270314T100000Z"} {
		_, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
		requirements.ErrorIs(err, ErrInvalidArgument)
	}
}

func TestMeetingRecordScopeHidesAllDerivedFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, source := calendarLinkFixture(t, st, "calendar@example.com", linkSeriesRaw)
	ref := FormatOccurrenceRef(OccurrenceRef{MessageID: event, Key: "20270314T100000Z"})
	scope := SourceScope{IDs: []int64{source}}
	before, err := st.GetMeetingRecordContext(t.Context(), scope, ref)
	requirements.NoError(err)
	hidden, _ := recordNote(t, st, "hidden", `{"summary_text":"Hidden summary","attendees":[{"email":"hidden@example.org"}],"action_items":[{"title":"Hidden action"}]}`, "2027-03-14T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), ref, hidden))
	older, _ := recordNote(t, st, "older", meetingProjectionRaw, "2027-03-07T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), FormatOccurrenceRef(OccurrenceRef{MessageID: event, Key: "20270307T100000Z"}), older))
	after, err := st.GetMeetingRecordContext(t.Context(), scope, ref)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestMeetingRecordUnionsVisibleCopies(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	a, sourceA := calendarLinkFixture(t, st, "calendar-a@example.com", linkSeriesRaw)
	b, _ := calendarLinkFixture(t, st, "calendar-b@example.org", linkSeriesRaw)
	note, notesSource := recordNote(t, st, "copy-note", meetingProjectionRaw, "2027-03-14T10:00:00Z")
	refA := FormatOccurrenceRef(OccurrenceRef{MessageID: a, Key: "20270314T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), FormatOccurrenceRef(OccurrenceRef{MessageID: b, Key: "20270314T100000Z"}), note))
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), refA)
	requirements.NoError(err)
	requirements.Len(record.Items, 1)
	assertions.Equal(note, record.Items[0].MessageID)
	record, err = st.GetMeetingRecordContext(t.Context(), SourceScope{IDs: []int64{sourceA, notesSource}}, refA)
	requirements.NoError(err)
	assertions.Empty(record.Items)
}

func TestMeetingRecordFindsVisibleCopyWithEarlierRDate(t *testing.T) {
	for _, tc := range []struct {
		name, bounds, rdate, key string
	}{
		{
			name:   "timed",
			bounds: `"start":{"dateTime":"2027-06-01T10:00:00Z"},"end":{"dateTime":"2027-06-01T11:00:00Z"}`,
			rdate:  `"RRULE:FREQ=DAILY;COUNT=1","RDATE:20270314T100000Z"`,
			key:    "20270314T100000Z",
		},
		{
			name:   "all-day",
			bounds: `"start":{"date":"2027-06-01"},"end":{"date":"2027-06-02"}`,
			rdate:  `"RRULE:FREQ=DAILY;COUNT=1","RDATE;VALUE=DATE:20270314"`,
			key:    "20270314",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			masterRaw := `{"id":"rdate-copy-a","iCalUID":"rdate-copies@example.org",` + tc.bounds + `,"recurrence":[` + tc.rdate + `]}`
			a, _ := calendarLinkFixture(t, st, "rdate-a@example.org", masterRaw)
			copyRaw := strings.Replace(masterRaw, "rdate-copy-a", "rdate-copy-b", 1)
			b, _ := calendarLinkFixture(t, st, "rdate-b@example.org", copyRaw)
			note, _ := recordNote(t, st, "rdate-note", meetingProjectionRaw, "2027-03-14T10:00:00Z")
			copyRef := FormatOccurrenceRef(OccurrenceRef{MessageID: b, Key: tc.key})
			requirements.NoError(st.LinkMeetingContext(t.Context(), copyRef, note))

			targetRef := FormatOccurrenceRef(OccurrenceRef{MessageID: a, Key: tc.key})
			record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), targetRef)
			requirements.NoError(err)
			requirements.Len(record.Items, 1)
			assertions.Equal(note, record.Items[0].MessageID)
		})
	}
}

func TestMeetingRecordCopyLimitIgnoresOtherEditedOccurrences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	masterRaw := `{"id":"series-a","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=400"]}`
	master, source := calendarLinkFixture(t, st, "calendar-a@example.org", masterRaw)
	copyRaw := strings.Replace(masterRaw, "series-a", "series-copy", 1)
	calendarLinkFixture(t, st, "calendar-b@example.org", copyRaw)
	for i := range 200 {
		original := time.Date(2027, time.April, 1+i, 10, 0, 0, 0, time.UTC)
		moved := original.Add(2 * time.Hour)
		raw := fmt.Sprintf(`{"id":"exception-%d","recurringEventId":"series-a","iCalUID":"shared@example.org","originalStartTime":{"dateTime":%q},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, i, original.Format(time.RFC3339), moved.Format(time.RFC3339), moved.Add(time.Hour).Format(time.RFC3339))
		data := &MessagePersistData{
			Message:      &Message{SourceID: source, SourceMessageID: fmt.Sprintf("exception-%d", i), MessageType: meetingLinkCalendarType, Subject: sql.NullString{String: "Project planning", Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: fmt.Sprintf("exception-%d", i), ConversationType: meetingLinkCalendarType, Title: "Project planning"},
			RawMIME:      []byte(raw),
			RawFormat:    "gcal_json",
			Metadata:     &sql.NullString{String: `{"calendar_id":"calendar-a@example.org"}`, Valid: true},
		}
		_, err := st.PersistMessage(data)
		requirements.NoError(err)
	}
	ref := FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270315T100000Z"})
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
	requirements.NoError(err)
	assertions.Empty(record.Items, "unrelated edited occurrences must be filtered before the copy candidate cap")
}

func TestOwnerRSVPFromOwnerIdentitiesNotCopySelf(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, source := calendarLinkFixture(t, st, "other-owner@example.com", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"other-owner@example.com","self":true,"responseStatus":"declined"},{"email":"alias@example.org","responseStatus":"accepted"}]}`)
	requirements.NoError(st.AddAccountIdentity(source, "alias@example.org", "user"))
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("evt:%d", event))
	requirements.NoError(err)
	assertions.Equal("accepted", record.OwnerRSVP)
	identities, err := st.OwnerIdentitiesContext(t.Context(), source)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.False(identities[0].Primary)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"account_email":"primary@example.com"}`))
	identities, err = st.OwnerIdentitiesContext(t.Context(), source)
	requirements.NoError(err)
	requirements.Len(identities, 2)
	assertions.Contains(identities, OwnerIdentity{Address: "primary@example.com", Primary: true})
}

func TestParticipantsProvenanceGranolaInviteeFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, note := projectionFixture(t, st, "invitee", "granola_json", `{"calendar_event":{"invitees":[{"email":"invited@example.com","name":"Invited Guest"}]}}`)
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note))
	requirements.NoError(err)
	requirements.Len(record.Participants, 1)
	assertions.Equal("invited", record.Participants[0].Provenance)
	assertions.Equal("invited@example.com", record.Participants[0].Email)
}

func TestRecordingsAsAttachments(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note, _ := recordNote(t, st, "recording", meetingProjectionRaw, "2027-03-14T10:00:00Z")
	_, err := st.db.Exec(`INSERT INTO attachments(message_id,mime_type,size,storage_path,duration_ms) VALUES(?,?,?,?,?)`, note, "audio/mpeg", 1234, "synthetic-recording", 1500)
	requirements.NoError(err)
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note))
	requirements.NoError(err)
	requirements.Len(record.Items, 1)
	requirements.Len(record.Items[0].Recordings, 1)
	recording := record.Items[0].Recordings[0]
	assertions.Positive(recording.AttachmentID)
	assertions.Equal("audio/mpeg", recording.MimeType)
	assertions.Equal(int64(1234), recording.Size)
	requirements.NotNil(recording.DurationSeconds)
	assertions.InDelta(1.5, *recording.DurationSeconds, 1e-9)
}

func TestMeetingRecordContentBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("event", "budget@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	ref := fmt.Sprintf("evt:%d", event)
	raw := fmt.Sprintf(`{"summary_text":%q,"transcript":%q}`, strings.Repeat("summary ", 3000), strings.Repeat("transcript ", 30000))
	for i := range 12 {
		note, _ := recordNote(t, st, fmt.Sprintf("note-%d", i), raw, "2027-03-14T10:00:00Z")
		requirements.NoError(st.LinkMeetingContext(t.Context(), ref, note))
	}
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
	requirements.NoError(err)
	requirements.Len(record.Items, 10)
	full := 0
	for _, item := range record.Items {
		if !item.ContentOmitted {
			full++
		}
		requirements.NotNil(item.Content)
		encoded, err := json.Marshal(item.Content)
		requirements.NoError(err)
		if item.ContentOmitted {
			assertions.LessOrEqual(len(encoded), 16384)
		} else {
			assertions.LessOrEqual(len(encoded), 131072)
		}
	}
	assertions.Equal(3, full)
	encoded, err := json.Marshal(record)
	requirements.NoError(err)
	assertions.LessOrEqual(len(encoded), 1048576)
}

func TestMeetingRecordPreviousLongGapAndOpenActions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"long-series","iCalUID":"long@example.org","start":{"dateTime":"2026-06-07T10:00:00Z"},"end":{"dateTime":"2026-06-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY"]}`
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", raw)
	old, _ := recordNote(t, st, "old-note", `{"summary_text":"Old summary","action_items":[{"title":"Open action","status":"open"},{"title":"Done action","status":"done"}]}`, "2026-06-07T10:00:00Z")
	oldRef := FormatOccurrenceRef(OccurrenceRef{MessageID: event, Key: "20260607T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), oldRef, old))
	ref := FormatOccurrenceRef(OccurrenceRef{MessageID: event, Key: "20270314T100000Z"})
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
	requirements.NoError(err)
	requirements.NotNil(record.Previous)
	assertions.Equal(oldRef, record.Previous.Ref)
	assertions.Equal(old, record.Previous.Item.MessageID)
	requirements.Len(record.Previous.OpenActions, 1)
	assertions.Equal("Open action", record.Previous.OpenActions[0].Action.Title)
	assertions.Equal(old, record.Previous.OpenActions[0].Meeting.MessageID)
}

func TestMeetingRecordSnapshotStable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note, _ := recordNote(t, st, "snapshot", `{"summary_text":"Before"}`, "2027-03-14T10:00:00Z")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	ref := fmt.Sprintf("msg:%d", note)
	before, err := st.GetMeetingRecordContext(ctx, OwnerScope(), ref)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(`{"summary_text":"After"}`), "meeting_json"))
	after, err := st.GetMeetingRecordContext(ctx, OwnerScope(), ref)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestMeetingRecordStructuralEvidenceBound(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	attendees := make([]map[string]any, 501)
	for i := range attendees {
		attendees[i] = map[string]any{"email": fmt.Sprintf("guest%d@example.com", i)}
	}
	raw, err := json.Marshal(map[string]any{"id": "large", "start": map[string]string{"dateTime": "2027-03-14T10:00:00Z"}, "end": map[string]string{"dateTime": "2027-03-14T11:00:00Z"}, "attendees": attendees})
	requirements.NoError(err)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", string(raw))
	_, err = st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("evt:%d", event))
	assertions.ErrorIs(err, ErrQueryTooBroad)
}

func TestMeetingPersonHelpersUseReadSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	participant, err := st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	first, err := st.GetMeetingPersonContext(ctx, person.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{participant}, first.ParticipantIDs)
	emails, err := st.MeetingParticipantEmailsContext(ctx, first.ParticipantIDs)
	requirements.NoError(err)
	assertions.Equal([]string{"guest@example.com"}, emails)
	_, err = st.db.Exec(`UPDATE participants SET email_address = ? WHERE id = ?`, "changed@example.org", participant)
	requirements.NoError(err)
	emails, err = st.MeetingParticipantEmailsContext(ctx, first.ParticipantIDs)
	requirements.NoError(err)
	assertions.Equal([]string{"guest@example.com"}, emails)
}

func TestMeetingRecordPreviousCandidatesExcludeHiddenEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, source := calendarLinkFixture(t, st, "calendar@example.com", linkSeriesRaw)
	_, note := projectionFixture(t, st, "hidden-event-note", "granola_json", `{"calendar_event":{"calendar_event_id":"private-event"}}`)
	var notesSource int64
	requirements.NoError(st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&notesSource))
	calendarLinkFixture(t, st, "hidden-calendar@example.org", singleLinkRaw("private-event", "weekly@example.org", "2027-03-07T10:00:00Z", "2027-03-07T11:00:00Z"))
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	scope := SourceScope{IDs: []int64{source, notesSource}}
	budget := 500000
	target, err := resolveLinkOccurrence(ctx, st.snapshotReader(ctx), scope, OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	candidates, err := st.previousMeetingCandidates(ctx, st.snapshotReader(ctx), scope, target)
	requirements.NoError(err)
	assertions.Empty(candidates)
}

func TestMeetingRecordEventDetailUsesEffectiveException(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	master, source := calendarLinkFixture(t, st, "calendar@example.com", `{"id":"weekly","iCalUID":"weekly@example.org","location":"Original room","description":"Original description","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, sameSource := calendarLinkFixture(t, st, "calendar@example.com", `{"id":"moved","recurringEventId":"weekly","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-08T10:00:00Z"},"start":{"dateTime":"2027-03-09T12:00:00Z"},"end":{"dateTime":"2027-03-09T13:00:00Z"},"location":"New room","description":"Updated description"}`)
	assertions.Equal(source, sameSource)
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), fmt.Sprintf("evt:%d@20270308T100000Z", master))
	requirements.NoError(err)
	requirements.NotNil(record.EventDetail)
	assertions.Equal("New room", record.EventDetail.Location)
	assertions.Equal("Updated description", record.EventDetail.Description)
}

func TestMeetingRecordPreviousCandidatesIgnoreHiddenUserLinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	current, calendarSource := calendarLinkFixture(t, st, "visible@example.com", linkSeriesRaw)
	hidden, _ := calendarLinkFixture(t, st, "hidden@example.org", linkSeriesRaw)
	note, notesSource := recordNote(t, st, "previous-hidden-decision", `{"transcript":"Permitted note"}`, "2027-03-07T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d@20270307T100000Z", hidden), note))
	scope := SourceScope{IDs: []int64{calendarSource, notesSource}}
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	target, err := resolveLinkOccurrence(ctx, st.snapshotReader(ctx), scope, OccurrenceRef{MessageID: current, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	// Hidden target decisions must be excluded before the candidate cap, even
	// when their stored logical UID matches a visible series.
	candidates, err := st.previousMeetingCandidates(ctx, st.snapshotReader(ctx), scope, target)
	requirements.NoError(err)
	assertions.Empty(candidates)
}

func TestMeetingRecordReferenceRequiresSafeID(t *testing.T) {
	assertions := assert.New(t)
	for _, ref := range []string{"msg:9007199254740992", "evt:9007199254740992"} {
		assertions.ErrorIs(ValidateMeetingRecordRef(ref), ErrInvalidArgument)
	}
}

func TestMeetingRecordUnionsOrphanExceptionWithVisibleMaster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	orphan, orphanSource := calendarLinkFixture(t, st, "orphan@example.com", `{"id":"orphan","recurringEventId":"missing-master","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	master, _ := calendarLinkFixture(t, st, "master@example.org", linkSeriesRaw)
	note, notesSource := recordNote(t, st, "orphan-copy-note", meetingProjectionRaw, "2027-03-14T10:00:00Z")
	masterRef := FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), masterRef, note))
	for _, ref := range []string{fmt.Sprintf("evt:%d", orphan), masterRef} {
		record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
		requirements.NoError(err)
		requirements.Len(record.Items, 1)
		assertions.Equal(note, record.Items[0].MessageID)
	}
	record, err := st.GetMeetingRecordContext(t.Context(), SourceScope{IDs: []int64{orphanSource, notesSource}}, fmt.Sprintf("evt:%d", orphan))
	requirements.NoError(err)
	assertions.Empty(record.Items)
}

func TestMeetingRecordPreviousExcludesSameLogicalMovedCopy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	moved, _ := calendarLinkFixture(t, st, "moved@example.com", `{"id":"moved-copy","recurringEventId":"missing-master","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T10:00:00Z"},"end":{"dateTime":"2027-03-15T11:00:00Z"}}`)
	master, _ := calendarLinkFixture(t, st, "stale-copy@example.org", linkSeriesRaw)
	current, _ := recordNote(t, st, "current-moved-copy", `{"transcript":"Current occurrence evidence"}`, "2027-03-14T10:00:00Z")
	currentRef := FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), currentRef, current))
	ref := fmt.Sprintf("evt:%d", moved)
	record, err := st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
	requirements.NoError(err)
	assertions.Nil(record.Previous)
	old, _ := recordNote(t, st, "prior-distinct-copy", `{"summary_text":"Prior occurrence evidence"}`, "2027-03-07T10:00:00Z")
	oldRef := FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270307T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), oldRef, old))
	record, err = st.GetMeetingRecordContext(t.Context(), OwnerScope(), ref)
	requirements.NoError(err)
	requirements.NotNil(record.Previous)
	assertions.Equal(oldRef, record.Previous.Ref)
	assertions.Equal(old, record.Previous.Item.MessageID)
}
