package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func identityNote(t *testing.T, st *Store, key, uid string) (*MessagePersistData, int64) {
	t.Helper()
	return projectionFixture(t, st, key, "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid))
}

func identityRef(id int64) string { return fmt.Sprintf("evt:%d", id) }

func TestMeetingIdentityLinkedTranscriptSharesKey(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, note := identityNote(t, st, "note", "meeting@example.org")
	ref := identityRef(event)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+ref, got.ByOccurrence[ref].Key)
	assert.Equal(t, got.ByOccurrence[ref].Key, got.ByTranscript[note].Key)
	assert.Equal(t, ref, got.ByTranscript[note].OccurrenceRef)
	assert.Equal(t, []string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Equal(t, []int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentityUnlinkedItemsStaySeparate(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "event@example.org"))
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := identityNote(t, st, "note", "")
	refs := []string{identityRef(event), identityRef(series) + "@20270314T100000Z", identityRef(series) + "@20270321T100000Z"}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	keys := map[string]bool{}
	for _, ref := range refs {
		assert.Equal(t, "occ:"+ref, got.ByOccurrence[ref].Key)
		keys[got.ByOccurrence[ref].Key] = true
	}
	assert.Equal(t, fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
	keys[got.ByTranscript[note].Key] = true
	assert.Len(t, keys, 4)
}

func TestMeetingIdentityAmbiguousMatchIsSuggestionOnlyForOwner(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	noteData, note := identityNote(t, st, "note", "weekly@example.org")
	refs := []string{identityRef(series) + "@20270314T100000Z", identityRef(single)}
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		suggestions bool
		want        []string
	}{
		{"owner", t.Context(), true, refs},
		{"disabled", t.Context(), false, nil},
		{"delegated", sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID, noteData.Message.SourceID}), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ResolveMeetingIdentitiesContext(tc.ctx, MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}, IncludeSuggestions: tc.suggestions})
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
			assert.Empty(t, got.ByTranscript[note].ExactOccurrenceRefs)
			assert.ElementsMatch(t, tc.want, got.ByTranscript[note].SuggestedOccurrenceRefs)
			for _, ref := range refs {
				assert.Empty(t, got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
		})
	}
}

func TestMeetingIdentityMultipleManualLinks(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, note := identityNote(t, st, "note", "")
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: one}))
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: two}))
	refs := []string{identityRef(one), identityRef(two)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, got.ByTranscript[note].ExactOccurrenceRefs)
	for _, ref := range refs {
		assert.Equal(t, []int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityOccurrenceOnlyFindsLinkedTranscript(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
			uid := "meeting@example.org"
			if manual {
				uid = ""
			}
			_, note := identityNote(t, st, "note", uid)
			if manual {
				require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			// The transcript is undated: discovery must use identity, not an activity window.
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(event)}})
			require.NoError(t, err)
			assert.Equal(t, []int64{note}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
			assert.Equal(t, "occ:"+identityRef(event), got.ByTranscript[note].Key)
		})
	}
}

func TestMeetingIdentityHiddenSourceTranscriptIgnored(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	visible, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, hiddenCopy := calendarFixture(t, st, "copy", singleLinkRaw("event", "meeting@example.org"), "hidden@example.com")
	_, note := identityNote(t, st, "hidden-note", "meeting@example.org")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.Message.SourceID})
	req := MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(event), identityRef(hiddenCopy)}, TranscriptIDs: []int64{note}, IncludeSuggestions: true}
	got, err := st.ResolveMeetingIdentitiesContext(ctx, req)
	require.NoError(t, err)
	require.Len(t, got.ByOccurrence, 1)
	assert.Empty(t, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
	assert.Empty(t, got.ByTranscript)
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	changed, err := st.ResolveMeetingIdentitiesContext(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, got, changed, "hidden evidence must not change authorized output")
}

func TestMeetingIdentityVisibleTranscriptIgnoresHiddenCalendar(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, hidden := calendarFixture(t, st, "hidden", singleLinkRaw("hidden", "weekly@example.org"), "hidden@example.com")
	eventData, series := calendarFixture(t, st, "series", linkSeriesRaw)
	noteData, note := identityNote(t, st, "note", "weekly@example.org")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{eventData.Message.SourceID, noteData.Message.SourceID})
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal(t, []string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: hidden}))
	changed, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal(t, got, changed, "hidden calendar decisions must not suppress visible links")
}

func TestMeetingIdentityInviteAndSyncedCopyMerge(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	inviteIDs := emailEvents(t, st, source)
	require.Len(t, inviteIDs, 1)
	invite := inviteIDs[0]
	_, synced := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	var kind string
	require.NoError(t, st.db.QueryRow(`SELECT message_type FROM messages WHERE id=?`, parent).Scan(&kind))
	assert.Equal(t, "email", kind, "the invitation remains a separate message")
	refs := []string{identityRef(invite), identityRef(synced)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+refs[0], got.ByOccurrence[refs[0]].Key)
	assert.Equal(t, got.ByOccurrence[refs[0]].Key, got.ByOccurrence[refs[1]].Key)
	assert.Equal(t, refs[0], got.ByOccurrence[refs[1]].OccurrenceRef)
}

func TestMeetingIdentityDamagedInviteEvidenceKeepsOtherMeetings(t *testing.T) {
	for _, damage := range []string{"missing", "malformed", "compression", "oversized", "multiple-events"} {
		t.Run(damage, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H"))
			require.NoError(t, err)
			ids := emailEvents(t, st, source)
			require.Len(t, ids, 1)
			invite := ids[0]
			_, valid := calendarFixture(t, st, "valid", singleLinkRaw("valid", "meeting@example.org"))
			_, note := identityNote(t, st, "note", "meeting@example.org")
			if damage == "missing" {
				_, err = st.db.Exec(`DELETE FROM message_raw WHERE message_id=?`, invite)
			} else {
				raw, compression := []byte("damaged invite"), ""
				if damage == "compression" {
					compression = "zlib"
				} else if damage == "oversized" {
					var compressed bytes.Buffer
					writer := zlib.NewWriter(&compressed)
					chunk := make([]byte, 1024)
					for range maxMeetingRawBytes/len(chunk) + 1 {
						_, err := writer.Write(chunk)
						require.NoError(t, err)
					}
					require.NoError(t, writer.Close())
					raw, compression = compressed.Bytes(), "zlib"
				} else if damage == "multiple-events" {
					raw = calendarFamilyDocument()
				}
				_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=? WHERE message_id=?`, raw, compression, invite)
			}
			require.NoError(t, err)
			refs := []string{identityRef(invite), identityRef(valid)}
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
			require.NoError(t, err)
			assert.Equal(t, "occ:"+refs[0], got.ByOccurrence[refs[0]].Key, "damaged evidence keeps its message identity")
			assert.Equal(t, "occ:"+refs[1], got.ByOccurrence[refs[1]].Key)
			assert.Equal(t, []int64{note}, got.ByOccurrence[refs[1]].LinkedTranscriptIDs)
			assert.Equal(t, refs[1], got.ByTranscript[note].OccurrenceRef)
		})
	}
}

func TestMeetingIdentityInviteReadFailurePropagates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H"))
	require.NoError(t, err)
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 1)
	// Inject a read failure inside the real snapshot: closing the database would
	// fail before UID hydration, so it cannot exercise this error boundary.
	failure := errors.New("calendar evidence read failed")
	for _, want := range []error{failure, context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, _ []driver.NamedValue) error {
				if strings.Contains(q, "SELECT raw.raw_data,raw.compression") {
					return want
				}
				return nil
			}})
			_, err := observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(ids[0])}})
			assert.ErrorIs(t, err, want)
		})
	}
}

func TestMeetingIdentityNoSuggestionQueriesWhenDisabled(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", linkSeriesRaw)
	calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, note := identityNote(t, st, "note", "weekly@example.org")
	count := 0
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, _ []driver.NamedValue) error {
		// Exact-link resolution is required even when suggestions are disabled.
		// This copy-selection predicate is only reached for an ambiguous single
		// candidate when the optional suggestion branch hydrates representatives.
		if strings.Contains(q, "FROM calendar_events e") && strings.Contains(q, "e.kind = 'single' OR") {
			count++
		}
		return nil
	}})
	got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Empty(t, got.ByTranscript[note].SuggestedOccurrenceRefs)
	assert.Zero(t, count, "disabled suggestions must not select their representatives")
	got, err = observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}, IncludeSuggestions: true})
	require.NoError(t, err)
	assert.Len(t, got.ByTranscript[note].SuggestedOccurrenceRefs, 2)
	assert.Positive(t, count, "the enabled control must exercise the optional queries")
}

func TestMeetingIdentityReverseOmitsUnrequestedSuggestions(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, note := identityNote(t, st, "note", "weekly@example.org")
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(t, err)
	assert.Empty(t, got.ByTranscript, "reverse discovery cannot expose suggestions when disabled")
	got, err = st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, IncludeSuggestions: true})
	require.NoError(t, err)
	assert.Contains(t, got.ByTranscript[note].SuggestedOccurrenceRefs, ref)
}

func TestMeetingIdentityPreservesPublicExceptionRefAndCanonicalKey(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := identityNote(t, st, "note", "")
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: moved}))
	public := identityRef(moved)
	canonical := identityRef(master) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{public, canonical}, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+canonical, got.ByOccurrence[public].Key)
	assert.Equal(t, "occ:"+canonical, got.ByOccurrence[canonical].Key)
	assert.Equal(t, public, got.ByOccurrence[public].OccurrenceRef)
	assert.Equal(t, public, got.ByTranscript[note].OccurrenceRef)
	assert.Equal(t, []string{public}, got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Equal(t, "occ:"+canonical, got.ByTranscript[note].Key)
}

func TestMeetingIdentityRetainsLegacyInviteSplit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	events := emailEvents(t, st, source)
	require.Len(t, events, 1)
	_, note := identityNote(t, st, "note", "family@example.com")
	// Earlier meeting links saved ICS decisions by message ID because the UID
	// was absent from metadata. Reading that existing decision must still split.
	_, err := st.db.Exec(meetingDecisionUpsert, note, "", sql.NullInt64{Int64: events[0], Valid: true}, "", "split", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(events[0])}, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Empty(t, got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Empty(t, got.ByOccurrence[identityRef(events[0])].LinkedTranscriptIDs)
	assert.Equal(t, fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
}
