package store

import (
	"context"
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

func TestMeetingIdentityLinkedTranscriptSharesKey(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	_, note := identityNote(t, st, "note", "meeting@example.org")
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event})
	for _, tc := range []struct {
		name          string
		transcriptIDs []int64
	}{
		{"transcript and occurrence", []int64{note}},
		{"occurrence only", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: tc.transcriptIDs})
			require.NoError(t, err)
			assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
			assert.Equal("occ:"+ref, got.ByTranscript[note].Key)
			assert.Equal(ref, got.ByTranscript[note].OccurrenceRef)
			assert.Equal([]string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
			assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
		})
	}
}

func TestMeetingIdentitySeriesReferenceNeedsOccurrenceKey(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "single@example.org"))
	_, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{fmt.Sprintf("evt:%d", single), fmt.Sprintf("evt:%d", series)}})
	require.ErrorContains(err, "series reference needs @<occurrence key>")
	singleRef := fmt.Sprintf("evt:%d", single)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{singleRef, "evt:999999"}})
	require.NoError(err)
	assert.Contains(got.ByOccurrence, singleRef)
	assert.NotContains(got.ByOccurrence, "evt:999999")
}

func TestMeetingIdentityUnlinkedItemsStaySeparate(t *testing.T) {
	assert := assert.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "event@example.org"))
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := identityNote(t, st, "note", "")
	refs := []string{formatMeetingOccurrence(OccurrenceRef{MessageID: event}), formatMeetingOccurrence(OccurrenceRef{MessageID: series}) + "@20270314T100000Z", formatMeetingOccurrence(OccurrenceRef{MessageID: series}) + "@20270321T100000Z"}
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

func TestMeetingIdentityHiddenEvidenceIgnored(t *testing.T) {
	for _, mode := range []string{"hidden transcript", "hidden calendar", "merged archive visibility"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "hidden transcript":
				assert, require := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				visible, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
				_, hiddenCopy := calendarFixture(t, st, "copy", singleLinkRaw("event", "meeting@example.org"), "hidden@example.com")
				_, note := identityNote(t, st, "hidden-note", "meeting@example.org")
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.Message.SourceID})
				req := MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: event}), formatMeetingOccurrence(OccurrenceRef{MessageID: hiddenCopy})}, TranscriptIDs: []int64{note}}
				got, err := st.ResolveMeetingIdentitiesContext(ctx, req)
				require.NoError(err)
				require.Len(got.ByOccurrence, 1)
				assert.Empty(got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: event})].LinkedTranscriptIDs)
				assert.Empty(got.ByTranscript)
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
				changed, err := st.ResolveMeetingIdentitiesContext(ctx, req)
				require.NoError(err)
				assert.Equal(got, changed, "hidden evidence must not change authorized output")
			case "hidden calendar":
				assert, require := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				_, hidden := calendarFixture(t, st, "hidden", singleLinkRaw("hidden", "weekly@example.org"), "hidden@example.com")
				eventData, series := calendarFixture(t, st, "series", linkSeriesRaw)
				noteData, note := identityNote(t, st, "note", "weekly@example.org")
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{eventData.Message.SourceID, noteData.Message.SourceID})
				ref := formatMeetingOccurrence(OccurrenceRef{MessageID: series}) + "@20270314T100000Z"
				got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
				require.NoError(err)
				assert.Equal([]string{ref}, got.ByTranscript[note].ExactOccurrenceRefs)
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: hidden}))
				changed, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: []int64{note}})
				require.NoError(err)
				assert.Equal(got, changed, "a hidden manual target leaves visible automatic evidence intact")
			case "merged archive visibility":
				assert, require := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				history, event := calendarFixture(t, st, "event", singleLinkRaw("event", "family@example.com"), "history@example.com")
				destination, err := st.GetOrCreateSource("gcal", "visible@example.com")
				require.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
				require.NoError(err)
				ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event})
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{destination.ID})
				req := MeetingIdentityRequest{OccurrenceRefs: []string{ref}}
				got, err := st.ResolveMeetingIdentitiesContext(ctx, req)
				require.NoError(err)
				require.Len(got.ByOccurrence, 1)
				assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
			}
		})
	}
}

func TestMeetingIdentityEditedDailySeriesReuseExpansion(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs, public []string
	var notes []int64
	for _, name := range []string{"first", "second"} {
		uid := name + "@example.org"
		_, series := calendarFixture(t, st, name, fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2000-01-01T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2000-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, name, uid))
		for day := 1; day <= 31; day++ {
			original := time.Date(2027, 3, day, 10, 0, 0, 0, time.UTC)
			at := original.Add(2 * time.Hour)
			key := name + "_" + original.Format("20060102T150405Z")
			_, edited := calendarFixture(t, st, key, fmt.Sprintf(`{"id":%q,"iCalUID":%q,"recurringEventId":%q,"originalStartTime":{"dateTime":%q},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, key, uid, name, original.Format(time.RFC3339), at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339)))
			_, note := projectionFixture(t, st, "note-"+key, "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":%q}}`, uid, at.Format(time.RFC3339)))
			refs = append(refs, fmt.Sprintf("evt:%d@%s", series, original.Format("20060102T150405Z")))
			public = append(public, fmt.Sprintf("evt:%d", edited))
			notes = append(notes, note)
		}
	}
	for _, requested := range [][]string{refs, nil} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: requested, TranscriptIDs: notes})
		require.NoError(err)
		require.Len(got.ByTranscript, 62)
		for i, ref := range refs {
			assert.Equal("occ:"+ref, got.ByTranscript[notes[i]].Key)
			assert.Equal(public[i], got.ByTranscript[notes[i]].OccurrenceRef)
			assert.Equal([]int64{notes[i]}, got.ByOccurrence[public[i]].LinkedTranscriptIDs)
		}
	}
}

func TestMeetingIdentitySparseReferencesKeepWindowGaps(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "daily", `{"id":"daily","iCalUID":"daily@example.org","start":{"dateTime":"2027-01-01T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	for i := range 210 {
		at := time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		calendarFixture(t, st, fmt.Sprintf("edit-%d", i), fmt.Sprintf(`{"id":"edit-%d","iCalUID":"daily@example.org","recurringEventId":"daily","originalStartTime":{"dateTime":%q},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, i, at.Format(time.RFC3339), at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339)))
	}
	refs := []string{fmt.Sprintf("evt:%d@20270101T100000Z", series), fmt.Sprintf("evt:%d@20271231T100000Z", series)}
	for _, requested := range [][]string{refs[:1], refs[1:], refs} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: requested})
		require.NoError(err)
		require.Len(got.ByOccurrence, len(requested))
		for _, ref := range requested {
			assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
			assert.Equal(ref, got.ByOccurrence[ref].OccurrenceRef)
		}
	}
}

func TestMeetingIdentityTwoLongRunningDailySeriesSavedDecisions(t *testing.T) {
	for _, decision := range []string{"link", "split"} {
		t.Run(decision, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			refs := make([]string, 0, 62)
			notes := make([]int64, 0, 62)
			for _, name := range []string{"first", "second"} {
				uid := name + "@example.org"
				_, series := calendarFixture(t, st, name, fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2000-01-01T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2000-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, name, uid))
				for day := 1; day <= 31; day++ {
					at := time.Date(2027, 3, day, 10, 0, 0, 0, time.UTC)
					ref := OccurrenceRef{MessageID: series, Key: at.Format("20060102T150405Z")}
					noteUID := ""
					if decision == "split" {
						noteUID = uid
					}
					_, note := projectionFixture(t, st, fmt.Sprintf("%s-note-%d", name, day), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":%q}}`, noteUID, at.Format(time.RFC3339)))
					if decision == "link" {
						require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
					} else {
						require.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
					}
					refs = append(refs, formatMeetingOccurrence(ref))
					notes = append(notes, note)
				}
			}
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: notes})
			require.NoError(err)
			require.Len(got.ByOccurrence, 62)
			require.Len(got.ByTranscript, 62)
			for i, ref := range refs {
				if decision == "link" {
					assert.Equal([]int64{notes[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
					assert.Equal(ref, got.ByTranscript[notes[i]].OccurrenceRef)
				} else {
					assert.Empty(got.ByOccurrence[ref].LinkedTranscriptIDs)
					assert.Empty(got.ByTranscript[notes[i]].ExactOccurrenceRefs)
					assert.Equal(fmt.Sprintf("msg:%d", notes[i]), got.ByTranscript[notes[i]].Key)
				}
			}
		})
	}
}

func TestMeetingIdentityInviteAndSyncedCopyShareIdentity(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	inviteIDs := emailEvents(t, st, source)
	require.Len(inviteIDs, 1)
	invite := inviteIDs[0]
	inviteRef := formatMeetingOccurrence(OccurrenceRef{MessageID: invite})
	before, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{inviteRef}})
	require.NoError(err)
	assert.Equal("occ:"+inviteRef, before.ByOccurrence[inviteRef].Key)
	_, synced := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	refs := []string{inviteRef, formatMeetingOccurrence(OccurrenceRef{MessageID: synced})}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(err)
	require.Len(got.ByOccurrence, 2)
	for _, ref := range refs {
		assert.Equal("occ:"+inviteRef, got.ByOccurrence[ref].Key)
		assert.Equal(inviteRef, got.ByOccurrence[ref].OccurrenceRef)
	}
}

func TestMeetingIdentityPreservesNoteScheduledRepresentative(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H")))
	invites := emailEvents(t, st, source)
	require.Len(invites, 1)
	_, synced := calendarFixture(t, st, "synced", singleLinkRaw("synced", "family@example.com"))
	_, note := identityNote(t, st, "note", "family@example.com")
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	syncedRef := formatMeetingOccurrence(OccurrenceRef{MessageID: synced})
	assert.Equal(syncedRef, record.Links[0].Ref)
	refs := []string{formatMeetingOccurrence(OccurrenceRef{MessageID: invites[0]}), syncedRef}
	for _, requested := range [][]string{nil, refs, {refs[1], refs[0]}} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: requested, TranscriptIDs: []int64{note}})
		require.NoError(err)
		assert.Equal(record.Links[0].Ref, got.ByTranscript[note].OccurrenceRef)
		assert.Equal([]string{syncedRef}, got.ByTranscript[note].ExactOccurrenceRefs)
		for _, ref := range refs {
			assert.Equal("occ:"+refs[0], got.ByOccurrence[ref].Key)
			assert.Equal(refs[0], got.ByOccurrence[ref].OccurrenceRef)
		}
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
		if strings.HasPrefix(q, "SELECT e.message_id,") {
			return failure
		}
		return nil
	}})
	_, err = observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: ids[0]})}})
	assert.ErrorIs(t, err, failure)
}

func TestMeetingIdentityUpdatedExceptionOrder(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", linkSeriesRaw)
	raw := `{"id":"edit","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	history, _ := calendarFixture(t, st, "edit", raw, "history@example.com")
	live, latest := calendarFixture(t, st, "edit", strings.NewReplacer("12:00:00Z", "14:00:00Z", "13:00:00Z", "15:00:00Z").Replace(raw))
	_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: live.Message.SourceID})
	requirements.NoError(err)
	_, note := identityNote(t, st, "note", "")
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	orders := []string{"default"}
	if !st.IsPostgreSQL() {
		st.db.SetMaxOpenConns(1)
		orders = []string{"OFF", "ON"}
	}
	for _, order := range orders {
		t.Run(order, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			if !st.IsPostgreSQL() {
				_, err := st.db.Exec("PRAGMA reverse_unordered_selects = " + order)
				require.NoError(err)
			}
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			require.Len(links, 1)
			public := formatMeetingOccurrence(OccurrenceRef{MessageID: latest})
			require.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), links[0].Start)
			canonical := formatMeetingOccurrence(ref)
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{public, canonical}, TranscriptIDs: []int64{note}})
			require.NoError(err)
			assert.Equal("occ:"+canonical, got.ByOccurrence[public].Key)
			assert.Equal("occ:"+canonical, got.ByOccurrence[canonical].Key)
			assert.Equal(public, got.ByOccurrence[public].OccurrenceRef)
			assert.Equal(public, got.ByOccurrence[canonical].OccurrenceRef)
			assert.Equal(public, got.ByTranscript[note].OccurrenceRef)
			assert.Equal([]int64{note}, got.ByOccurrence[canonical].LinkedTranscriptIDs)
			assert.Equal([]string{public}, got.ByTranscript[note].ExactOccurrenceRefs)
			assert.Equal("occ:"+canonical, got.ByTranscript[note].Key)
		})
	}
}

func TestMeetingIdentityOccurrenceOnlyCancelledMasterSeriesInstance(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", `{"id":"series","status":"cancelled","recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, copyID := calendarFixture(t, st, "copy", strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"copy-series"`, 1), "copy@example.com")
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	for _, ref := range []string{
		fmt.Sprintf("evt:%d@20270314T100000Z", copyID),
		fmt.Sprintf("evt:%d", exception),
	} {
		t.Run(ref, func(t *testing.T) {
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
			require.NoError(t, err)
			assert.Equal(t, []int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
		})
	}
}

func TestMeetingIdentityOccurrenceOnlyPreservesGranolaRepresentative(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"orphan","iCalUID":"weekly@example.org","recurringEventId":"other-series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	seriesRef := formatMeetingOccurrence(OccurrenceRef{MessageID: series}) + "@20270314T100000Z"
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	require.Equal(seriesRef, record.Links[0].Ref)
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: orphan})
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	assert.Equal("occ:"+ref, got.ByTranscript[note].Key)
	assert.Equal(record.Links[0].Ref, got.ByTranscript[note].OccurrenceRef)
	// Both copies share a group while the note retains its selected copy.
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
	before, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: orphan})}})
	requirements.NoError(err)
	assertions.Equal([]int64{note}, before.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: orphan})].LinkedTranscriptIDs)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	// Matching provider and original keys from another source must stay separate.
	_, unrelated := calendarFixture(t, st, "other-orphan", `{"id":"other-orphan","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "other@example.com")
	_, otherNote := identityNote(t, st, "other-note", "")
	requirements.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: unrelated}))
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: master}) + "@20270314T100000Z"
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
}

func TestMeetingIdentityFractionalReferencesRemainSeparate(t *testing.T) {
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
	refs := []string{formatMeetingOccurrence(OccurrenceRef{MessageID: old}), formatMeetingOccurrence(OccurrenceRef{MessageID: latest}), formatMeetingOccurrence(OccurrenceRef{MessageID: other})}
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	require.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: old}), forward.ByTranscript[note].OccurrenceRef)
	reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(err)
	assert.Equal([]int64{note}, reverse.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: old})].LinkedTranscriptIDs)
	assert.Empty(reverse.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: latest})].LinkedTranscriptIDs)
	assert.Equal([]int64{otherNote}, reverse.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: other})].LinkedTranscriptIDs)
}

func TestMeetingIdentityMovedCopyReverseDiscovery(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "original", linkSeriesRaw)
	calendarFixture(t, st, "moved", linkSeriesRaw, "other@example.org")
	_, edited := calendarFixture(t, st, "exception", `{"id":"exception","iCalUID":"edited@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "other@example.org")
	_, note := projectionFixture(t, st, "moved-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"edited@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: edited})
	reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(t, err)
	assert.Equal(t, forward.ByTranscript[note], reverse.ByTranscript[note])
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
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: series}) + "@20270314T100000Z"
	got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	assert.Len(got.ByTranscript, 1)
	assert.LessOrEqual(reads, 12)
}

func TestMeetingIdentityLongRunningDailySeriesWithNotes(t *testing.T) {
	for _, tc := range []struct {
		name, zone, format string
		seriesCount        int
	}{
		{"timed", "", "twenty_json", 4},
		{"all day/UTC", "UTC", "twenty_json", 2},
		{"all day/Asia/Tokyo", "Asia/Tokyo", "twenty_json", 2},
		{"all day/America/New_York", "America/New_York", "twenty_json", 2},
		{"Granola", "", "granola_json", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			zone, hour, keyFormat := time.UTC, 10, "20060102T150405Z"
			if tc.zone != "" {
				var err error
				zone, err = time.LoadLocation(tc.zone)
				require.NoError(err)
				hour, keyFormat = 0, "20060102"
			}
			var refs []string
			var notes []int64
			for _, name := range []string{"first", "second", "third", "fourth"}[:tc.seriesCount] {
				uid := name + "@example.org"
				raw := fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2000-01-01T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2000-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, name, uid)
				if tc.zone != "" {
					raw = fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"date":"2000-01-01"},"end":{"date":"2000-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`, name, uid)
				}
				data, series := calendarFixture(t, st, name, raw)
				if tc.zone != "" {
					require.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, fmt.Sprintf(`{"time_zone":%q}`, tc.zone)))
				}
				for day := 1; day <= 31; day++ {
					at := time.Date(2027, 3, day, hour, 0, 0, 0, zone)
					key := at.Format(keyFormat)
					raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":%q}}`, uid, at.UTC().Format(time.RFC3339))
					if tc.format == "granola_json" {
						raw = fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":%q}}`, name+"_"+key, at.Format(time.RFC3339))
					}
					_, note := projectionFixture(t, st, fmt.Sprintf("%s-note-%d", name, day), tc.format, raw)
					refs = append(refs, fmt.Sprintf("evt:%d@%s", series, key))
					notes = append(notes, note)
				}
			}
			for _, requested := range [][]string{refs, nil} {
				got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: requested, TranscriptIDs: notes})
				require.NoError(err)
				require.Len(got.ByOccurrence, len(refs))
				require.Len(got.ByTranscript, len(notes))
				for i, ref := range refs {
					assert.Equal("occ:"+ref, got.ByOccurrence[ref].Key)
					assert.Equal([]int64{notes[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
					assert.Equal(ref, got.ByTranscript[notes[i]].OccurrenceRef)
				}
			}
		})
	}
}

func TestMeetingIdentityUIDOverlappingOccurrencesStayAmbiguous(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	calendarFixture(t, st, "moved", `{"id":"moved","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := identityNote(t, st, "note", "weekly@example.org")
	refs := []string{fmt.Sprintf("evt:%d@20270314T100000Z", series), fmt.Sprintf("evt:%d@20270321T100000Z", series)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal(fmt.Sprintf("msg:%d", note), got.ByTranscript[note].Key)
	assert.Empty(got.ByTranscript[note].ExactOccurrenceRefs)
}

func TestMeetingIdentityCompletesNewManualGroups(t *testing.T) {
	for _, mode := range []string{"manual chain", "transcript only with split"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "manual chain":
				assert, require := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
				_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
				_, note := identityNote(t, st, "note", "")
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: one}))
				require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: two}))
				refs := []string{formatMeetingOccurrence(OccurrenceRef{MessageID: one}), formatMeetingOccurrence(OccurrenceRef{MessageID: two})}
				got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{note}})
				require.NoError(err)
				assert.ElementsMatch(refs, got.ByTranscript[note].ExactOccurrenceRefs)
				for _, ref := range refs {
					assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
				}
				_, three := calendarFixture(t, st, "three", singleLinkRaw("three", "three@example.org"))
				first := note
				_, second := identityNote(t, st, "second", "")
				_, deferred := identityNote(t, st, "deferred", "one@example.org")
				for _, link := range []struct{ note, event int64 }{{second, two}, {second, three}, {deferred, three}} {
					require.NoError(st.LinkMeetingContext(t.Context(), link.note, OccurrenceRef{MessageID: link.event}))
				}
				got, err = st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: one})}})
				require.NoError(err)
				assert.Equal([]int64{first, deferred}, got.ByOccurrence[refs[0]].LinkedTranscriptIDs)
				assert.Len(got.ByOccurrence, 1, "discovered notes do not expand the requested meetings")
				assert.NotContains(got.ByTranscript, second)
				assert.ElementsMatch(refs, got.ByTranscript[first].ExactOccurrenceRefs)
				assert.Equal(fmt.Sprintf("msg:%d", first), got.ByTranscript[first].Key)
			case "transcript only with split":
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
					assert.Equal([]int64{one, two}, got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: event})].LinkedTranscriptIDs)
				}
			}
		})
	}
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
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: event}), got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]int64{note}, got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: event})].LinkedTranscriptIDs)
}

func TestMeetingIdentityBatchReadBound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxReads [3]int
	}{
		{"twenty", [3]int{8, 8, 7}},
		{"granola", [3]int{9, 9, 8}},
		{"recurring", [3]int{12, 12, 11}},
		{"merged historical", [3]int{7, 7, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st *Store
			var req MeetingIdentityRequest
			var liveRefs []string
			switch tc.name {
			case "merged historical":
				st = newRFC822IDBackfillBackendStore(t)
				req = MeetingIdentityRequest{}
				var sourceID int64
				for i := range 20 {
					key := fmt.Sprintf("event-%d", i)
					data, event := calendarFixture(t, st, "historical-"+key, singleLinkRaw(key, ""), "historical@example.org")
					sourceID = data.Message.SourceID
					_, note := identityNote(t, st, fmt.Sprintf("note-%d", i), "")
					require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
					req.OccurrenceRefs = append(req.OccurrenceRefs, formatMeetingOccurrence(OccurrenceRef{MessageID: event}))
					req.TranscriptIDs = append(req.TranscriptIDs, note)
				}
				destination, err := st.GetOrCreateSource("gcal", "live@example.org")
				require.NoError(t, err)
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: sourceID, IntoSourceID: destination.ID})
				require.NoError(t, err)
				liveRefs = []string{}
				for i := range 20 {
					key := fmt.Sprintf("event-%d", i)
					_, event := calendarFixture(t, st, "live-"+key, singleLinkRaw(key, ""), "live@example.org")
					liveRefs = append(liveRefs, formatMeetingOccurrence(OccurrenceRef{MessageID: event}))
				}
			case "recurring":
				st = newRFC822IDBackfillBackendStore(t)
				_, series := calendarFixture(t, st, "series", strings.Replace(linkSeriesRaw, "COUNT=6", "COUNT=24", 1))
				req = MeetingIdentityRequest{}
				for i := range 20 {
					at := time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i*7)
					key := at.Format("20060102T150405Z")
					_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q}}`, "series_"+key))
					req.OccurrenceRefs = append(req.OccurrenceRefs, formatMeetingOccurrence(OccurrenceRef{MessageID: series, Key: key}))
					req.TranscriptIDs = append(req.TranscriptIDs, note)
				}
			default:
				format := "twenty_json"
				if tc.name == "granola" {
					format = "granola_json"
				}
				st = newRFC822IDBackfillBackendStore(t)
				req = MeetingIdentityRequest{}
				for i := range 20 {
					_, id := calendarFixture(t, st, fmt.Sprintf("event-%d", i), singleLinkRaw(fmt.Sprintf("event-%d", i), fmt.Sprintf("event-%d@example.org", i)))
					key := fmt.Sprintf("event-%d", i)
					raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, key+"@example.org")
					if tc.name == "granola" {
						raw = fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q}}`, key)
					}
					_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), format, raw)
					req.OccurrenceRefs = append(req.OccurrenceRefs, formatMeetingOccurrence(OccurrenceRef{MessageID: id}))
					req.TranscriptIDs = append(req.TranscriptIDs, note)
				}
			}
			for modeIndex, mode := range []string{"both", "events", "notes"} {
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
						if tc.name == "merged historical" {
							ref = liveRefs[i]
						}
						assert.Equal([]int64{req.TranscriptIDs[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
						if tc.name == "recurring" || tc.name == "merged historical" {
							assert.Equal(ref, got.ByTranscript[req.TranscriptIDs[i]].OccurrenceRef)
						}
						if tc.name == "merged historical" && mode != "notes" {
							assert.Equal(got.ByOccurrence[ref], got.ByOccurrence[req.OccurrenceRefs[i]])
						}
					}
					assert.LessOrEqual(reads, tc.maxReads[modeIndex])
					if tc.name == "granola" {
						assert.Equal(1, sourceReads)
					}
				})
			}
		})
	}
}

func TestMeetingIdentityZonedReverseDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "zoned", `{"id":"zoned","iCalUID":"zoned@example.org","start":{"dateTime":"2027-04-12T10:00:00-04:00","timeZone":"America/Toronto"},"end":{"dateTime":"2027-04-12T11:00:00-04:00","timeZone":"America/Toronto"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "zoned-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zoned@example.org","startsAt":"2027-04-12T14:00:00Z"}}`)
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event}) + "@20270412T140000Z"
	forward, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(forward, 1)
	require.Equal(ref, forward[0].Ref)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentityUnidentifiedExceptionDoesNotAbsorbSingle(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "exception", `{"id":"exception","iCalUID":"shared@example.org","recurringEventId":"missing","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "shared@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: exception}), formatMeetingOccurrence(OccurrenceRef{MessageID: single})}, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: single}), got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]int64{note}, got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: single})].LinkedTranscriptIDs)
	assert.Empty(got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: exception})].LinkedTranscriptIDs)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: single}), links[0].Ref)
}

func TestMeetingIdentityReverseSavedTargetAfterLiveCopyArrives(t *testing.T) {
	t.Run("calendar merge", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		historical, event := calendarFixture(t, st, "historical", singleLinkRaw("event", ""), "historical@example.org")
		noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
		require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
		destination, err := st.GetOrCreateSource("gcal", "live@example.org")
		require.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: historical.Message.SourceID, IntoSourceID: destination.ID})
		require.NoError(err)
		_, live := calendarFixture(t, st, "live", singleLinkRaw("event", ""), "live@example.org")
		_, unrelated := calendarFixture(t, st, "unrelated", singleLinkRaw("event", ""), "unrelated@example.org")
		_, otherNote := projectionFixture(t, st, "other-note", "plaud_json", `{}`)
		require.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: unrelated}))
		links, err := meetingLinksForTest(t, st, note)
		require.NoError(err)
		require.Len(links, 1)
		assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: live}), links[0].Ref)
		for _, ctx := range []context.Context{t.Context(), sourcescope.WithSourceIDs(t.Context(), []int64{destination.ID, noteData.Message.SourceID})} {
			got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{OccurrenceRefs: []string{formatMeetingOccurrence(OccurrenceRef{MessageID: live})}})
			require.NoError(err)
			assert.Equal([]int64{note}, got.ByOccurrence[formatMeetingOccurrence(OccurrenceRef{MessageID: live})].LinkedTranscriptIDs)
			assert.NotContains(got.ByTranscript, otherNote)
			record, err := st.MeetingRecordContext(ctx, MeetingLinkRef{Kind: "evt", MessageID: live})
			require.NoError(err)
			assert.Equal([]int64{note}, record.MessageIDs)
		}
	})
	for _, mode := range []string{"automatic", "saved", "new decision"} {
		t.Run("note merge/"+mode, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
			var data *MessagePersistData
			var note int64
			if mode == "automatic" {
				data, note = identityNote(t, st, "unique-note", "merge@example.org")
			} else {
				data, note = projectionFixture(t, st, "unique-note", "meeting_json", meetingProjectionRaw)
			}
			target := OccurrenceRef{MessageID: event}
			if mode == "saved" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, target))
			}
			destination, err := st.GetOrCreateSource("meeting_import", "empty@example.com")
			requirements.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: data.Message.SourceID, IntoSourceID: destination.ID})
			requirements.NoError(err)
			ids := []int64{note}
			if mode == "new decision" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, target))
			}
			ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event})
			for _, transcriptIDs := range [][]int64{ids, nil} {
				got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: transcriptIDs})
				requirements.NoError(err)
				assertions.Equal("occ:"+ref, got.ByTranscript[note].Key)
				assertions.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
			// Grant visibility follows the destination, not the retired source.
			for _, sourceID := range []int64{data.Message.SourceID, destination.ID} {
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{sourceID, eventData.Message.SourceID})
				got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: ids})
				requirements.NoError(err)
				if sourceID == destination.ID {
					assertions.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
				} else {
					assertions.Empty(got.ByTranscript)
					assertions.Empty(got.ByOccurrence[ref].LinkedTranscriptIDs)
				}
			}
			if mode == "automatic" {
				_, err = st.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, note)
				requirements.NoError(err)
				got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: ids})
				requirements.NoError(err)
				assertions.Empty(got.ByTranscript)
				assertions.Empty(got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
		})
	}
}

func TestMeetingIdentityScopedGranolaAfterSourceMerge(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	history, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
	noteData, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	destination, err := st.GetOrCreateSource("gcal", "destination@example.com")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
	require.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{destination.ID, noteData.Message.SourceID})
	ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event})
	got, err := st.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Equal(ref, got.ByTranscript[note].OccurrenceRef)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
}

func TestMeetingIdentitySparseUIDBatchKeepsOccurrenceEvidence(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", strings.ReplaceAll(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, ""))
	calendarFixture(t, st, "sparse", `{"id":"sparse","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"summary":"Edited title"}`)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "other@example.org")
	edited := fmt.Sprintf("evt:%d@20270314T100000Z", master)
	second := fmt.Sprintf("evt:%d@20270321T100000Z", master)
	copyRef := fmt.Sprintf("evt:%d@20270314T100000Z", copyID)
	for _, tc := range []struct {
		name string
		refs []string
	}{
		{"edited date alone", []string{edited, copyRef}},
		{"edited date first", []string{edited, second, copyRef}},
		{"edited date last", []string{second, edited, copyRef}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: tc.refs})
			require.NoError(t, err)
			assert.Equal("occ:"+edited, got.ByOccurrence[edited].Key)
			assert.Equal("occ:"+edited, got.ByOccurrence[copyRef].Key)
		})
	}
}

func TestMeetingIdentitySparseUIDFallbackCopiesMerge(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", strings.ReplaceAll(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, ""))
	calendarFixture(t, st, "sparse", `{"id":"sparse","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"summary":"Edited title"}`)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "other@example.org")
	_, manual := identityNote(t, st, "manual", "")
	_, automatic := identityNote(t, st, "automatic", "weekly@example.org")
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	require.NoError(st.LinkMeetingContext(t.Context(), manual, ref))
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: manual})
	require.NoError(err)
	assert.Equal([]int64{manual, automatic}, record.MessageIDs)
	refs := []string{formatMeetingOccurrence(ref), fmt.Sprintf("evt:%d@20270314T100000Z", copyID)}
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{manual, automatic}})
	require.NoError(err)
	assert.Equal(got.ByOccurrence[refs[0]].Key, got.ByOccurrence[refs[1]].Key)
	for _, ref := range refs {
		assert.Equal([]int64{manual, automatic}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}
