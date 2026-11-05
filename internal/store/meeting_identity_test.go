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
	"go.kenn.io/msgvault/internal/calrecur"
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
		reverseOnly bool
	}{
		{"owner", t.Context(), true, refs, false},
		{"disabled", t.Context(), false, nil, false},
		{"delegated", sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID, noteData.Message.SourceID}), true, nil, false},
		{"occurrences only", t.Context(), false, nil, true},
		{"occurrences only enabled", t.Context(), true, refs, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)

			count := 0
			observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, _ []driver.NamedValue) error {
				// This predicate selects representatives for optional suggestions.
				if strings.Contains(q, "FROM calendar_events e") && strings.Contains(q, "e.kind = 'single' OR") {
					count++
				}
				return nil
			}})
			request := MeetingIdentityRequest{TranscriptIDs: []int64{note}, IncludeSuggestions: tc.suggestions}
			if tc.reverseOnly {
				request.TranscriptIDs = nil
				request.OccurrenceRefs = refs[:1]
			}
			got, err := observed.ResolveMeetingIdentitiesContext(tc.ctx, request)
			require.NoError(err)
			if !tc.suggestions {
				assert.Zero(count, "disabled suggestions must not select their representatives")
			} else if tc.name == "owner" {
				assert.Positive(count, "the enabled control must exercise the optional queries")
			}
			if tc.reverseOnly && !tc.suggestions {
				assert.Empty(got.ByTranscript)
			} else {
				assert.Equal(fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
				assert.Empty(got.ByTranscript[note].ExactOccurrenceRefs)
				assert.ElementsMatch(tc.want, got.ByTranscript[note].SuggestedOccurrenceRefs)
			}
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
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	assert.Len(links, 2)
	refs := []string{identityRef(one), identityRef(two)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.ElementsMatch(refs, got.ByTranscript[note].ExactOccurrenceRefs)
	for _, ref := range refs {
		assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityOccurrenceOnlyFindsLinkedTranscript(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, note := identityNote(t, st, "note", "meeting@example.org")
	// The transcript is undated: discovery must use identity, not an activity window.
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(event)}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
	assert.Equal("occ:"+identityRef(event), got.ByTranscript[note].Key)
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
	assert.Empty(changed.ByTranscript[note].ExactOccurrenceRefs)
	assert.Equal(fmt.Sprintf("msg:%d", note), changed.ByTranscript[note].Key)
	assert.Empty(changed.ByOccurrence)
}

func TestMeetingIdentityInviteAndSyncedCopyMerge(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	inviteIDs := emailEvents(t, st, source)
	require.Len(inviteIDs, 1)
	invite := inviteIDs[0]
	_, synced := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
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

func TestMeetingIdentityCalendarReadFailurePropagates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H"))
	require.NoError(t, err)
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 1)
	// Inject a calendar read failure inside the real snapshot.
	failure := errors.New("calendar evidence read failed")
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, _ []driver.NamedValue) error {
		if strings.Contains(q, "SELECT e.message_id, m.source_id") {
			return failure
		}
		return nil
	}})
	_, err = observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(ids[0])}})
	assert.ErrorIs(t, err, failure)
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

func TestMeetingIdentityFractionalAliasReverseDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"old","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	_, old := calendarFixture(t, st, "old", raw)
	_, note := projectionFixture(t, st, "undated", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: old}))
	_, latest := calendarFixture(t, st, "new", strings.ReplaceAll(raw, `"old"`, `"new"`))
	_, other := calendarFixture(t, st, "other", strings.ReplaceAll(strings.ReplaceAll(raw, `"old"`, `"other"`), ".123456Z", ".654321Z"))
	_, otherNote := projectionFixture(t, st, "other-note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: other}))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(err)
	refs := []string{}
	for _, o := range page.Occurrences {
		refs = append(refs, o.Ref)
	}
	require.ElementsMatch([]string{identityRef(latest), identityRef(other)}, refs)
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	require.Equal(identityRef(latest), forward.ByTranscript[note].OccurrenceRef)
	reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(err)
	assert.Equal([]int64{note}, reverse.ByOccurrence[identityRef(latest)].LinkedTranscriptIDs)
	assert.Equal([]int64{otherNote}, reverse.ByOccurrence[identityRef(other)].LinkedTranscriptIDs)
}

func TestMeetingIdentityMovedCopyReverseDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, original := calendarFixture(t, st, "original", linkSeriesRaw)
	_, moved := calendarFixture(t, st, "moved", linkSeriesRaw, "other@example.org")
	calendarFixture(t, st, "exception", `{"id":"exception","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "other@example.org")
	_, note := projectionFixture(t, st, "moved-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	require.Equal(identityRef(original)+"@20270314T100000Z", forward.ByTranscript[note].OccurrenceRef)
	for _, id := range []int64{original, moved} {
		ref := identityRef(id) + "@20270314T100000Z"
		reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
		require.NoError(err)
		assert.Equal([]int64{note}, reverse.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityRecurringUIDReadBound(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := identityNote(t, st, "matching", "weekly@example.org")
	for i := range 220 {
		raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, time.Date(2028, 1, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.RFC3339))
		projectionFixture(t, st, fmt.Sprintf("other-%d", i), "twenty_json", raw)
	}
	reads := 0
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, _ string, _ []driver.NamedValue) error { reads++; return nil }})
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	assert.Len(got.ByTranscript, 1)
	assert.LessOrEqual(reads, 45)
}

func TestMeetingIdentityDamagedUIDCopy(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270314T100000Z\r\nDURATION:PT1H"))
	require.NoError(err)
	ids := emailEvents(t, st, source)
	require.Len(ids, 1)
	damaged := ids[0]
	_, event := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	_, note := identityNote(t, st, "note", "")
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	err = st.UpsertMessageRawWithFormat(damaged, []byte("damaged invitation"), rawFormatICS)
	require.NoError(err)
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	if assert.Len(links, 1) {
		assert.Equal(identityRef(event), links[0].Ref)
	}
	ref := identityRef(event)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal(ref, got.ByOccurrence[ref].OccurrenceRef)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentityTranscriptOnlyCompletesGroup(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "group@example.org"))
	_, one := identityNote(t, st, "one", "group@example.org")
	_, two := identityNote(t, st, "two", "")
	require.NoError(st.LinkMeetingContext(t.Context(), two, OccurrenceRef{MessageID: event}))
	_, split := identityNote(t, st, "split", "group@example.org")
	require.NoError(st.UnlinkMeetingContext(t.Context(), split, OccurrenceRef{MessageID: event}))
	for _, note := range []int64{one, two} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
		require.NoError(err)
		assert.Equal([]int64{one, two}, got.ByTranscript[note].LinkedTranscriptIDs)
		assert.Equal([]int64{one, two}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityCompletesNewManualGroups(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, three := calendarFixture(t, st, "three", singleLinkRaw("three", "three@example.org"))
	_, first := identityNote(t, st, "first", "")
	_, second := identityNote(t, st, "second", "")
	_, deferred := identityNote(t, st, "deferred", "one@example.org")
	for _, link := range []struct{ note, event int64 }{{first, one}, {first, two}, {second, two}, {second, three}, {deferred, three}} {
		require.NoError(st.LinkMeetingContext(t.Context(), link.note, OccurrenceRef{MessageID: link.event}))
	}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(one)}})
	require.NoError(err)
	assert.Equal([]int64{first}, got.ByOccurrence[identityRef(one)].LinkedTranscriptIDs)
	assert.Equal([]int64{first, second}, got.ByOccurrence[identityRef(two)].LinkedTranscriptIDs)
	assert.Equal([]int64{second, deferred}, got.ByOccurrence[identityRef(three)].LinkedTranscriptIDs)
}

func TestMeetingIdentityCompletesSuggestedGroups(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "series", linkSeriesRaw)
	_, two := calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, ambiguous := identityNote(t, st, "ambiguous", "weekly@example.org")
	_, linked := identityNote(t, st, "linked", "")
	require.NoError(st.LinkMeetingContext(t.Context(), linked, OccurrenceRef{MessageID: two}))
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{ambiguous}, IncludeSuggestions: true})
	require.NoError(err)
	assert.ElementsMatch([]string{identityRef(one) + "@20270314T100000Z", identityRef(two)}, got.ByTranscript[ambiguous].SuggestedOccurrenceRefs)
	assert.Equal([]int64{linked}, got.ByOccurrence[identityRef(two)].LinkedTranscriptIDs)
}

func TestMeetingIdentityHiddenCopiesDoNotConsumeCandidateLimit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "visible", singleLinkRaw("visible", "copies@example.org"))
	noteData, note := identityNote(t, st, "note", "copies@example.org")
	for i := range 201 {
		key := fmt.Sprintf("hidden-%d", i)
		calendarFixture(t, st, key, singleLinkRaw(key, "copies@example.org"), "hidden@example.com")
	}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID, noteData.Message.SourceID})
	got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal(identityRef(event), got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]int64{note}, got.ByOccurrence[identityRef(event)].LinkedTranscriptIDs)
}

func TestMeetingIdentityBatchReadBound(t *testing.T) {
	for _, format := range []string{"twenty_json", "granola_json"} {
		t.Run(format, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			req := MeetingIdentityRequest{}
			for i := range 20 {
				_, id := calendarFixture(t, st, fmt.Sprintf("event-%d", i), singleLinkRaw(fmt.Sprintf("event-%d", i), fmt.Sprintf("event-%d@example.org", i)))
				key := fmt.Sprintf("event-%d", i)
				raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, key+"@example.org")
				if format == "granola_json" {
					raw = fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q}}`, key)
				}
				_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), format, raw)
				req.OccurrenceRefs = append(req.OccurrenceRefs, identityRef(id))
				req.TranscriptIDs = append(req.TranscriptIDs, note)
			}
			for _, mode := range []string{"both", "events", "notes"} {
				t.Run(mode, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					request := req
					if mode == "events" {
						request.TranscriptIDs = nil
					}
					if mode == "notes" {
						request.OccurrenceRefs = nil
					}
					reads, sourceReads := 0, 0
					observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
						reads++
						if strings.Contains(query, "SELECT id,sync_config FROM sources") {
							sourceReads++
						}
						return nil
					}})
					got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), request)
					require.NoError(err)
					require.Len(got.ByTranscript, 20)
					for i, ref := range req.OccurrenceRefs {
						assert.Equal([]int64{req.TranscriptIDs[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
					}
					assert.LessOrEqual(reads, 35)
					if format == "granola_json" {
						assert.Equal(1, sourceReads)
					}
				})
			}
		})
	}
}

func TestMeetingFractionalExceptionKeepsMessageIdentity(t *testing.T) {
	for _, archivedMaster := range []bool{false, true} {
		t.Run(strconv.FormatBool(archivedMaster), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var master int64
			if archivedMaster {
				_, master = calendarFixture(t, st, "master", linkSeriesRaw)
			}
			_, exception := calendarFixture(t, st, "fractional", `{"id":"fractional","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
			_, note := identityNote(t, st, "note", "")
			// Orphan fractional decisions already retain the original key internally.
			_, err := st.db.Exec(meetingDecisionUpsert, note, "", sql.NullInt64{Int64: exception, Valid: true}, "20270314T100000Z", "link", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			requirements.NoError(err)
			ref := identityRef(exception)
			from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			budget := calrecur.RequestReadBudget
			resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), OccurrenceRef{MessageID: exception}, &budget)
			if archivedMaster {
				requirements.NoError(err)
				masters, err := meetingEventRows(ctx, st.meetingReader(ctx), "e.message_id = ?", []any{master}, &budget)
				requirements.NoError(err)
				requirements.Len(masters, 1)
				expanded, err := expandMeetingSeries(ctx, st.meetingReader(ctx), masters[0], from, from.AddDate(0, 0, 1), &budget)
				requirements.NoError(err)
				requirements.Len(expanded, 2)
				for _, occurrence := range expanded {
					if occurrence.Row.MessageID == exception {
						assertions.Equal(OccurrenceRef{MessageID: exception}, occurrence.Ref)
						assertions.Equal(meetingIdentity{EventMessageID: exception, OccurrenceKey: "20270314T100000Z"}, occurrence.Identity)
					}
				}
			}
			release()
			requirements.NoError(err)
			assertions.Equal(OccurrenceRef{MessageID: exception}, resolved.Ref)
			assertions.Equal(meetingIdentity{EventMessageID: exception, OccurrenceKey: "20270314T100000Z"}, resolved.Identity)
			assertions.Equal(from.Add(12*time.Hour), resolved.Start)
			assertions.Equal(from.Add(13*time.Hour), resolved.End)
			identities, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: []int64{note}})
			requirements.NoError(err)
			assertions.Equal("occ:"+ref, identities.ByOccurrence[ref].Key)
			assertions.Equal(ref, identities.ByTranscript[note].OccurrenceRef)
		})
	}
}
