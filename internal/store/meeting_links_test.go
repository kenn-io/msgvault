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

const linkSeriesRaw = `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`

func calendarLinkFixture(t *testing.T, st *Store, calendar, raw string) (int64, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource("google_calendar", calendar)
	require.NoError(t, err)
	var event struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &event))
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: event.ID, MessageType: meetingLinkCalendarType, Subject: sql.NullString{String: "Project planning", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: event.ID, ConversationType: meetingLinkCalendarType, Title: "Project planning"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: fmt.Sprintf(`{"calendar_id":%q}`, calendar), Valid: true},
	}
	id, err := st.PersistMessage(data)
	require.NoError(t, err)
	return id, source.ID
}

func singleLinkRaw(id, uid, start, end string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":%q},"end":{"dateTime":%q}}`, id, uid, start, end)
}
func granolaLinkFixture(t *testing.T, st *Store, id string) int64 {
	t.Helper()
	_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":"2027-03-14T10:00:00Z"}}`, id))
	return note
}
func memberLinks(t *testing.T, st *Store, scope SourceScope, id int64) []EffectiveLink {
	t.Helper()
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	require.NoError(t, err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := linksForMember(ctx, st.snapshotReader(ctx), scope, id, &budget)
	require.NoError(t, err)
	return links
}
func linkTargets(links []EffectiveLink) []OccurrenceRef {
	got := make([]OccurrenceRef, 0, len(links))
	for _, link := range links {
		got = append(got, link.Target)
	}
	return got
}

func TestLinkGranolaInstanceIDToOccurrence(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	links := memberLinks(t, st, OwnerScope(), note)
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(links))
	require.Len(t, links, 1)
	assert.Equal(t, "instance_id", links[0].Basis)
}

func TestLinkGranolaIDToExceptionAnchorsSeries(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "edited")
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestNoLinkExdatedWithoutException(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1))
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestLinkExceptionBeatsExdate(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1))
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestLinkTwentyUIDByScheduledStartUnique(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		moved, want bool
	}{
		{"exact", "2027-03-14T10:00:00Z", false, true}, {"one-minute", "2027-03-14T10:01:00Z", false, true},
		{"two-minutes", "2027-03-14T10:02:00Z", false, false}, {"moved", "2027-03-14T12:00:00Z", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
			if tc.moved {
				calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
			}
			_, note := projectionFixture(t, st, "twenty", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, tc.start))
			links := memberLinks(t, st, OwnerScope(), note)
			if tc.want {
				assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(links))
			} else {
				assert.Empty(t, links)
			}
		})
	}
}

func TestSourceDeletionDoesNotRetargetMeetingDecisionAfterIDReuse(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, note := projectionFixture(t, st, "manual-note", "meeting_json", `{}`)
	oldTarget, oldSource := calendarLinkFixture(t, st, "old-calendar@example.org", singleLinkRaw("old", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", oldTarget), note))
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM sources WHERE id = ?`), oldSource)
	requirements.NoError(err)

	newTarget, _ := calendarLinkFixture(t, st, "new-calendar@example.org", singleLinkRaw("new", "different@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	if !st.IsPostgreSQL() {
		assertions.Equal(oldTarget, newTarget, "SQLite should reuse the deleted highest message ID in this regression case")
	}
	assertions.Empty(memberLinks(t, st, OwnerScope(), note), "a decision for a deleted calendar source must not attach to an unrelated event that reuses its ID")
}

func TestMeetingLinkDecisionTargetsUpgradeAndBackfill(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	target, _ := calendarLinkFixture(t, st, "calendar@example.org", singleLinkRaw("legacy-target", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, note := projectionFixture(t, st, "legacy-note", "meeting_json", `{}`)
	_, err := st.DB().Exec(st.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision) VALUES (?, ?, '', 'link')`), note, fmt.Sprintf("evt:%d", target))
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DROP INDEX idx_meeting_link_decisions_target_message_id`))
	requirements.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(st.Rebind(`ALTER TABLE meeting_link_decisions DROP COLUMN target_message_id CASCADE`))
	} else {
		for _, name := range []string{"idx_meeting_link_decisions_ref", "idx_meeting_link_decisions_logical"} {
			_, err = st.DB().Exec(st.Rebind(`DROP INDEX ` + name))
			requirements.NoError(err)
		}
		_, err = st.DB().Exec(st.Rebind(`ALTER TABLE meeting_link_decisions RENAME TO meeting_link_decisions_legacy`))
		requirements.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`CREATE TABLE meeting_link_decisions (
			member_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			target_ref TEXT NOT NULL,
			target_logical TEXT NOT NULL DEFAULT '',
			decision TEXT NOT NULL CHECK(decision IN ('link','split')),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(member_message_id,target_ref)
		)`))
		requirements.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) SELECT member_message_id,target_ref,target_logical,decision,created_at FROM meeting_link_decisions_legacy`))
		requirements.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`DROP TABLE meeting_link_decisions_legacy`))
	}
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migrationMeetingLinkDecisionTargetsV1)
	requirements.NoError(err)

	requirements.NoError(st.InitSchemaContext(t.Context()))
	var backfilled int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`), note).Scan(&backfilled))
	requirements.Equal(target, backfilled)
}

func TestLinkDecisionSkipsUnsupportedHistoricalRecurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	oldRaw := `{"id":"old-series","iCalUID":"old@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`
	oldTarget, _ := calendarLinkFixture(t, st, "old-calendar@example.org", oldRaw)
	_, note := projectionFixture(t, st, "unsupported-history-note", "meeting_json", `{}`)
	oldRef := FormatOccurrenceRef(OccurrenceRef{MessageID: oldTarget, Key: "20270314T100000Z"})
	requirements.NoError(st.LinkMeetingContext(t.Context(), oldRef, note))

	unsupportedRaw := strings.Replace(oldRaw, "RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=HOURLY;COUNT=2", 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(oldTarget, []byte(unsupportedRaw), "gcal_json"))

	newTarget, _ := calendarLinkFixture(t, st, "new-calendar@example.org", singleLinkRaw("new-event", "new@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	newRef := fmt.Sprintf("evt:%d", newTarget)
	requirements.NoError(st.LinkMeetingContext(t.Context(), newRef, note))
	assertions.Contains(linkTargets(memberLinks(t, st, OwnerScope(), note)), OccurrenceRef{MessageID: newTarget})
}

func TestTwoCalendarCopiesSameUIDLinkBoth(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	a, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	b, _ := calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assertions.ElementsMatch([]OccurrenceRef{OccurrenceRef{MessageID: a, Key: "20270314T100000Z"}, OccurrenceRef{MessageID: b, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	key, err := logicalMeetingKey(ctx, st.snapshotReader(ctx), OwnerScope(), note, &budget)
	requirements.NoError(err)
	assertions.Equal("ical:weekly@example.org@20270314T100000Z", key)
}

func TestSplitSupersedesOlderEquivalentUserLinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	a, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	b, _ := calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	refA := fmt.Sprintf("evt:%d@20270314T100000Z", a)
	refB := fmt.Sprintf("evt:%d@20270314T100000Z", b)

	requirements.NoError(st.LinkMeetingContext(context.Background(), refA, note))
	requirements.NoError(st.LinkMeetingContext(context.Background(), refB, note))
	assertions.ElementsMatch(
		[]OccurrenceRef{{MessageID: a, Key: "20270314T100000Z"}, {MessageID: b, Key: "20270314T100000Z"}},
		linkTargets(memberLinks(t, st, OwnerScope(), note)),
	)

	requirements.NoError(st.UnlinkMeetingContext(context.Background(), refA, note))
	assertions.Empty(memberLinks(t, st, OwnerScope(), note), "the split must supersede older links to UID-equivalent copies")

	requirements.NoError(st.LinkMeetingContext(context.Background(), refB, note))
	assertions.ElementsMatch(
		[]OccurrenceRef{{MessageID: b, Key: "20270314T100000Z"}},
		linkTargets(memberLinks(t, st, OwnerScope(), note)),
	)
}

func TestLateOccurrenceEquivalenceLetsNewerSplitOverrideOlderLink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	oldTarget, oldSource := calendarLinkFixture(t, st, "old-calendar@example.org", singleLinkRaw("old", "", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	newTarget, newSource := calendarLinkFixture(t, st, "new-calendar@example.org", singleLinkRaw("new", "", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, note := projectionFixture(t, st, "manual-note", "meeting_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", oldTarget), note))
	for _, event := range []struct {
		id, calendar string
		source       int64
	}{{"old", "old-calendar@example.org", oldSource}, {"new", "new-calendar@example.org", newSource}} {
		data := &MessagePersistData{
			Message:      &Message{SourceID: event.source, SourceMessageID: event.id, MessageType: meetingLinkCalendarType, Subject: sql.NullString{String: "Project planning", Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: event.id, ConversationType: meetingLinkCalendarType, Title: "Project planning"},
			RawMIME:      []byte(singleLinkRaw(event.id, "shared@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z")),
			RawFormat:    "gcal_json",
			Metadata:     &sql.NullString{String: fmt.Sprintf(`{"calendar_id":%q}`, event.calendar), Valid: true},
		}
		_, err := st.PersistMessage(data)
		requirements.NoError(err)
	}
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", newTarget), note))
	assertions.Empty(memberLinks(t, st, OwnerScope(), note), "a newer split must suppress an older explicit link after the calendar copies become equivalent")

	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", newTarget), note))
	assertions.ElementsMatch([]OccurrenceRef{{MessageID: newTarget}}, linkTargets(memberLinks(t, st, OwnerScope(), note)), "a later link must override the split without reviving an older link to the other copy")
}

func TestSameProviderIDDifferentUIDAmbiguous(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "conflict@example.org", strings.Replace(linkSeriesRaw, "weekly@example.org", "other@example.org", 1))
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestScopedLinkIgnoresHiddenConflict(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, source := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "conflict@example.org", strings.Replace(linkSeriesRaw, "weekly@example.org", "other@example.org", 1))
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	var noteSource int64
	require.NoError(t, st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&noteSource))
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, SourceScope{IDs: []int64{source, noteSource}}, note)))
	assert.Empty(t, memberLinks(t, st, SourceScope{IDs: []int64{source}}, note))
}

func TestLateMasterFindsInstanceKeyedNote(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assertions.Empty(memberLinks(t, st, OwnerScope(), note))
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(note, links[0].MemberMessageID)
}

func TestLinkFunctionsRespectBudget(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	require.NoError(t, err)
	defer release()
	budget := 0
	_, err = linksForMember(ctx, st.snapshotReader(ctx), OwnerScope(), note, &budget)
	require.ErrorIs(t, err, ErrQueryTooBroad)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	budget = calrecur.RequestReadBudget
	_, err = linksForMember(ctx, st.snapshotReader(ctx), OwnerScope(), note, &budget)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUserLinkExclusive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	other, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("other", "other@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", other), note))
	links := memberLinks(t, st, OwnerScope(), note)
	assertions.ElementsMatch([]OccurrenceRef{OccurrenceRef{MessageID: other}}, linkTargets(links))
	requirements.Len(links, 1)
	assertions.Equal("user", links[0].Origin)
}

func TestHiddenUserLinkDoesNotAffectScopedResult(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, source := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	other, _ := calendarLinkFixture(t, st, "hidden@example.org", singleLinkRaw("other", "other@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	require.NoError(t, st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", other), note))
	var noteSource int64
	require.NoError(t, st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&noteSource))
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, SourceScope{IDs: []int64{source, noteSource}}, note)))
}

func TestLogicalSplitVetoesLaterCopy(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	require.NoError(t, st.UnlinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", master), note))
	calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestLinkReplacesSplit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ref := fmt.Sprintf("evt:%d@20270314T100000Z", master)
	require.NoError(t, st.UnlinkMeetingContext(context.Background(), ref, note))
	require.NoError(t, st.LinkMeetingContext(context.Background(), ref, note))
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestOrphanExceptionAnchorThenMasterArrives(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "edited")
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: orphan}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestParsedSeriesOccurrenceLinksOrphanExceptionInBothDirections(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assertions.Equal([]OccurrenceRef{{MessageID: orphan}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: orphan}, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(note, links[0].MemberMessageID)
	release()

	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	expected := []OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}}
	assertions.Equal(expected, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	ctx, release, err = st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget = calrecur.RequestReadBudget
	links, err = membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), expected[0], &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(note, links[0].MemberMessageID)
}

func TestUnkeyedNoteManualLinkVisibleFromEventAndPrevious(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "unkeyed", "plaud_json", `{"summary":"Manual evidence"}`)
	require.NoError(t, st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), note))
	check := func(ref OccurrenceRef) {
		ctx, release, err := st.BeginReadSnapshotContext(context.Background())
		require.NoError(t, err)
		defer release()
		budget := calrecur.RequestReadBudget
		links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), ref, &budget)
		require.NoError(t, err)
		require.Len(t, links, 1)
		assert.Equal(t, note, links[0].MemberMessageID)
	}
	check(OccurrenceRef{MessageID: orphan})
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	check(OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})
}

func TestHighlySharedKeyBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw(fmt.Sprintf("shared-%d", i), "shared@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	}
	_, note := projectionFixture(t, st, "shared", "twenty_json", `{"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Empty(memberLinks(t, st, OwnerScope(), note))
	suggestions, err := st.SuggestMeetingLinksContext(context.Background(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.NotEmpty(suggestions)
	assertions.Contains(suggestions[0].Reasons, "too_many_candidates")
}

func TestJoinKeyBackToBackLinksOnlyOne(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	a, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(singleLinkRaw("a", "a@example.org", "2027-03-14T10:00:00Z", "2027-03-14T10:30:00Z"), `"start":`, `"hangoutLink":"https://meet.google.com/abc-defg-hij","start":`, 1))
	calendarLinkFixture(t, st, "primary@example.com", strings.Replace(singleLinkRaw("b", "b@example.org", "2027-03-14T10:30:00Z", "2027-03-14T11:00:00Z"), `"start":`, `"hangoutLink":"https://meet.google.com/abc-defg-hij","start":`, 1))
	_, note := projectionFixture(t, st, "circle", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/abc-defg-hij","date":"2027-03-14T10:00:00Z"}}`)
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: a}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestJoinKeyAmbiguousNoLink(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for _, id := range []string{"a", "b"} {
		calendarLinkFixture(t, st, "primary@example.com", strings.Replace(singleLinkRaw(id, id+"@example.org", "2027-03-14T10:00:00Z", "2027-03-14T10:30:00Z"), `"start":`, `"hangoutLink":"https://meet.google.com/abc-defg-hij","start":`, 1))
	}
	_, note := projectionFixture(t, st, "circle", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/abc-defg-hij","date":"2027-03-14T10:00:00Z"}}`)
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestDecisionConsolidationEffectiveLinkNewestWins(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	target, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("target", "target@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	legacyData, legacy := projectionFixture(t, st, "legacy", "granola_json", `{}`)
	_, survivor := projectionFixture(t, st, "canonical", "granola_json", `{}`)
	ref := fmt.Sprintf("evt:%d", target)
	requirements.NoError(st.LinkMeetingContext(context.Background(), ref, legacy))
	requirements.NoError(st.UnlinkMeetingContext(context.Background(), ref, survivor))
	_, err := st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ?`, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), legacy)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ?`, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), survivor)
	requirements.NoError(err)
	requirements.NoError(st.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy", "canonical"))
	links := memberLinks(t, st, OwnerScope(), survivor)
	assertions.ElementsMatch([]OccurrenceRef{OccurrenceRef{MessageID: target}}, linkTargets(links))
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, legacy).Scan(&count))
	assertions.Zero(count)
}

func TestLinkBudgetAllowsShortSeries(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := 100
	links, err := linksForMember(ctx, st.snapshotReader(ctx), OwnerScope(), note, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assert.Positive(t, budget)
}

func TestOrphanSplitVetoesCopyAfterMasterArrives(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "twenty-orphan", "twenty_json", `{"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	require.NoError(t, st.UnlinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), note))
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	calendarLinkFixture(t, st, "copy@example.org", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestOrphanSplitWithoutUIDVetoesEquivalentCopyAfterMasterArrives(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "twenty-orphan-no-uid", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	require.NoError(st.UnlinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), note))
	var logical string
	require.NoError(st.db.QueryRow(`SELECT target_logical FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&logical))
	assert.Empty(logical)

	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	for _, calendar := range []string{"primary@example.com", "copy@example.org"} {
		calendarLinkFixture(t, st, calendar, `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	}

	assert.Empty(memberLinks(t, st, OwnerScope(), note), "an orphan split without a stored UID must still veto newly visible equivalent copies")
}

func TestStandaloneAllDayOccurrenceUsesSkippedMidnightBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, startDate, endDate, wantStart, wantEnd string
	}{
		{"start on skipped midnight", "2018-11-04", "2018-11-05", "2018-11-04T03:00:00Z", "2018-11-05T02:00:00Z"},
		{"exclusive end on skipped midnight", "2018-11-03", "2018-11-04", "2018-11-03T03:00:00Z", "2018-11-04T03:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source, err := st.GetOrCreateSource("google_calendar", "primary@example.com")
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncConfig(source.ID, `{"time_zone":"America/Sao_Paulo"}`))
			raw := fmt.Sprintf(`{"id":"all-day-boundary","iCalUID":"boundary@example.org","start":{"date":%q},"end":{"date":%q}}`, tc.startDate, tc.endDate)
			target, _ := calendarLinkFixture(t, st, "primary@example.com", raw)

			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			require.NoError(err)
			budget := calrecur.RequestReadBudget
			got, err := resolveLinkOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: target}, &budget)
			release()
			require.NoError(err)
			assert.Equal(tc.wantStart, got.Start.UTC().Format(time.RFC3339))
			assert.Equal(tc.wantEnd, got.End.UTC().Format(time.RFC3339))
		})
	}
}

func TestJoinKeyExceptionOverride(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(linkSeriesRaw, `"start":`, `"hangoutLink":"https://meet.google.com/old-code-aaa","start":`, 1))
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","hangoutLink":"https://meet.google.com/new-code-bbb","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "join-override", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/new-code-bbb","date":"2027-03-14T12:00:00Z"}}`)
	assert.ElementsMatch(t, []OccurrenceRef{OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	_, oldNote := projectionFixture(t, st, "old-join-override", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/old-code-aaa","date":"2027-03-14T12:00:00Z"}}`)
	assert.Empty(t, memberLinks(t, st, OwnerScope(), oldNote))
}

func TestCompleteExceptionClearsJoinAndAttendeeEvidence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	masterRaw := strings.Replace(linkSeriesRaw, `"start":`, `"hangoutLink":"https://meet.google.com/old-code-aaa","attendees":[{"email":"guest@example.org","responseStatus":"accepted"}],"start":`, 1)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", masterRaw)
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "cleared-exception", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/old-code-aaa","date":"2027-03-14T12:05:00Z"}}`)
	at := time.Date(2027, 3, 14, 12, 15, 0, 0, time.UTC)
	_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, at, note)
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("guest@example.org", "Guest Example", "example.org")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant}, []string{"Guest Example"}))

	assertions.Empty(memberLinks(t, st, OwnerScope(), note), "a complete exception with an empty join URL must clear the master's key")
	reverse, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}), 10)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(note, reverse[0].MemberMessageID)
	assertions.Zero(reverse[0].SharedAttendees, "a complete exception with no attendees must clear the master's attendees")
	assertions.NotContains(reverse[0].Reasons, "shared_attendees")

	calendarLinkFixture(t, st, "primary@example.com", `{"id":"sparse","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"}}`)
	_, sparseNote := projectionFixture(t, st, "sparse-exception", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/old-code-aaa","date":"2027-03-21T10:05:00Z"}}`)
	sparseAt := time.Date(2027, 3, 21, 10, 5, 0, 0, time.UTC)
	_, err = st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, sparseAt, sparseNote)
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(sparseNote, "to", []int64{participant}, []string{"Guest Example"}))
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270321T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), sparseNote)))
	sparseReverse, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270321T100000Z"}), 10)
	requirements.NoError(err)
	requirements.Len(sparseReverse, 1)
	assertions.Equal(1, sparseReverse[0].SharedAttendees)
}

func TestExactProviderKeyDoesNotFetchUnrelatedSingles(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw(fmt.Sprintf("unrelated-%d", i), fmt.Sprintf("unrelated-%d@example.org", i), "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	}
	target, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("target", "target@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "target")
	assert.ElementsMatch(t, []OccurrenceRef{{MessageID: target}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestHiddenDecisionCandidatesDoNotCrowdVisibleInverse(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	visible, eventSource := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("visible", "copies@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	hidden, _ := calendarLinkFixture(t, st, "hidden@example.org", singleLinkRaw("hidden", "copies@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	for i := range 201 {
		_, note := projectionFixture(t, st, fmt.Sprintf("hidden-decision-%d", i), "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", hidden), note))
	}
	data, note := projectionFixture(t, st, "visible-decision", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", visible), note))
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), SourceScope{IDs: []int64{eventSource, data.Message.SourceID}}, OccurrenceRef{MessageID: visible}, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assert.Equal(t, note, links[0].MemberMessageID)
}

func TestInverseIgnoresOtherInstanceKeys(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	for i := range 201 {
		projectionFixture(t, st, fmt.Sprintf("other-instance-%d", i), "granola_json", `{"calendar_event":{"calendar_event_id":"series_20270307T100000Z"}}`)
	}
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assert.Equal(t, note, links[0].MemberMessageID)
}

func TestHiddenDecisionsDoNotSpendVisibleBudget(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	visible, source := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("visible", "visible@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "visible")
	for i := range 6 {
		hidden, _ := calendarLinkFixture(t, st, "hidden@example.org", singleLinkRaw(fmt.Sprintf("hidden-%d", i), "hidden@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
		requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", hidden), note))
	}
	var noteSource int64
	requirements.NoError(st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&noteSource))
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := 6
	links, err := linksForMember(ctx, st.snapshotReader(ctx), SourceScope{IDs: []int64{source, noteSource}}, note, &budget)
	requirements.NoError(err)
	assert.Equal(t, []OccurrenceRef{{MessageID: visible}}, linkTargets(links))
}

func TestLinkGranolaAllDayInstanceKeepsCivilDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, source := calendarLinkFixture(t, st, "primary@example.com", `{"id":"civil","iCalUID":"civil@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"America/Los_Angeles"}`))
	note := granolaLinkFixture(t, st, "civil_20270314")
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"Pacific/Kiritimati"}`))
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestMeetingDecisionRejectsInvalidMemberAndTarget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	_, note := projectionFixture(t, st, "manual-invalid", "plaud_json", `{}`)
	for _, ref := range []string{fmt.Sprintf("msg:%d", note), fmt.Sprintf("evt:%d", master), fmt.Sprintf("evt:%d@20270401T100000Z", master), "evt:999999"} {
		requirements.Error(st.LinkMeetingContext(context.Background(), ref, note))
	}
	requirements.Error(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", master), master))
	var count int
	require.NoError(t, st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
	assertions.Zero(count)
}

func TestLinkAllDayMovedExceptionUsesCurrentZone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, source := calendarLinkFixture(t, st, "primary@example.com", `{"id":"civil-move","iCalUID":"civil-move@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"Pacific/Kiritimati"}`))
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"civil-edited","recurringEventId":"civil-move","iCalUID":"civil-move@example.org","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, manual := projectionFixture(t, st, "manual-civil", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), manual))
	requirements.NoError(st.UpdateSourceSyncConfig(source, `{"time_zone":"Pacific/Honolulu"}`))
	note := granolaLinkFixture(t, st, "civil-move_20270314")
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314"}}, linkTargets(memberLinks(t, st, OwnerScope(), manual)))
	note = granolaLinkFixture(t, st, "civil-edited")
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestInverseDecisionLimitCountsDistinctMembers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	secondRaw := `{"id":"edited-second","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	second, _ := calendarLinkFixture(t, st, "primary@example.com", secondRaw)
	for i := range 102 {
		_, note := projectionFixture(t, st, fmt.Sprintf("multiple-decisions-%d", i), "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", second), note))
		requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), note))
	}
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	assertions.Len(links, 102)
	release()
	for i := 102; i < 201; i++ {
		_, note := projectionFixture(t, st, fmt.Sprintf("multiple-decisions-%d", i), "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", master), note))
	}
	ctx, release, err = st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget = calrecur.RequestReadBudget
	links, err = membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}, &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
	assertions.Nil(links)
}

func TestInverseBareRecurringMasterIDsDoNotExhaustCandidateLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	for i := range meetingLinkCandidateLimit {
		_, _ = projectionFixture(t, st, fmt.Sprintf("bare-series-%d", i), "granola_json", `{"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	}
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(note, links[0].MemberMessageID)
}

func TestUnlinkReplacesEquivalentAliasDecision(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse-%t", reverse), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
			_, note := projectionFixture(t, st, "manual-unlink-alias", "plaud_json", `{}`)
			alias := fmt.Sprintf("evt:%d", orphan)
			var master int64
			if !reverse {
				requirements.NoError(st.LinkMeetingContext(context.Background(), alias, note))
				master, _ = calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
			} else {
				master, _ = calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
				requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", master), note))
			}
			ref := fmt.Sprintf("evt:%d@20270314T100000Z", master)
			if reverse {
				ref = alias
			}
			requirements.NoError(st.UnlinkMeetingContext(context.Background(), ref, note))
			assertions.Empty(memberLinks(t, st, OwnerScope(), note))
			var count int
			requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
			assertions.Equal(1, count)
		})
	}
}

func TestCirclebackSupportedScheduledTimeLinks(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata-%t", metadata), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			target, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(singleLinkRaw("flexible", "flexible@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"), `"start":`, `"hangoutLink":"https://meet.google.com/abc-defg-hij","start":`, 1))
			raw := `{"meeting":{"meetingUrl":"https://meet.google.com/abc-defg-hij","date":"2027-03-14 10:00:00"}}`
			if metadata {
				raw = `{}`
			}
			_, note := projectionFixture(t, st, "flexible-note", "circleback_json", raw)
			if metadata {
				_, err := st.db.Exec(`UPDATE messages SET metadata = ? WHERE id = ?`, `{"platform":"circleback","meeting_url":"https://meet.google.com/abc-defg-hij","scheduled_start":"2027-03-14 10:00:00"}`, note)
				requirements.NoError(err)
				_, err = st.db.Exec(`DELETE FROM meeting_link_key_state WHERE message_id = ?`, note)
				requirements.NoError(err)
				requirements.NoError(st.backfillMeetingLinkKeysContext(context.Background()))
			}
			var at sql.NullTime
			requirements.NoError(st.db.QueryRow(`SELECT scheduled_at FROM meeting_link_keys WHERE message_id = ?`, note).Scan(&at))
			assertions.True(at.Valid)
			assertions.Equal([]OccurrenceRef{{MessageID: target}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
		})
	}
}

func TestCirclebackFallsBackToDateWhenStartTimeIsInvalid(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	target, _ := calendarLinkFixture(t, st, "primary@example.com", strings.Replace(singleLinkRaw("circleback-target", "target@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"), `"start":`, `"hangoutLink":"https://meet.google.com/abc-defg-hij","start":`, 1))
	_, note := projectionFixture(t, st, "circleback-fallback", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/abc-defg-hij","startTime":"not-a-time","date":"2027-03-14T10:00:00Z"}}`)
	requirements.NoError(st.SetMessageMetadata(note, sql.NullString{String: `{"platform":"circleback","start":"not-a-time"}`, Valid: true}))
	links := memberLinks(t, st, OwnerScope(), note)
	requirements.Len(links, 1)
	assertions.Equal([]OccurrenceRef{{MessageID: target}}, linkTargets(links))
}

func TestManualLinkIgnoresUnrelatedSeriesExceptions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	for i := range 201 {
		at := time.Date(2027, 4, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		raw := fmt.Sprintf(`{"id":"other-exception-%d","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":%q},"start":{"dateTime":%q},"end":{"dateTime":%q}}`, i, at.Format(time.RFC3339), at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339))
		calendarLinkFixture(t, st, "primary@example.com", raw)
	}
	_, note := projectionFixture(t, st, "manual-large-series", "plaud_json", `{}`)
	target := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(context.Background(), FormatOccurrenceRef(target), note))
	assertions.Equal([]OccurrenceRef{target}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	links, err := membersForOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), target, &budget)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(note, links[0].MemberMessageID)
}

func TestJoinKeyExceptionUnsupportedURLRemovesInheritedKey(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", strings.Replace(linkSeriesRaw, `"start":`, `"hangoutLink":"https://meet.google.com/old-code-aaa","start":`, 1))
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","hangoutLink":"https://video.example.org/room","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "unsupported-join", "circleback_json", `{"meeting":{"meetingUrl":"https://meet.google.com/old-code-aaa","date":"2027-03-14T12:00:00Z"}}`)
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}
