package store_test

import (
	"fmt"
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

func newColdTimelineActivityFixture(t *testing.T) *timelineActivityFixture {
	t.Helper()
	f := newTimelineActivityFixture(t)
	source, err := f.Store.GetOrCreateSource("beeper", "chat-account")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	room, err := f.Store.EnsureConversation(source.ID, "default-thread", "Default room")
	require.NoError(t, err)
	f.Source, f.ConvID = source, room
	return f
}

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

func TestColdAdapterFiltersMixedNoise(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newColdTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "mixed-noise")
	at := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	human := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	noise := addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(time.Hour), false)
	sender := f.EnsureParticipant("no-reply@example.com", "Automated sender", "example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), sender, noise)
	requirements.NoError(err)
	requirements.NoError(f.Store.ReplaceMessageRecipients(noise, "from", []int64{sender}, nil))
	q := timelineBurstQuery()
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal([]int64{human}, page.Bursts[0].LatestMessageIDs)
	assertions.Equal(human, page.Bursts[0].AnchorMessageID)
	assertions.Equal(at, page.Bursts[0].FirstAt)
	assertions.Equal(at, page.Bursts[0].LastAt)
	assertions.Equal(1, page.Bursts[0].Inbound)
	assertions.Equal(1, page.NoiseOmitted)
	count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, count)

	q.Filters.IncludeNoise = true
	page, err = f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal([]int64{noise, human}, page.Bursts[0].LatestMessageIDs)
	assertions.Zero(page.NoiseOmitted)
	count, err = f.Store.CountColdActivityMessagesContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, count)

	// Leave only the automated message to check whole-room exclusion.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), human)
	requirements.NoError(err)
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

// A saved person's human address keeps an automated sender alias visible.
func TestColdAdapterKeepsSavedAutomatedAliasesVisible(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newColdTimelineActivityFixture(t)
	coldTimelineRoom(t, f, f.Source.ID, "default-thread")
	alias := f.EnsureParticipant("no-reply@example.com", "Automated alias", "example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	id := addColdTimelineMessage(t, f, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false)
	sender, recipient := alias, f.peer
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
	byParticipant := map[int64]store.ActivityMessageCounterpart{}
	for _, cp := range cold.Bursts[0].Counterparts {
		byParticipant[cp.ParticipantID] = cp
	}
	requirements.Contains(byParticipant, alias)
	assertions.Empty(byParticipant[alias].Excluded)
	assertions.Equal(id, cold.Bursts[0].AnchorMessageID)
	assertions.Equal(1, cold.Bursts[0].Inbound)
}

// Bypassing live eligibility, scope, or exact archive-zone dating leaks a row here.
func TestColdAdapterAppliesEligibilityAndScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newColdTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "visible")
	hidden, err := f.Store.GetOrCreateSource("beeper", "hidden-account")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(hidden.ID, "owner@example.com", "manual"))
	hiddenRoom := coldTimelineRoom(t, f, hidden.ID, "hidden")
	at := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // Oct 31, 23:30 in Vancouver (UTC-7).
	visible := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	addColdTimelineMessage(t, f, hiddenRoom, hidden.ID, at, false)
	deleted := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), deleted)
	requirements.NoError(err)
	fullRoom := coldTimelineRoom(t, f, f.Source.ID, "full-room")
	setTimelineConversationTier(t, f, f.Source.ID, "full-room", conversationpolicy.Full)
	addColdTimelineMessage(t, f, fullRoom, f.Source.ID, at, false)
	emailRoom, err := f.Store.EnsureConversationWithType(f.Source.ID, "email-thread", "email_thread", "Email thread")
	requirements.NoError(err)
	setTimelineConversationTier(t, f, f.Source.ID, "email-thread", conversationpolicy.Cold)
	f.add(t, emailRoom, f.Source.ID, at, false, "")
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
	f := newColdTimelineActivityFixture(t)
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
					page, err := f.Store.ListColdActivityBurstsContext(t.Context(), tc.query)
					require.NoError(t, err)
					require.Len(t, page.Bursts, 1)
					assert.Equal(t, []int64{id}, page.Bursts[0].LatestMessageIDs)
					assert.Equal(t, at, page.Bursts[0].FirstAt)
				})
			}
		})
	}
}

// Counts describe all admitted messages, independently of burst pagination.
func TestColdCountIgnoresBurstPagination(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newColdTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "first")
	second := coldTimelineRoom(t, f, f.Source.ID, "second")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	addColdTimelineMessage(t, f, room, f.Source.ID, at.Add(time.Minute), true)
	addColdTimelineMessage(t, f, second, f.Source.ID, at.Add(2*time.Minute), false)
	q := timelineBurstQuery()
	q.Limit = 1
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
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
func TestColdAdapterMatchesWarmAfterRetraction(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newColdTimelineActivityFixture(t)
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
		warm.Bursts[i].FirstAt = warm.Bursts[i].FirstAt.UTC()
		warm.Bursts[i].LastAt = warm.Bursts[i].LastAt.UTC()
	}
	assertions.Equal(warm, cold)
}

// Candidate loading and counterpart hydration must cover more than one 512-ID chunk.
func TestColdAdapterAggregatesCompleteLargeBurst(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newColdTimelineActivityFixture(t)
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
}

// Cold reconstruction applies the stored audience limit without projection rows.
func TestColdAdapterUsesStoredAudiencePolicy(t *testing.T) {
	for _, tc := range []struct {
		name                string
		outgoing, chat      bool
		want, limit, roster int
	}{
		{name: "sender-plus-recipient"},
		{name: "outgoing-limit-1", outgoing: true},
		{name: "chat-roster-limit-30", chat: true, limit: 30, roster: 30, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newColdTimelineActivityFixture(t)
			coldTimelineRoom(t, f, f.Source.ID, "default-thread")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			if tc.chat {
				for _, participant := range []int64{f.owner, f.peer, other} {
					requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, participant, "member"))
				}
			}
			for i := 3; i < tc.roster; i++ {
				participant := f.EnsureParticipant(fmt.Sprintf("member%d@example.com", i), "Member", "example.com")
				requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, participant, "member"))
			}
			limit := tc.limit
			if limit == 0 {
				limit = 1
			}

			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := addColdTimelineMessage(t, f, f.ConvID, f.Source.ID, at, tc.outgoing)
			switch {
			case tc.outgoing:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, other}, []string{"Alice", "Bob"}))
			case tc.chat:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
			case !tc.chat:
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, other}, []string{"Owner", "Bob"}))
			}
			p, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: limit})
			requirements.NoError(err)
			_, err = p.RunOnce(t.Context())
			requirements.NoError(err)
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			cold, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(cold.Bursts, 1)
			assertions.Equal(tc.want, cold.Bursts[0].Inbound+cold.Bursts[0].Outbound, "cold counted contact")
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

// Message type determines contact totals and each peer's contact annotation.
func TestColdAdapterAppliesMessageTypeContactEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		want       int
	}{
		{"SMS", "sms", 1},
		{"missed Synctech call", "synctech_sms_call", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newColdTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "contact-parity")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE id=?`), room)
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
	f := newColdTimelineActivityFixture(t)
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
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Zero(page.Bursts[0].Outbound)
	var peer store.ActivityMessageCounterpart
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			peer = cp
		}
	}
	requirements.Equal(f.peer, peer.ParticipantID)
	assertions.False(peer.Counted)
}

func TestColdAdapterRetainsCompleteDayForTimestampWindow(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	zone := time.FixedZone("stored", 8*60*60)
	storedTimes := []time.Time{start.Add(-8 * time.Hour).In(zone), start.Add(-time.Second).In(zone), start.In(zone), start.Add(30 * time.Minute).In(zone), end.In(zone), start.Add(8 * time.Hour).In(zone)}
	intersectionTimes := []time.Time{start.Add(-24 * time.Hour), start.Add(-time.Hour), start, end, start.Add(24 * time.Hour)}
	for _, tc := range []struct {
		name               string
		times              []time.Time
		from, to           string
		after, before      *time.Time
		wantBursts, want   int
		checkLatestMessage bool
	}{
		{name: "timestamp bounds", times: storedTimes, after: &start, before: &end, wantBursts: 1, want: 6},
		{name: "after", times: storedTimes, after: &start, wantBursts: 1, want: 6},
		{name: "before", times: storedTimes, before: &end, wantBursts: 1, want: 6},
		{name: "outside", times: storedTimes, after: new(start.Add(9 * time.Hour))},
		{name: "date bounds", times: intersectionTimes, from: "2026-10-22", to: "2026-10-22", after: new(start.Add(-48 * time.Hour)), before: new(start.Add(48 * time.Hour)), wantBursts: 1, want: 3, checkLatestMessage: true},
		{name: "disjoint", times: intersectionTimes, from: "2026-10-21", to: "2026-10-21", after: &start, before: &end, checkLatestMessage: true},
		{name: "unbounded", times: intersectionTimes, wantBursts: 3, want: 5, checkLatestMessage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newColdTimelineActivityFixture(t)
			room := coldTimelineRoom(t, f, f.Source.ID, "stored-offset-window")
			for _, at := range tc.times {
				addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
			}
			q := store.BurstQuery{FromDate: tc.from, ToDate: tc.to, After: tc.after, Before: tc.before, Limit: 10}
			page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Len(page.Bursts, tc.wantBursts)
			if tc.checkLatestMessage {
				messages := 0
				for _, burst := range page.Bursts {
					messages += len(burst.LatestMessageIDs)
				}
				assertions.Equal(tc.want, messages)
			}
			count, err := f.Store.CountColdActivityMessagesContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(tc.want, count)
		})
	}
}

func TestColdAdapterKiritimatiStoredOffset(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newColdTimelineActivityFixture(t)
	room := coldTimelineRoom(t, f, f.Source.ID, "kiritimati-offset")
	at, err := time.Parse("2006-01-02 15:04:05Z07:00", "2026-10-20 22:30:00-12:00")
	requirements.NoError(err)
	id := addColdTimelineMessage(t, f, room, f.Source.ID, at, false)
	projectColdTimeline(t, f, "Pacific/Kiritimati")
	q := timelineBurstQuery()
	q.FromDate, q.ToDate = "2026-10-22", "2026-10-22"
	page, err := f.Store.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(page.Bursts, 1)
	assertions.Equal("2026-10-22", page.Bursts[0].LocalDate)
	assertions.Equal([]int64{id}, page.Bursts[0].LatestMessageIDs)
}
