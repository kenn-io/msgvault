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
	"go.kenn.io/msgvault/internal/persontiers"
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

func addColdTimelineMessage(t *testing.T, f *timelineActivityFixture, room, source int64, at time.Time, outgoing bool) int64 {
	t.Helper()
	id := f.add(t, room, source, at, outgoing, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='chat' WHERE id=?`), id)
	require.NoError(t, err)
	return id
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

func TestColdAdapterExcludesEmailThreads(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineActivityFixture(t)
	setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false, "")
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	assertions.Empty(page.Bursts)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	assertions.Zero(count)
}

func TestColdAdapterIntersectsDateAndTimestampWindows(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "date-intersection")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-24 * time.Hour, -time.Hour, 0, time.Hour, 24 * time.Hour} {
		addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(offset), false)
	}
	for _, tc := range []struct {
		name          string
		from, to      string
		after, before *time.Time
		want          int
	}{
		{"date bounds", "2026-10-22", "2026-10-22", new(at.Add(-48 * time.Hour)), new(at.Add(48 * time.Hour)), 3},
		{"timestamp bounds", "2026-10-21", "2026-10-23", &at, new(at.Add(time.Hour)), 1},
		{"disjoint", "2026-10-21", "2026-10-21", &at, new(at.Add(time.Hour)), 0},
		{"unbounded", "", "", nil, nil, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			q := store.BurstQuery{FromDate: tc.from, ToDate: tc.to, After: tc.after, Before: tc.before, Limit: 10}
			page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			messages := 0
			for _, burst := range page.Bursts {
				messages += len(burst.LatestMessageIDs)
			}
			assertions.Equal(tc.want, messages)
			count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(tc.want, count)
		})
	}
}

// A saved person's human address keeps automated aliases visible in either role.
func TestColdAdapterKeepsSavedAutomatedAliasesVisible(t *testing.T) {
	for _, automatedSender := range []bool{false, true} {
		t.Run(fmt.Sprintf("automated-sender=%t", automatedSender), func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			f := newTimelineActivityFixture(t)
			coldTimelineRoom(t, f, f.Source.ID, "default-thread")
			alias := f.EnsureParticipant("no-reply@example.com", "Automated alias", "example.com")
			_, err := f.Store.LinkParticipants(f.peer, alias)
			requirements.NoError(err)
			person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
			requirements.NoError(err)
			id := addColdTimelineMessage(t, f, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false)
			sender, recipient := f.peer, alias
			if automatedSender {
				sender, recipient = alias, f.peer
			}
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), sender, id)
			requirements.NoError(err)
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, recipient}, nil))
			q := timelineBurstQuery()
			q.Subject.PersonID = person.ID
			q.Filters.Exclusions = persontiers.DefaultConfig()
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(cold.Bursts, 1)
			count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(1, count)
			assertions.Equal(id, cold.Bursts[0].AnchorMessageID)
			assertions.Equal(1, cold.Bursts[0].Inbound)
		})
	}
}

// A room above the default audience limit retains its roster without direct contact.
func TestColdAdapterShowsDefaultThresholdBroadcast(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	src, err := f.Store.GetOrCreateSource("beeper", "quiet-account")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	room := coldTimelineRoom(t, f, src.ID, "quiet-room")
	for i := range 28 {
		participant := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		requirements.NoError(f.Store.EnsureConversationParticipant(room, participant, "member"))
	}
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := addColdTimelineMessage(t, f, room, src.ID, at, false)
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	for _, limit := range []int{0, 30} {
		q.Filters.MaxDirect = limit
		cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(cold.Bursts, 1)
		b := cold.Bursts[0]
		assertions.True(b.Cold)
		assertions.Equal(store.ChannelChat, b.Channel)
		assertions.Equal(id, b.AnchorMessageID)
		assertions.Len(b.Counterparts, 30)
		if limit == 0 {
			assertions.Zero(b.Inbound, "large inbound room contributes no direct contact")
		} else {
			assertions.Equal(1, b.Inbound, "explicit limit admits the room as direct contact")
		}
	}
}

// Bypassing live eligibility, scope, or exact archive-zone dating leaks a row here.
func TestColdAdapterAppliesEligibilityAndScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "visible")
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(hidden.ID, "owner@example.com", "manual"))
	hiddenRoom := coldTimelineRoom(t, f, hidden.ID, "hidden")
	at := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // Oct 31, 23:30 in Vancouver (UTC-7).
	visible := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	addColdTimelineMessage(t, f, hiddenRoom, hidden.ID, at, false)
	deleted := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), deleted)
	requirements.NoError(err)
	addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(24*time.Hour), false)
	fullRoom := coldTimelineRoom(t, f, f.Source.ID, "full-room")
	setTimelineConversationTier(t, f, f.Source.ID, "full-room", conversationpolicy.Full)
	addColdTimelineMessage(t, f, fullRoom, f.Source.ID, at, false)
	projectColdTimeline(t, f, "America/Vancouver")
	q := store.BurstQuery{FromDate: "2026-10-31", ToDate: "2026-10-31", Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	page, err := f.Store.ListColdActivityBurstsContext(ctx, q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal([]int64{visible}, page.Bursts[0].LatestMessageIDs)
	assertions.Equal("2026-10-31", page.Bursts[0].LocalDate)
	empty, err := f.Store.ListColdActivityBurstsContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), q)
	requirements.NoError(err)
	assertions.Empty(empty.Bursts)
	q.Subject.ParticipantIDs = []int64{f.peer}
	subject, err := f.Store.ListColdActivityBurstsContext(ctx, q)
	requirements.NoError(err)
	assertions.Equal(page, subject)
	unused := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	q.Subject = store.ActivitySubject{ParticipantIDs: []int64{unused}}
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	requirements.NoError(err)
	assertions.Empty(empty.Bursts)
	count, err := f.Store.CountColdActivityMessagesContext(ctx, q)
	requirements.NoError(err)
	assertions.Zero(count)
	person, _, err := f.Store.CreatePersonFromParticipant(unused)
	requirements.NoError(err)
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	requirements.NoError(err)
	assertions.Empty(empty.Bursts)
	q.Subject = store.ActivitySubject{}
	q.ConversationID = coldTimelineRoom(t, f, f.Source.ID, "empty-visible")
	empty, err = f.Store.ListColdActivityBurstsContext(ctx, q)
	requirements.NoError(err)
	assertions.Empty(empty.Bursts)
}

func TestColdAdapterFallsBackFromMissingSentAt(t *testing.T) {
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "zero-sent-at")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	for _, sentAt := range []struct {
		name  string
		value any
	}{
		{"zero", time.Time{}},
		{"NULL", nil},
	} {
		t.Run(sentAt.name, func(t *testing.T) {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sent_at=?,received_at=? WHERE id=?`), sentAt.value, at, id)
			require.NoError(t, err)
			for _, tc := range []struct {
				name  string
				query store.BurstQuery
			}{
				{"timestamp", store.BurstQuery{After: &at, Before: new(at.Add(time.Hour)), Limit: 10}},
				{"civil-date", store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Run("list", func(t *testing.T) {
						page, err := f.Store.ListColdActivityBurstsContext(t.Context(), tc.query)
						require.NoError(t, err)
						require.Len(t, page.Bursts, 1)
						assert.Equal(t, []int64{id}, page.Bursts[0].LatestMessageIDs)
						assert.Equal(t, at, page.Bursts[0].FirstAt)
					})
					t.Run("count", func(t *testing.T) {
						count, err := f.Store.CountColdActivityMessagesContext(t.Context(), tc.query)
						require.NoError(t, err)
						assert.Equal(t, 1, count)
					})
				})
			}
		})
	}
}

// Counts describe all admitted messages, independently of burst pagination.
func TestColdCountIgnoresBurstPagination(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "first")
	second := coldTimelineRoom(t, f, f.Source.ID, "second")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	first := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	latest := addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(time.Minute), true)
	addColdTimelineMessage(t, f, second, f.Source.ID, at.Add(2*time.Minute), false)
	q := timelineBurstQuery()
	q.Limit = 1
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal([]int64{latest, first}, page.Bursts[0].LatestMessageIDs)
	assertions.Equal(1, page.Bursts[0].Inbound)
	assertions.Equal(1, page.Bursts[0].Outbound)
	requirements.NotNil(page.NextKey)
	q.AfterKey = page.NextKey
	next, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(next.Bursts, 1)
	assertions.Equal(second, next.Bursts[0].ConversationID)
	assertions.Nil(next.NextKey)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(3, count)
	q.AfterKey = nil
	q.Descending = true
	reverse, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(reverse.Bursts, 1)
	assertions.Equal(second, reverse.Bursts[0].ConversationID)
}

// The same archive must produce the same logical groups after full-room retraction.
func TestColdAdapterMatchesSpineNoiseAndDateRules(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "room")
	at := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	messageIDs := []int64{addColdTimelineMessage(t, f, room, f.Source.ID, at, false)}
	for i := range 6 {
		messageIDs = append(messageIDs, addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(time.Duration(i+1)*time.Minute), i%2 == 0))
	}
	noiseRoom := coldTimelineRoom(t, f, f.Source.ID, "noise")
	noise := addColdTimelineMessage(t, f, noiseRoom, f.Source.ID, at, false)
	messageIDs = append(messageIDs, noise)
	sender := f.EnsureParticipant("no-reply@example.com", "Automated sender", "example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), sender, noise)
	requirements.NoError(err)
	requirements.NoError(f.Store.ReplaceMessageRecipients(noise, "from", []int64{sender}, nil))
	// Both instants stay on Nov 1 in UTC and Vancouver.
	messageIDs = append(messageIDs, addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(time.Hour), false))
	for _, item := range []struct {
		source int64
		key    string
	}{{f.Source.ID, "room"}, {f.Source.ID, "noise"}} {
		setTimelineConversationTier(t, f, item.source, item.key, conversationpolicy.Full)
	}
	projectColdTimeline(t, f, "America/Vancouver")
	q := store.BurstQuery{FromDate: "2026-11-01", ToDate: "2026-11-01", Limit: 5000}
	warm, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(warm.Bursts, 1)
	assertions.Equal(5, warm.Bursts[0].Inbound)
	assertions.Equal(3, warm.Bursts[0].Outbound)
	assertions.Len(warm.Bursts[0].LatestMessageIDs, 5)
	for _, item := range []struct {
		source int64
		key    string
	}{{f.Source.ID, "room"}, {f.Source.ID, "noise"}} {
		setTimelineConversationTier(t, f, item.source, item.key, conversationpolicy.Cold)
	}
	for _, id := range messageIDs {
		retractTimelineActivity(t, f, id)
	}
	for _, table := range []string{"activity_event_persons", "activity_events"} {
		var projected int
		requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&projected))
		requirements.Zero(projected, "%s must be empty before cold parity comparison", table)
	}
	cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	for i := range warm.Bursts {
		warm.Bursts[i].Cold = true
		warm.Bursts[i].FirstAt = warm.Bursts[i].FirstAt.UTC()
		warm.Bursts[i].LastAt = warm.Bursts[i].LastAt.UTC()
	}
	assertions.Equal(warm, cold)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(8, count)
	q.Filters.IncludeNoise = true
	count, err = f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(9, count)
}

// Candidate loading and counterpart hydration must cover more than one 512-ID chunk.
func TestColdAdapterAggregatesCompleteLargeBurst(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "large-history")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	var latest int64
	for i := range 1100 {
		sender, recipient := f.peer, f.owner
		if i%2 == 1 {
			sender, recipient = f.owner, f.peer
		}
		err = tx.QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent,source_is_from_me) VALUES(?,?,?,'chat',?,?,TRUE,?) RETURNING id`), f.Source.ID, room, fmt.Sprintf("cold-history-%d", i), at.Add(time.Duration(i)*time.Second), sender, i%2 == 1).Scan(&latest)
		requirements.NoError(err)
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`), latest, recipient)
		requirements.NoError(err)
	}
	requirements.NoError(tx.Commit())
	nextRoom := coldTimelineRoom(t, f, f.Source.ID, "next-history")
	addColdTimelineMessage(t, f, nextRoom, f.Source.ID, at.Add(time.Hour), false)
	q := timelineBurstQuery()
	q.Limit = 1
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal(550, page.Bursts[0].Inbound)
	assertions.Equal(550, page.Bursts[0].Outbound)
	assertions.Equal(latest, page.Bursts[0].AnchorMessageID)
	assertions.Len(page.Bursts[0].LatestMessageIDs, 5)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1101, count)
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
			coldTimelineRoom(t, f, f.Source.ID, "default-thread")
			setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Full)
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
				for _, participant := range []int64{f.owner, f.peer, other} {
					requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, participant, "member"))
				}
			}
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := addColdTimelineMessage(t, f, f.ConvID, f.Source.ID, at, tc.outgoing)
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
		})
	}
}

// Message type determines contact totals and each peer's contact annotation.
func TestColdAdapterAppliesMessageTypeContactEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, kind, conversation string
		want                     int
	}{
		{"SMS", "sms", "direct_chat", 1},
		{"missed Synctech call", "synctech_sms_call", "direct_chat", 0},
		{"missed Google Voice call", "google_voice_call", "direct_chat", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "contact-parity")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type=? WHERE id=?`), tc.conversation, room)
			requirements.NoError(err)
			id := addColdTimelineMessage(t, f, room, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type=?,subject=? WHERE id=?`), tc.kind, tc.name, id)
			requirements.NoError(err)
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(cold.Bursts, 1)
			assertions.Equal(tc.want, cold.Bursts[0].Inbound+cold.Bursts[0].Outbound)
			foundPeer := false
			for _, cp := range cold.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					foundPeer = true
					assertions.Equal(tc.want == 1, cp.Counted)
				}
			}
			assertions.True(foundPeer, "peer must remain visible")
		})
	}
}

func TestColdAdapterIgnoresStaleProjection(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "growing-room")
	setTimelineConversationTier(t, f, f.Source.ID, "growing-room", conversationpolicy.Full)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	id := addColdTimelineMessage(t, f, room, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), true)
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
	projectColdTimeline(t, f, "UTC")
	var evidence string
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT evidence FROM activity_event_persons WHERE message_id=? AND person_id=?`), id, person.ID).Scan(&evidence)
	requirements.NoError(err)
	requirements.Equal("direct", evidence)
	for i := range 25 {
		participant := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
		requirements.NoError(f.Store.EnsureConversationParticipant(room, participant, "member"))
	}
	setTimelineConversationTier(t, f, f.Source.ID, "growing-room", conversationpolicy.Cold)
	q := timelineBurstQuery()
	q.Subject.PersonID = person.ID
	q.Filters.IncludeNoise = true
	for _, retracted := range []bool{false, true} {
		if retracted {
			retractTimelineActivity(t, f, id)
		}
		page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(page.Bursts, 1)
		assertions.Zero(page.Bursts[0].Outbound, "retracted=%t", retracted)
		var peer store.ActivityMessageCounterpart
		for _, cp := range page.Bursts[0].Counterparts {
			if cp.ParticipantID == f.peer {
				peer = cp
			}
		}
		requirements.Equal(f.peer, peer.ParticipantID)
		assertions.False(peer.Counted, "retracted=%t", retracted)
	}
}

func TestColdAdapterHonorsSenderExclusionOptOut(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "sender-exclusion")
	id := addColdTimelineMessage(t, f, room, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false)
	sender := f.EnsureParticipant("no-reply@example.com", "Automated sender", "example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), sender, id)
	requirements.NoError(err)
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	for _, exclude := range []bool{false, true} {
		q := timelineBurstQuery()
		q.Filters.Exclusions = persontiers.DefaultConfig()
		if !exclude {
			q.Filters.Exclusions.ExcludeAddressPatterns = nil
		}
		page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
		requirements.NoError(err)
		if exclude {
			assertions.Empty(page.Bursts)
			continue
		}
		requirements.Len(page.Bursts, 1)
		assertions.Equal(1, page.Bursts[0].Inbound)
		assertions.Zero(page.NoiseOmitted)
	}
}

func TestColdAdapterFiltersOffsetStoredTimestamps(t *testing.T) {
	for _, offset := range []int{0, 8 * 60 * 60, -7 * 60 * 60, 9 * 60 * 60} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "stored-offset-window")
			start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			zone := time.FixedZone("stored", offset)
			early := addColdTimelineMessage(t, f, room, f.Source.ID, start.Add(-8*time.Hour).In(zone), false)
			justBefore := addColdTimelineMessage(t, f, room, f.Source.ID, start.Add(-time.Second).In(zone), false)
			atStart := addColdTimelineMessage(t, f, room, f.Source.ID, start.In(zone), false)
			inside := addColdTimelineMessage(t, f, room, f.Source.ID, start.Add(30*time.Minute).In(zone), false)
			atEnd := addColdTimelineMessage(t, f, room, f.Source.ID, end.In(zone), false)
			late := addColdTimelineMessage(t, f, room, f.Source.ID, start.Add(8*time.Hour).In(zone), false)
			for _, tc := range []struct {
				name          string
				after, before *time.Time
				want          []int64
			}{
				{"both bounds", &start, &end, []int64{inside, atStart}},
				{"after only", &start, nil, []int64{late, atEnd, inside, atStart}},
				{"before only", nil, &end, []int64{inside, atStart, justBefore, early}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					for _, boundOffset := range []int{0, -7 * 60 * 60, 9 * 60 * 60} {
						boundZone := time.FixedZone("bounds", boundOffset)
						q := timelineBurstQuery()
						q.After, q.Before = tc.after, tc.before
						if tc.after != nil {
							q.After = new(tc.after.In(boundZone))
						}
						if tc.before != nil {
							q.Before = new(tc.before.In(boundZone))
						}
						page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
						requirements.NoError(err)
						count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
						requirements.NoError(err)
						assertions.Equal(len(tc.want), count)
						requirements.Len(page.Bursts, 1)
						assertions.Equal(tc.want, page.Bursts[0].LatestMessageIDs)
						assertions.Equal(len(tc.want), page.Bursts[0].Inbound)
					}
				})
			}
		})
	}
}
