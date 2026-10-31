package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
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
	assert := assert.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, note := identityNote(t, st, "note", "meeting@example.org")
	ref := identityRef(event)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
	assert.Equal(got.ByOccurrence[ref].Key, got.ByTranscript[note].Key)
	assert.Equal(ref, got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentityUnlinkedItemsStaySeparate(t *testing.T) {
	assert := assert.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "event@example.org"))
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := identityNote(t, st, "note", "")
	refs := []string{identityRef(event), identityRef(series) + "@20270314T100000Z", identityRef(series) + "@20270321T100000Z"}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	keys := map[string]bool{}
	for _, ref := range refs {
		assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
		keys[got.ByOccurrence[ref].Key] = true
	}
	assert.Equal(fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
	keys[got.ByTranscript[note].Key] = true
	assert.Len(keys, 4)
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
			assert := assert.New(t)

			got, err := st.ResolveMeetingIdentitiesContext(tc.ctx, MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}, IncludeSuggestions: tc.suggestions})
			require.NoError(t, err)
			assert.Equal(fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
			assert.Empty(got.ByTranscript[note].ExactOccurrenceRefs)
			assert.ElementsMatch(tc.want, got.ByTranscript[note].SuggestedOccurrenceRefs)
			for _, ref := range refs {
				assert.Empty(got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
		})
	}
}

func TestMeetingIdentityMultipleManualLinks(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, note := identityNote(t, st, "note", "")
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: one}))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: two}))
	refs := []string{identityRef(one), identityRef(two)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.ElementsMatch(refs, got.ByTranscript[note].ExactOccurrenceRefs)
	for _, ref := range refs {
		assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityOccurrenceOnlyFindsLinkedTranscript(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(strconv.FormatBool(manual), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
			uid := "meeting@example.org"
			if manual {
				uid = ""
			}
			_, note := identityNote(t, st, "note", uid)
			if manual {
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			// The transcript is undated: discovery must use identity, not an activity window.
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(event)}})
			require.NoError(err)
			assert.Equal([]int64{note}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
			assert.Equal("occ:"+identityRef(event), got.ByTranscript[note].Key)
		})
	}
}

func TestMeetingIdentityHiddenSourceTranscriptIgnored(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	visible, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, hiddenCopy := calendarFixture(t, st, "copy", singleLinkRaw("event", "meeting@example.org"), "hidden@example.com")
	_, note := identityNote(t, st, "hidden-note", "meeting@example.org")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.Message.SourceID})
	req := MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(event), identityRef(hiddenCopy)}, TranscriptIDs: []int64{note}, IncludeSuggestions: true}
	got, err := st.ResolveMeetingIdentitiesContext(ctx, req)
	require.NoError(err)
	require.Len(got.ByOccurrence, 1)
	assert.Empty(got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
	assert.Empty(got.ByTranscript)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	changed, err := st.ResolveMeetingIdentitiesContext(ctx, req)
	require.NoError(err)
	assert.Equal(got, changed, "hidden evidence must not change authorized output")
}

func TestMeetingIdentityVisibleTranscriptIgnoresHiddenCalendar(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, hidden := calendarFixture(t, st, "hidden", singleLinkRaw("hidden", "weekly@example.org"), "hidden@example.com")
	eventData, series := calendarFixture(t, st, "series", linkSeriesRaw)
	noteData, note := identityNote(t, st, "note", "weekly@example.org")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{eventData.Message.SourceID, noteData.Message.SourceID})
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal([]string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: hidden}))
	changed, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal(got, changed, "hidden calendar decisions must not suppress visible links")
}

func TestMeetingIdentityInviteAndSyncedCopyMerge(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	inviteIDs := emailEvents(t, st, source)
	require.Len(inviteIDs, 1)
	invite := inviteIDs[0]
	_, synced := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	var kind string
	require.NoError(st.db.QueryRow(`SELECT message_type FROM messages WHERE id=?`, parent).Scan(&kind))
	assert.Equal("email", kind, "the invitation remains a separate message")
	refs := []string{identityRef(invite), identityRef(synced)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(err)
	assert.Equal("occ:"+refs[0], got.ByOccurrence[refs[0]].Key)
	assert.Equal(got.ByOccurrence[refs[0]].Key, got.ByOccurrence[refs[1]].Key)
	assert.Equal(refs[0], got.ByOccurrence[refs[1]].OccurrenceRef)
}

func TestMeetingIdentityDamagedInviteEvidenceKeepsOtherMeetings(t *testing.T) {
	for _, damage := range []string{"missing", "malformed", "compression", "oversized", "multiple-events"} {
		t.Run(damage, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H"))
			require.NoError(err)
			ids := emailEvents(t, st, source)
			require.Len(ids, 1)
			invite := ids[0]
			_, valid := calendarFixture(t, st, "valid", singleLinkRaw("valid", "meeting@example.org"))
			_, note := identityNote(t, st, "note", "meeting@example.org")
			if damage == "missing" {
				_, err = st.db.Exec(`DELETE FROM message_raw WHERE message_id=?`, invite)
			} else {
				raw, compression := []byte("damaged invite"), ""
				switch damage {
				case "compression":
					compression = "zlib"
				case "oversized":
					var compressed bytes.Buffer
					writer := zlib.NewWriter(&compressed)
					chunk := make([]byte, 1024)
					for range maxMeetingRawBytes/len(chunk) + 1 {
						_, err := writer.Write(chunk)
						require.NoError(err)
					}
					require.NoError(writer.Close())
					raw, compression = compressed.Bytes(), "zlib"
				case "multiple-events":
					raw = calendarFamilyDocument()
				}
				_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=? WHERE message_id=?`, raw, compression, invite)
			}
			require.NoError(err)
			refs := []string{identityRef(invite), identityRef(valid)}
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
			require.NoError(err)
			assert.Equal("occ:"+refs[0], got.ByOccurrence[refs[0]].Key, "damaged evidence keeps its message identity")
			assert.Equal("occ:"+refs[1], got.ByOccurrence[refs[1]].Key)
			assert.Equal([]int64{note}, got.ByOccurrence[refs[1]].LinkedTranscriptIDs)
			assert.Equal(refs[1], got.ByTranscript[note].OccurrenceRef)
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
	assert, require := assert.New(t), require.New(t)
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
	require.NoError(err)
	assert.Empty(got.ByTranscript[note].SuggestedOccurrenceRefs)
	assert.Zero(count, "disabled suggestions must not select their representatives")
	got, err = observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}, IncludeSuggestions: true})
	require.NoError(err)
	assert.Len(got.ByTranscript[note].SuggestedOccurrenceRefs, 2)
	assert.Positive(count, "the enabled control must exercise the optional queries")
}

func TestMeetingIdentityReverseOmitsUnrequestedSuggestions(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, note := identityNote(t, st, "note", "weekly@example.org")
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Empty(got.ByTranscript, "reverse discovery cannot expose suggestions when disabled")
	got, err = st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, IncludeSuggestions: true})
	require.NoError(err)
	assert.Contains(got.ByTranscript[note].SuggestedOccurrenceRefs, ref)
}

func TestMeetingIdentityPreservesPublicExceptionRefAndCanonicalKey(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := identityNote(t, st, "note", "")
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: moved}))
	public := identityRef(moved)
	canonical := identityRef(master) + "@20270314T100000Z"
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{public, canonical}, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal("occ:"+canonical, got.ByOccurrence[public].Key)
	assert.Equal("occ:"+canonical, got.ByOccurrence[canonical].Key)
	assert.Equal(public, got.ByOccurrence[public].OccurrenceRef)
	assert.Equal(public, got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]string{public}, got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Equal("occ:"+canonical, got.ByTranscript[note].Key)
}

func TestMeetingIdentityRetainsLegacyInviteSplit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	events := emailEvents(t, st, source)
	require.Len(events, 1)
	_, note := identityNote(t, st, "note", "family@example.com")
	// Earlier meeting links saved ICS decisions by message ID because the UID
	// was absent from metadata. Reading that existing decision must still split.
	_, err := st.db.Exec(meetingDecisionUpsert, note, "", sql.NullInt64{Int64: events[0], Valid: true}, "", "split", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(events[0])}, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Empty(got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Empty(got.ByOccurrence[identityRef(events[0])].LinkedTranscriptIDs)
	assert.Equal(fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
}

func TestMeetingIdentityOccurrenceOnlyFindsGranolaWithOrphanRepresentative(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"orphan","iCalUID":"weekly@example.org","recurringEventId":"other-series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ref := identityRef(orphan)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	assert.Equal("occ:"+ref, got.ByTranscript[note].Key)
	assert.Equal(ref, got.ByTranscript[note].OccurrenceRef)
	// The normal series copy shares the orphan representative and its note.
	seriesRef := identityRef(series) + "@20270314T100000Z"
	got, err = st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{seriesRef}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[seriesRef].LinkedTranscriptIDs)
	assert.Equal("occ:"+ref, got.ByOccurrence[seriesRef].Key)
}

func TestMeetingIdentityOccurrenceOnlyFindsOrphanDecisionAfterMasterArrives(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	orphanData, orphan := calendarFixture(t, st, "orphan", `{"id":"orphan","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	noteData, note := identityNote(t, st, "manual-note", "")
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	before, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(orphan)}})
	requirements.NoError(err)
	assertions.Equal([]int64{note}, before.ByOccurrence[identityRef(orphan)].LinkedTranscriptIDs)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	// Matching provider and original keys from another source must stay separate.
	_, unrelated := calendarFixture(t, st, "other-orphan", `{"id":"other-orphan","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "other@example.com")
	_, otherNote := identityNote(t, st, "other-note", "")
	requirements.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: unrelated}))
	ref := identityRef(master) + "@20270314T100000Z"
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	requirements.NoError(err)
	assertions.Equal("occ:"+ref, forward.ByTranscript[note].Key)
	for _, ctx := range []context.Context{t.Context(), sourcescope.WithSourceIDs(t.Context(), []int64{orphanData.Message.SourceID, noteData.Message.SourceID})} {
		got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
		requirements.NoError(err)
		assertions.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
		assertions.Equal("occ:"+ref, got.ByTranscript[note].Key)
		assertions.NotContains(got.ByTranscript, otherNote)
	}
	hidden, err := st.ResolveMeetingIdentitiesContext(sourcescope.WithSourceIDs(t.Context(), []int64{orphanData.Message.SourceID}), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	requirements.NoError(err)
	assertions.Empty(hidden.ByOccurrence[ref].LinkedTranscriptIDs)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	split, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	requirements.NoError(err)
	assertions.Empty(split.ByOccurrence[ref].LinkedTranscriptIDs)
	assertions.NotContains(split.ByTranscript, note)
}
