package store_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func projectColdTimeline(t *testing.T, f *timelineActivityFixture, zone string) {
	t.Helper()
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: zone, BatchSize: 10000})
	require.NoError(t, err)
	_, err = projector.RunBackstop(t.Context())
	require.NoError(t, err)
}

func coldTimelineRoom(t *testing.T, f *timelineActivityFixture, source int64, key string) int64 {
	t.Helper()
	room, err := f.Store.EnsureConversation(source, key, "Quiet room")
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), room)
	require.NoError(t, err)
	require.NoError(t, f.Store.EnsureConversationParticipant(room, f.owner, "member"))
	require.NoError(t, f.Store.EnsureConversationParticipant(room, f.peer, "member"))
	setTimelineConversationTier(t, f, source, key, conversationpolicy.Cold)
	return room
}

func setTimelineConversationTier(t *testing.T, f *timelineActivityFixture, source int64, key string, tier conversationpolicy.Tier) {
	t.Helper()
	// Overrides stay staged on main, so emulate the eligibility producer that
	// will eventually derive effective_tier from this configuration.
	require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), source, key, tier))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversation_policy SET effective_tier=? WHERE conversation_id=(SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?)`), string(tier), source, key)
	require.NoError(t, err)
}

func retractTimelineActivity(t *testing.T, f *timelineActivityFixture, messageID int64) {
	t.Helper()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM activity_event_persons WHERE message_id=?`), messageID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM activity_events WHERE message_id=?`), messageID)
	require.NoError(t, err)
}

// Removing cold-message selection or depending on retracted spine rows loses this room.
func TestColdAdapterReturnsRetractedColdRoomMessages(t *testing.T) {
	f := newTimelineActivityFixture(t)
	src, err := f.Store.GetOrCreateSource("beeper", "quiet-account")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	room := coldTimelineRoom(t, f, src.ID, "quiet-room")
	for i := range 28 {
		participant := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		require.NoError(t, f.Store.EnsureConversationParticipant(room, participant, "member"))
	}
	setTimelineConversationTier(t, f, src.ID, "quiet-room", conversationpolicy.Full)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := f.add(t, room, src.ID, at, false, "")
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
	projectColdTimeline(t, f, "UTC")
	var projected int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM activity_events WHERE message_id=?`), id).Scan(&projected))
	assert.Equal(t, 1, projected)
	setTimelineConversationTier(t, f, src.ID, "quiet-room", conversationpolicy.Cold)
	projectColdTimeline(t, f, "UTC")
	retractTimelineActivity(t, f, id)
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM activity_events WHERE message_id=?`), id).Scan(&projected))
	assert.Zero(t, projected)
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	spine, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, spine.Bursts)
	cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, cold.Bursts, 1)
	b := cold.Bursts[0]
	assert.True(t, b.Cold)
	assert.Equal(t, store.ChannelChat, b.Channel)
	assert.Equal(t, id, b.AnchorMessageID)
	assert.Len(t, b.Counterparts, 30)
	assert.Zero(t, b.Inbound, "large inbound room is visible but is not direct contact")
	q.Filters.IncludeNoise = false
	filtered, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, filtered.Bursts)
	assert.Equal(t, 1, filtered.NoiseOmitted)
}

// Bypassing live eligibility, scope, or exact archive-zone dating leaks a row here.
func TestColdAdapterAppliesEligibilityAndScope(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "visible")
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(hidden.ID, "owner@example.com", "manual"))
	hiddenRoom := coldTimelineRoom(t, f, hidden.ID, "hidden")
	at := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // Oct 31, 23:30 in Vancouver (UTC-7).
	visible := f.add(t, room, f.Source.ID, at, false, "")
	hiddenID := f.add(t, hiddenRoom, hidden.ID, at, false, "")
	for _, change := range []string{"is_sent=FALSE", "deleted_at=CURRENT_TIMESTAMP", "deleted_from_source_at=CURRENT_TIMESTAMP", `message_type='calendar_event',metadata='{"status":"cancelled"}'`, "sent_at=NULL,received_at=NULL,internal_date=NULL"} {
		id := f.add(t, room, f.Source.ID, at, false, "")
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET `+change+` WHERE id=?`), id)
		require.NoError(t, err)
	}
	f.add(t, room, f.Source.ID, at.Add(24*time.Hour), false, "")
	f.add(t, f.ConvID, f.Source.ID, at, false, "") // No cold policy.
	projectColdTimeline(t, f, "America/Vancouver")
	q := store.BurstQuery{FromDate: "2026-10-31", ToDate: "2026-10-31", Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	page, err := f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	require.Len(t, page.Bursts, 1)
	assert.Equal(t, []int64{visible}, page.Bursts[0].LatestMessageIDs)
	assert.Equal(t, "2026-10-31", page.Bursts[0].LocalDate)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), hiddenID)
	require.NoError(t, err)
	unchanged, err := f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, page, unchanged)
	empty, err := f.Store.ListColdActivityBurstsContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), q)
	require.NoError(t, err)
	assert.Empty(t, empty.Bursts)
	q.Subject.ParticipantIDs = []int64{f.peer}
	subject, err := f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, page, subject)
	unused := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{unused}}
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, empty.Bursts)
	count, err := f.Store.CountColdActivityMessagesContext(ctx, q)
	require.NoError(t, err)
	assert.Zero(t, count)
	person, _, err := f.Store.CreatePersonFromParticipant(unused)
	require.NoError(t, err)
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, empty.Bursts)
	q.Subject = store.ActivitySubject{}
	q.ConversationID = hiddenRoom
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, empty.Bursts)
}

// Counts describe all admitted representatives, independently of burst pagination.
func TestColdCountMatchesList(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "first")
	second := coldTimelineRoom(t, f, f.Source.ID, "second")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	first := f.add(t, room, f.Source.ID, at, false, "")
	latest := f.add(t, room, f.Source.ID, at.Add(time.Minute), true, "")
	f.add(t, second, f.Source.ID, at.Add(2*time.Minute), false, "")
	q := timelineBurstQuery()
	q.Limit = 1
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, page.Bursts, 1)
	assert.Equal(t, []int64{latest, first}, page.Bursts[0].LatestMessageIDs)
	assert.Equal(t, 1, page.Bursts[0].Inbound)
	assert.Equal(t, 1, page.Bursts[0].Outbound)
	require.NotNil(t, page.NextKey)
	q.AfterKey = page.NextKey
	next, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, next.Bursts, 1)
	assert.Equal(t, second, next.Bursts[0].ConversationID)
	assert.Nil(t, next.NextKey)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	q.AfterKey = nil
	q.Descending = true
	reverse, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, reverse.Bursts, 1)
	assert.Equal(t, second, reverse.Bursts[0].ConversationID)
	// Timestamp windows are half-open, overriding local dates.
	q.After, q.Before = &at, new(at.Add(time.Minute))
	bounded, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 1, bounded)
	// received_at is the origin when sent_at is absent.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sent_at=NULL,received_at=? WHERE id=?`), at, first)
	require.NoError(t, err)
	bounded, err = f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 1, bounded)
}

// The same archive must produce the same logical groups after full-room retraction.
func TestColdAdapterMatchesSpineCopyNoiseAndDateRules(t *testing.T) {
	f := newTimelineActivityFixture(t)
	other, err := f.Store.GetOrCreateSource("gmail", "copy@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
	room := coldTimelineRoom(t, f, f.Source.ID, "mail")
	copyRoom := coldTimelineRoom(t, f, other.ID, "mail-copy")
	for _, conv := range []int64{room, copyRoom} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='email_thread' WHERE id=?`), conv)
		require.NoError(t, err)
	}
	at := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	rep := f.add(t, room, f.Source.ID, at, false, "<same@example.com>")
	f.add(t, copyRoom, other.ID, at, false, "<same@example.com>")
	for i := range 6 {
		f.add(t, room, f.Source.ID, at.Add(time.Duration(i+1)*time.Minute), i%2 == 0, "")
	}
	noiseRoom := coldTimelineRoom(t, f, f.Source.ID, "noise")
	noise := f.add(t, noiseRoom, f.Source.ID, at, false, "")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), noise)
	require.NoError(t, err)
	// Both instants on the DST fallback day stay in the same local-date group.
	f.add(t, room, f.Source.ID, at.Add(time.Hour), false, "")
	for _, item := range []struct {
		source int64
		key    string
	}{{f.Source.ID, "mail"}, {other.ID, "mail-copy"}, {f.Source.ID, "noise"}} {
		setTimelineConversationTier(t, f, item.source, item.key, conversationpolicy.Full)
	}
	projectColdTimeline(t, f, "America/Vancouver")
	q := store.BurstQuery{FromDate: "2026-11-01", ToDate: "2026-11-01", Limit: 5000}
	warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, warm.Bursts, 1)
	assert.Equal(t, 2, warm.Bursts[0].CopyCounts[rep])
	assert.Equal(t, 5, warm.Bursts[0].Inbound)
	assert.Equal(t, 3, warm.Bursts[0].Outbound)
	assert.Len(t, warm.Bursts[0].LatestMessageIDs, 5)
	for _, item := range []struct {
		source int64
		key    string
	}{{f.Source.ID, "mail"}, {other.ID, "mail-copy"}, {f.Source.ID, "noise"}} {
		setTimelineConversationTier(t, f, item.source, item.key, conversationpolicy.Cold)
	}
	projectColdTimeline(t, f, "America/Vancouver")
	cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	for i := range warm.Bursts {
		warm.Bursts[i].Cold = true
		warm.Bursts[i].FirstAt = warm.Bursts[i].FirstAt.UTC()
		warm.Bursts[i].LastAt = warm.Bursts[i].LastAt.UTC()
	}
	assert.Equal(t, warm, cold)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 8, count)
	q.Filters.IncludeNoise = true
	count, err = f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 9, count)
}

// Treating meeting evidence as email would collapse unrelated all-day events.
func TestColdAdapterDatesDayPrecisionBeforeCopyCollapse(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "all-day")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='email_thread' WHERE id=?`), room)
	require.NoError(t, err)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	first := f.add(t, room, f.Source.ID, at, false, "<event@example.com>")
	second := f.add(t, room, f.Source.ID, at, false, "<event@example.com>")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='calendar_event',metadata='{"all_day":true}' WHERE id IN (?,?)`), first, second)
	require.NoError(t, err)
	projectColdTimeline(t, f, "America/Vancouver")
	q := store.BurstQuery{FromDate: "2026-11-01", ToDate: "2026-11-01", Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}}
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, page.Bursts, 1)
	assert.Equal(t, store.ChannelMeeting, page.Bursts[0].Channel)
	assert.Equal(t, []int64{second, first}, page.Bursts[0].LatestMessageIDs)
	assert.Equal(t, "2026-11-01", page.Bursts[0].LocalDate)
	assert.Empty(t, page.Bursts[0].CopyCounts)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// A burst page limit must never truncate the messages used for a complete group.
func TestColdAdapterAggregatesCompleteLargeBurst(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "large-history")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var latest int64
	for i := range 6000 {
		sender, recipient := f.peer, f.owner
		if i%2 == 1 {
			sender, recipient = f.owner, f.peer
		}
		err = tx.QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent,source_is_from_me) VALUES(?,?,?,'chat',?,?,TRUE,?) RETURNING id`), f.Source.ID, room, fmt.Sprintf("cold-history-%d", i), at.Add(time.Duration(i)*time.Second), sender, i%2 == 1).Scan(&latest)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`), latest, recipient)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	nextRoom := coldTimelineRoom(t, f, f.Source.ID, "next-history")
	f.add(t, nextRoom, f.Source.ID, at.Add(time.Hour), false, "")
	q := timelineBurstQuery()
	q.Limit = 1
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, page.Bursts, 1)
	assert.Equal(t, 3000, page.Bursts[0].Inbound)
	assert.Equal(t, 3000, page.Bursts[0].Outbound)
	assert.Equal(t, latest, page.Bursts[0].AnchorMessageID)
	assert.Len(t, page.Bursts[0].LatestMessageIDs, 5)
	require.NotNil(t, page.NextKey)
	q.AfterKey = page.NextKey
	next, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, next.Bursts, 1)
	assert.Equal(t, nextRoom, next.Bursts[0].ConversationID)
	assert.Nil(t, next.NextKey)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 6001, count)
}

// Saved-person volume uses the classifier's strongest role, as the spine does.
func TestColdAdapterMatchesSavedPersonRoles(t *testing.T) {
	f := newTimelineActivityFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(t, err)
	room := coldTimelineRoom(t, f, f.Source.ID, "multiple-authors")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := f.add(t, room, f.Source.ID, at, true, "")
	// A saved person is both another author and an addressed recipient. The
	// persisted spine credits its strongest sender role, not the weaker To role.
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner, f.peer}, []string{"Owner", "Alice"}))
	setTimelineConversationTier(t, f, f.Source.ID, "multiple-authors", conversationpolicy.Full)
	projectColdTimeline(t, f, "UTC")
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, warm.Bursts, 1)
	assert.Zero(t, warm.Bursts[0].Outbound)
	setTimelineConversationTier(t, f, f.Source.ID, "multiple-authors", conversationpolicy.Cold)
	projectColdTimeline(t, f, "UTC")
	cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, cold.Bursts, 1)
	assert.Zero(t, cold.Bursts[0].Outbound)
	q.Filters.IncludeNoise = false
	filtered, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, filtered.Bursts)
	assert.Equal(t, 1, filtered.NoiseOmitted)
}

// Reapplying strongest projected roles for an explicit limit loses the raw
// addressed evidence that the spine deliberately reclassifies for that limit.
func TestColdAdapterCustomLimitMatchesSpineSavedPersonRoles(t *testing.T) {
	for _, limit := range []int{1, 25} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
			require.NoError(t, err)
			room := coldTimelineRoom(t, f, f.Source.ID, "custom-limit-authors")
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := f.add(t, room, f.Source.ID, at, true, "")
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{f.owner, f.peer}, []string{"Owner", "Alice"}))
			setTimelineConversationTier(t, f, f.Source.ID, "custom-limit-authors", conversationpolicy.Full)
			projectColdTimeline(t, f, "UTC")
			q := timelineBurstQuery()
			q.Filters.MaxDirect = limit
			warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, warm.Bursts, 1)
			assert.Equal(t, 1, warm.Bursts[0].Outbound)
			assert.Zero(t, warm.NoiseOmitted)

			setTimelineConversationTier(t, f, f.Source.ID, "custom-limit-authors", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, cold.Bursts, 1)
			assert.Equal(t, 1, cold.Bursts[0].Outbound)
			warm.Bursts[0].Cold = true
			warm.Bursts[0].FirstAt = warm.Bursts[0].FirstAt.UTC()
			warm.Bursts[0].LastAt = warm.Bursts[0].LastAt.UTC()
			assert.Equal(t, warm, cold)
			count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
		})
	}
}

// Cold reconstruction must keep the same sender/alias/envelope audience and
// configured default limit as warm activity, even after spine rows are gone.
func TestColdAdapterAudienceAndProjectionPolicyParity(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		outgoing, saved, aliases, chat, envelope, explicit bool
		want                                               int
	}{
		{name: "sender-plus-recipient", explicit: true},
		{name: "saved-aliases", aliases: true, explicit: true, want: 1},
		{name: "default-unsaved", outgoing: true},
		{name: "default-saved", outgoing: true, saved: true},
		{name: "chat-envelope", chat: true, envelope: true, explicit: true, want: 1},
		{name: "chat-roster", chat: true, explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			if tc.aliases {
				_, err := f.Store.LinkParticipants(f.peer, other)
				requirements.NoError(err)
				_, _, err = f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
			} else if tc.saved {
				_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
			}
			if tc.chat {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), f.ConvID)
				requirements.NoError(err)
				for _, participant := range []int64{f.owner, f.peer, other} {
					requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, participant, "member"))
				}
			}
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := f.add(t, f.ConvID, f.Source.ID, at, tc.outgoing, "")
			switch {
			case tc.outgoing:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, other}, []string{"Alice", "Bob"}))
			case tc.aliases:
				third := f.EnsureParticipant("alice-alt@example.com", "Alice", "example.com")
				_, err := f.Store.LinkParticipants(f.peer, third)
				requirements.NoError(err)
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, other, third}, []string{"Owner", "Bob", "Alice"}))
			case tc.chat && !tc.envelope:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
			case !tc.chat:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, other}, []string{"Owner", "Bob"}))
			}
			p, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 1})
			requirements.NoError(err)
			_, err = p.RunOnce(t.Context())
			requirements.NoError(err)
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			if tc.explicit {
				q.Filters.MaxDirect = 1
			}
			warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(warm.Bursts, 1)
			assertions.Equal(tc.want, warm.Bursts[0].Inbound+warm.Bursts[0].Outbound, "warm counted contact")
			setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
			retractTimelineActivity(t, f, id)
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(cold.Bursts, 1)
			assertions.Equal(tc.want, cold.Bursts[0].Inbound+cold.Bursts[0].Outbound, "cold counted contact")
			for _, cp := range cold.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assertions.Equal(tc.want == 1, cp.Counted)
				}
			}
			q.Filters.IncludeNoise = false
			filtered, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Len(filtered.Bursts, tc.want)
			assertions.Equal(1-tc.want, filtered.NoiseOmitted)
		})
	}
}

// Retraction must preserve both contact totals and each peer's contact annotation.
func TestColdAdapterPreservesContactEligibilityAcrossRetraction(t *testing.T) {
	for _, tc := range []struct {
		name, kind, conversation string
		outbound                 bool
		want                     int
	}{
		{"legacy email", "", "email_thread", false, 1},
		{"email", "email", "email_thread", false, 1},
		{"SMS", "sms", "direct_chat", false, 1},
		{"meeting", "meeting_transcript", "meeting", true, 1},
		{"missed Synctech call", "synctech_sms_call", "direct_chat", false, 0},
		{"rejected Synctech call", "synctech_sms_call", "direct_chat", false, 0},
		{"missed Google Voice call", "google_voice_call", "direct_chat", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "contact-parity")
			setTimelineConversationTier(t, f, f.Source.ID, "contact-parity", conversationpolicy.Full)
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type=? WHERE id=?`), tc.conversation, room)
			requirements.NoError(err)
			id := f.add(t, room, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), tc.outbound, "")
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type=?,subject=? WHERE id=?`), tc.kind, tc.name, id)
			requirements.NoError(err)
			projectColdTimeline(t, f, "UTC")
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(warm.Bursts, 1)
			assertions.Equal(tc.want, warm.Bursts[0].Inbound+warm.Bursts[0].Outbound)
			setTimelineConversationTier(t, f, f.Source.ID, "contact-parity", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, id)
			var remaining int
			requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM activity_events WHERE message_id=?`), id).Scan(&remaining))
			assertions.Zero(remaining)
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(cold.Bursts, 1)
			assertions.Equal(tc.want, cold.Bursts[0].Inbound+cold.Bursts[0].Outbound)
			for _, burst := range []store.ActivityBurst{warm.Bursts[0], cold.Bursts[0]} {
				foundPeer := false
				for _, cp := range burst.Counterparts {
					if cp.ParticipantID == f.peer {
						foundPeer = true
						assertions.Equal(tc.want == 1, cp.Counted, "cold=%v", burst.Cold)
					}
				}
				assertions.True(foundPeer, "peer must remain visible, cold=%v", burst.Cold)
			}
		})
	}
}

func TestColdAdapterHonorsListExclusionOptOut(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "list-parity")
	setTimelineConversationTier(t, f, f.Source.ID, "list-parity", conversationpolicy.Full)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='email_thread' WHERE id=?`), room)
	requirements.NoError(err)
	id := f.add(t, room, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false, "")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='email',list_id='list.example.com' WHERE id=?`), id)
	requirements.NoError(err)
	projectColdTimeline(t, f, "UTC")
	for _, cold := range []bool{false, true} {
		if cold {
			setTimelineConversationTier(t, f, f.Source.ID, "list-parity", conversationpolicy.Cold)
			projectColdTimeline(t, f, "UTC")
			retractTimelineActivity(t, f, id)
		}
		for _, exclude := range []bool{false, true} {
			q := timelineBurstQuery()
			q.Filters.Exclusions = persontiers.DefaultConfig()
			q.Filters.Exclusions.ExcludeListMessages = exclude
			var page store.BurstPage
			if cold {
				page, err = f.Store.ListColdActivityBurstsContext(t.Context(), q)
			} else {
				page, err = f.Store.ListActivityBurstsContext(t.Context(), q)
			}
			requirements.NoError(err)
			if exclude {
				assertions.Empty(page.Bursts, "cold=%v", cold)
				continue
			}
			requirements.Len(page.Bursts, 1, "cold=%v", cold)
			assertions.Equal(1, page.Bursts[0].Inbound)
			assertions.Zero(page.NoiseOmitted)
		}
	}
}

func TestColdAdapterUsesUTCWindowBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"UTC", 0},
		{"west", -7 * 60 * 60},
		{"east", 9 * 60 * 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "offset-window")
			start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.add(t, room, f.Source.ID, start.Add(-time.Second), false, "")
			atStart := f.add(t, room, f.Source.ID, start, false, "")
			inside := f.add(t, room, f.Source.ID, start.Add(30*time.Minute), false, "")
			f.add(t, room, f.Source.ID, start.Add(time.Hour), false, "")
			zone := time.FixedZone(tc.name, tc.offset)
			after, before := start.In(zone), start.Add(time.Hour).In(zone)
			q := timelineBurstQuery()
			q.After, q.Before = &after, &before
			page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(page.Bursts, 1)
			assertions.Equal([]int64{inside, atStart}, page.Bursts[0].LatestMessageIDs)
			assertions.Equal(2, page.Bursts[0].Inbound)
			count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(2, count)
		})
	}
}
