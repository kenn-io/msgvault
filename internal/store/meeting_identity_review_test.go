package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingIdentityDamagedUIDCopy(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	imported, err := st.PersistICalendarContext(t.Context(), icsSource(t, st), calendarDocument("REQUEST", "DTSTART:20270314T100000Z\r\nDURATION:PT1H"))
	require.NoError(t, err)
	require.Len(t, imported.MessageIDs, 1)
	damaged := imported.MessageIDs[0]
	_, event := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	_, note := identityNote(t, st, "note", "")
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression='' WHERE message_id=?`, []byte("damaged invitation"), damaged)
	require.NoError(t, err)
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(t, err)
	if assert.Len(t, links, 1) {
		assert.Equal(t, identityRef(event), links[0].Ref)
	}
	ref := identityRef(event)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(t, err)
	assert.Equal(t, ref, got.ByOccurrence[ref].OccurrenceRef)
	assert.Equal(t, []int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentityRejectsUnknownICSVersion(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	imported, err := st.PersistICalendarContext(t.Context(), icsSource(t, st), calendarDocument("REQUEST", "DTSTART:20270314T100000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;COUNT=2"))
	require.NoError(t, err)
	require.Len(t, imported.MessageIDs, 1)
	event := imported.MessageIDs[0]
	_, note := identityNote(t, st, "note", "")
	ref := OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, ref))
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, `{"ics_version":999}`, event)
	require.NoError(t, err)
	_, err = st.MeetingLinksContext(t.Context(), note)
	assert.ErrorContains(t, err, "unsupported ICS metadata version 999")
	_, err = st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(ref)}})
	assert.ErrorContains(t, err, "unsupported ICS metadata version 999")
}

func TestMeetingIdentityTranscriptOnlyCompletesGroup(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "group@example.org"))
	_, one := identityNote(t, st, "one", "group@example.org")
	_, two := identityNote(t, st, "two", "")
	require.NoError(t, st.LinkMeetingContext(t.Context(), two, OccurrenceRef{MessageID: event}))
	_, split := identityNote(t, st, "split", "group@example.org")
	require.NoError(t, st.UnlinkMeetingContext(t.Context(), split, OccurrenceRef{MessageID: event}))
	for _, note := range []int64{one, two} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
		require.NoError(t, err)
		assert.Equal(t, []int64{one, two}, got.ByTranscript[note].LinkedTranscriptIDs)
		assert.Equal(t, []int64{one, two}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityCompletesNewManualGroups(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, three := calendarFixture(t, st, "three", singleLinkRaw("three", "three@example.org"))
	_, first := identityNote(t, st, "first", "")
	_, second := identityNote(t, st, "second", "")
	_, deferred := identityNote(t, st, "deferred", "one@example.org")
	for _, link := range []struct{ note, event int64 }{{first, one}, {first, two}, {second, two}, {second, three}, {deferred, three}} {
		require.NoError(t, st.LinkMeetingContext(t.Context(), link.note, OccurrenceRef{MessageID: link.event}))
	}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(one)}})
	require.NoError(t, err)
	assert.Equal(t, []int64{first}, got.ByOccurrence[identityRef(one)].LinkedTranscriptIDs)
	assert.Equal(t, []int64{first, second}, got.ByOccurrence[identityRef(two)].LinkedTranscriptIDs)
	assert.Equal(t, []int64{second, deferred}, got.ByOccurrence[identityRef(three)].LinkedTranscriptIDs)
}

func TestMeetingIdentityCompletesSuggestedGroups(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "series", linkSeriesRaw)
	_, two := calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, ambiguous := identityNote(t, st, "ambiguous", "weekly@example.org")
	_, linked := identityNote(t, st, "linked", "")
	require.NoError(t, st.LinkMeetingContext(t.Context(), linked, OccurrenceRef{MessageID: two}))
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{ambiguous}, IncludeSuggestions: true})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{identityRef(one) + "@20270314T100000Z", identityRef(two)}, got.ByTranscript[ambiguous].SuggestedOccurrenceRefs)
	assert.Equal(t, []int64{linked}, got.ByOccurrence[identityRef(two)].LinkedTranscriptIDs)
}

func TestParticipantsByEmailScopesBeforeAmbiguity(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	visible, err := st.EnsureParticipant("visible@example.com", "Visible", "example.com")
	require.NoError(t, err)
	hidden, err := st.EnsureParticipant("hidden@example.com", "Hidden", "example.com")
	require.NoError(t, err)
	for i, id := range []int64{visible, hidden} {
		_, err = st.db.Exec(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value,is_primary) VALUES (?,?,'Alias@Example.com',FALSE)`, id, []string{"email", "apple_id"}[i])
		require.NoError(t, err)
	}
	data, note := projectionFixture(t, st, "visible-note", "plaud_json", `{}`)
	require.NoError(t, st.ReplaceMessageRecipients(note, "attendee", []int64{visible}, nil))
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
	got, err := st.ParticipantsByEmailContext(ctx, []string{"alias@example.com"})
	require.NoError(t, err)
	assert.Equal(t, map[string]AddressParticipant{"alias@example.com": {ParticipantID: visible}}, got)
	got, err = st.ParticipantsByEmailContext(t.Context(), []string{"alias@example.com"})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParticipantsByEmailBatchReadBound(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	var addresses []string
	want := map[string]AddressParticipant{}
	for i := range 20 {
		address := fmt.Sprintf("attendee-%d@example.com", i)
		id, err := st.EnsureParticipant(address, "Attendee", "example.com")
		require.NoError(t, err)
		addresses = append(addresses, address)
		want[address] = AddressParticipant{ParticipantID: id}
	}
	reads := 0
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(context.Context, string, []driver.NamedValue) error { reads++; return nil }})
	got, err := observed.ParticipantsByEmailContext(t.Context(), addresses)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, reads)
	for i := range 500 {
		addresses = append(addresses, fmt.Sprintf("unknown-%d@example.com", i))
	}
	reads = 0
	got, err = observed.ParticipantsByEmailContext(t.Context(), addresses)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 2, reads)
}

func TestMeetingIdentityHiddenCopiesDoNotConsumeCandidateLimit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "visible", singleLinkRaw("visible", "copies@example.org"))
	noteData, note := identityNote(t, st, "note", "copies@example.org")
	for i := range 201 {
		key := fmt.Sprintf("hidden-%d", i)
		calendarFixture(t, st, key, singleLinkRaw(key, "copies@example.org"), "hidden@example.com")
	}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID, noteData.Message.SourceID})
	got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal(t, identityRef(event), got.ByTranscript[note].OccurrenceRef)
	assert.Equal(t, []int64{note}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
}
