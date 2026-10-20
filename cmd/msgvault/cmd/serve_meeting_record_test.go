package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func meetingRecordAdapterNote(t *testing.T, f calendarEventsFixture) (int64, int64) {
	t.Helper()
	requirements := require.New(t)
	src, err := f.st.GetOrCreateSource("meeting_import", "notes@example.org")
	requirements.NoError(err)
	conv, err := f.st.EnsureConversation(src.ID, "note", "Planning")
	requirements.NoError(err)
	id, err := f.st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "note", MessageType: "meeting_transcript", Subject: sql.NullString{String: "Planning", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(f.st.UpsertMessageRawWithFormat(id, []byte(`{"summary_text":"Notes","transcript":"Recorded evidence"}`), "meeting_json"))
	requirements.NoError(f.st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", f.eventID), id))
	return id, src.ID
}

func TestMeetingRecordAdapterMessageReadAndFollowUp(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, notesSource := meetingRecordAdapterNote(t, f)
	ref := fmt.Sprintf("evt:%d", f.eventID)
	w := calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.sourceID, notesSource}, "/api/v1/meetings/"+ref)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var record store.MeetingRecord
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &record))
	requirements.Len(record.Items, 1)
	assertions.Equal(note, record.Items[0].MessageID)
	w = calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{notesSource}, record.Items[0].FollowUp)
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	w = calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.hiddenSourceID}, "/api/v1/meetings/"+ref)
	assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
	w = calendarEventsAgentRequest(t, f.owner, f.handler, "calendar.read", []int64{f.sourceID}, "/api/v1/meetings/"+ref)
	assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
}

func TestMeetingRecordAdapterHidesNotesAndSuggestions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, notesSource := meetingRecordAdapterNote(t, f)
	ref := fmt.Sprintf("evt:%d", f.eventID)
	w := calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.sourceID}, "/api/v1/meetings/"+ref)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "Recorded evidence")
	assertions.NotContains(w.Body.String(), "notes@example.org")

	w = calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.sourceID}, "/api/v1/meetings/"+ref+"/suggestions")
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var hiddenSuggestions []store.MeetingSuggestion
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &hiddenSuggestions))
	assertions.NotContains(suggestionMemberIDs(hiddenSuggestions), note)

	w = calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.sourceID, notesSource}, "/api/v1/meetings/"+ref+"/suggestions")
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var visibleSuggestions []store.MeetingSuggestion
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &visibleSuggestions))
	assertions.Contains(suggestionMemberIDs(visibleSuggestions), note)
}

func suggestionMemberIDs(suggestions []store.MeetingSuggestion) []int64 {
	ids := make([]int64, 0, len(suggestions))
	for _, suggestion := range suggestions {
		ids = append(ids, suggestion.MemberMessageID)
	}
	return ids
}

func TestMeetingRecordDaemonRoundTrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, _ := meetingRecordAdapterNote(t, f)
	result, err := f.owner.GetMeeting(context.Background(), fmt.Sprintf("evt:%d", f.eventID))
	requirements.NoError(err)
	requirements.Len(result.Items, 1)
	assertions.Equal(note, result.Items[0].MessageID)
	result, err = f.owner.UnlinkMeeting(t.Context(), fmt.Sprintf("evt:%d", f.eventID), note)
	requirements.NoError(err)
	assertions.Empty(result.Items)
	result, err = f.owner.LinkMeeting(t.Context(), fmt.Sprintf("evt:%d", f.eventID), note)
	requirements.NoError(err)
	assertions.Len(result.Items, 1)
}

func TestMeetingRecordInvalidAndMissingSuggestions(t *testing.T) {
	assertions := assert.New(t)
	f := calendarEventsAPIFixture(t)
	for _, tc := range []struct {
		ref    string
		status int
	}{{"msg:0", 400}, {"msg:999999", 404}, {"invalid", 400}} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/meetings/"+tc.ref+"/suggestions", nil)
		req.Header.Set("X-Api-Key", "owner")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		assertions.Equal(tc.status, w.Code, w.Body.String())
	}
}

func TestPersonMeetingsAdapterContactAndParticipantEmails(t *testing.T) {
	for _, emailOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("email-only-%t", emailOnly), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := calendarEventsAPIFixture(t)
			requirements.NoError(f.st.UpsertMessageRawWithFormat(f.eventID, []byte(`{"id":"event","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"attendees":[{"email":"invited@example.com"}]}`), "gcal_json"))
			participant, err := f.st.EnsureParticipant("invited@example.com", "Synthetic Invitee", "example.com")
			requirements.NoError(err)
			person, _, err := f.st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			if emailOnly {
				_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`DELETE FROM person_participants WHERE person_id = ?`), person.ID)
				requirements.NoError(err)
				_, err = f.st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "Invited@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				requirements.NoError(err)
			}
			path := fmt.Sprintf("/api/v1/people/%d/meetings?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z", person.ID)
			w := calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{f.sourceID}, path)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var page struct {
				Items      []store.MeetingRecordSummary `json:"items"`
				NextCursor string                       `json:"next_cursor"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
			requirements.Len(page.Items, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", f.eventID), page.Items[0].Ref)
			assertions.Empty(page.NextCursor)
		})
	}
}

func TestPersonMeetingsAdapterRefFormsAndMissingPerson(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, notesSource := meetingRecordAdapterNote(t, f)
	participant, err := f.st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, note)
	requirements.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	for _, ref := range []string{strconv.FormatInt(person.ID, 10), fmt.Sprintf("person:%d", person.ID), fmt.Sprintf("participant:%d", participant)} {
		w := calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{notesSource}, "/api/v1/people/"+ref+"/meetings?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z")
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), fmt.Sprintf(`"ref":"msg:%d"`, note))
	}
	w := calendarEventsAgentRequest(t, f.owner, f.handler, "message.read", []int64{notesSource}, "/api/v1/people/999999/meetings")
	assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
}

type meetingSnapshotMutationAdapter struct {
	*storeAPIAdapter

	mutate func()
}

func (a *meetingSnapshotMutationAdapter) GetMeetingPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	person, err := a.storeAPIAdapter.GetMeetingPersonContext(ctx, id)
	if err == nil && a.mutate != nil {
		mutate := a.mutate
		a.mutate = nil
		mutate()
	}
	return person, err
}

func TestPersonMeetingsOwnerResolutionAndHistoryShareSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	raw := []byte(`{"id":"event","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	requirements.NoError(f.st.UpsertMessageRawWithFormat(f.eventID, raw, "gcal_json"))
	participant, err := f.st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	adapter := &meetingSnapshotMutationAdapter{storeAPIAdapter: &storeAPIAdapter{store: f.st}, mutate: func() {
		require.NoError(t, f.st.UpsertMessageRawWithFormat(f.eventID, []byte(`{"id":"event","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"attendees":[{"email":"other@example.org"}]}`), "gcal_json"))
	}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	path := fmt.Sprintf("/api/v1/people/%d/meetings?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z", person.ID)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Api-Key", "owner")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), fmt.Sprintf(`"ref":"evt:%d"`, f.eventID))
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Api-Key", "owner")
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), fmt.Sprintf(`"ref":"evt:%d"`, f.eventID))
}
