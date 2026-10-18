package store_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var activityNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type activityFixture struct {
	s                         *store.Store
	source, room, self, other int64
}

func newActivityFixture(t *testing.T) activityFixture {
	t.Helper()
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	source, err := s.GetOrCreateSource("slack", "team:self")
	require.NoError(t, err)
	room, err := s.EnsureConversation(source.ID, "room", "Synthetic room")
	require.NoError(t, err)
	self, err := s.EnsureParticipant("self@example.test", "Self", "example.test")
	require.NoError(t, err)
	other, err := s.EnsureParticipant("other@example.test", "Selfish", "example.test")
	require.NoError(t, err)
	require.NoError(t, s.AddAccountIdentity(source.ID, "self@example.test", "manual"))
	return activityFixture{s, source.ID, room, self, other}
}
func (f activityFixture) message(t *testing.T, key string, at time.Time, own bool) int64 {
	t.Helper()
	id, err := f.s.UpsertMessage(&store.Message{SourceID: f.source, ConversationID: f.room, SourceMessageID: key, MessageType: "slack", SentAt: sql.NullTime{Time: at, Valid: true}, IsFromMe: own})
	require.NoError(t, err)
	return id
}
func (f activityFixture) activity(t *testing.T, at time.Time) time.Time {
	t.Helper()
	got, err := f.s.ConversationLastActivityContext(t.Context(), f.room, at)
	require.NoError(t, err)
	return got
}
func (f activityFixture) refresh(t *testing.T) {
	t.Helper()
	_, err := f.s.RefreshConversationPolicy(t.Context(), []int64{f.room}, activityNow)
	require.NoError(t, err)
}
func (f activityFixture) tier(t *testing.T) conversationpolicy.Tier {
	t.Helper()
	rows, err := f.s.ListConversationPolicies(t.Context())
	require.NoError(t, err)
	for _, row := range rows {
		if row.ConversationID == f.room {
			return row.EffectiveTier
		}
	}
	require.FailNow(t, "missing conversation")
	return ""
}

// Removing a valid signal branch must lose only its matching room's activity.
func TestConversationPolicyActivitySignals(t *testing.T) {
	cutoff := activityNow.Add(-180 * 24 * time.Hour)
	for _, tc := range []struct {
		name   string
		signal string
		want   bool
	}{
		{"own", "own", true}, {"confirmed sender", "sender", true}, {"explicit mention", "mention", true}, {"from", "from", true},
		{"generic recipient", "to", false}, {"display substring", "name", false}, {"everyone", "everyone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newActivityFixture(t)
			id := f.message(t, "signal", cutoff, tc.signal == "own")
			switch tc.signal {
			case "sender":
				_, err := f.s.DB().Exec(f.s.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), f.self, id)
				requirements.NoError(err)
			case "mention", "from", "to":
				requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, tc.signal, []int64{f.self}, []string{"Self"}))
			case "name":
				requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{f.other}, []string{"Selfish"}))
				requirements.NoError(f.s.UpsertMessageBody(id, sql.NullString{String: "Selfishly mentioning another participant", Valid: true}, sql.NullString{}))
			case "everyone":
				requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{f.other}, []string{"everyone"}))
				requirements.NoError(f.s.UpsertMessageBody(id, sql.NullString{String: "This message contains @everyone but does not mention the owner", Valid: true}, sql.NullString{}))
			}
			_, err := f.s.RefreshConversationPolicy(t.Context(), []int64{f.room}, activityNow)
			requirements.NoError(err)
			if tc.want {
				assertions.Equal(cutoff, f.activity(t, activityNow))
				assertions.Equal(conversationpolicy.Full, f.tier(t))
				_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow.Add(time.Nanosecond))
				requirements.NoError(err)
				assertions.Equal(conversationpolicy.Cold, f.tier(t))
			} else {
				assertions.True(f.activity(t, activityNow).IsZero())
				assertions.Equal(conversationpolicy.Cold, f.tier(t))
			}
		})
	}
}
func TestConversationPolicyActivityRecentReplyToOldOwnMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	parent := f.message(t, "parent", activityNow.AddDate(-5, 0, 0), true)
	recent := activityNow.Add(-time.Hour)
	child := f.message(t, "reply", recent, false)
	requirements.NoError(f.s.SetMessageReplyContext(t.Context(), child, parent))
	f.refresh(t)
	assertions.Equal(recent, f.activity(t, activityNow))
	assertions.Equal(conversationpolicy.Full, f.tier(t))
	grandchild := f.message(t, "transitive", activityNow, false)
	requirements.NoError(f.s.SetMessageReplyContext(t.Context(), grandchild, child))
	assertions.Equal(recent, f.activity(t, activityNow))
	requirements.NoError(f.s.ClearMessageRepliesContext(t.Context(), f.source, []int64{child}))
	f.refresh(t)
	assertions.Equal(activityNow.AddDate(-5, 0, 0), f.activity(t, activityNow))
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
}
func TestConversationPolicyActivityRejectsFutureAndCrossRoom(t *testing.T) {
	f := newActivityFixture(t)
	recent := activityNow.Add(-time.Hour)
	f.message(t, "valid", recent, true)
	f.message(t, "future", activityNow.Add(time.Hour), true)
	assert.Equal(t, recent, f.activity(t, activityNow))
	for _, crossSource := range []bool{false, true} {
		t.Run(strconv.FormatBool(crossSource), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			g := newActivityFixture(t)
			source := g.source
			if crossSource {
				other, err := g.s.GetOrCreateSource("slack", "other:self")
				requirements.NoError(err)
				source = other.ID
			}
			room := g.room
			if !crossSource {
				var err error
				room, err = g.s.EnsureConversation(source, "other", "Other")
				requirements.NoError(err)
			}
			parent, err := g.s.UpsertMessage(&store.Message{SourceID: source, ConversationID: room, SourceMessageID: "parent", MessageType: "slack", IsFromMe: true, SentAt: sql.NullTime{Time: recent, Valid: true}})
			requirements.NoError(err)
			child := g.message(t, "child", activityNow, false)
			requirements.NoError(g.s.SetMessageReplyContext(t.Context(), child, parent))
			assertions.True(g.activity(t, activityNow).IsZero())
			assertions.Equal(conversationpolicy.Cold, g.tier(t))
		})
	}
}
func TestConversationPolicyActivityNewestFirstReplyRepair(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	recent := activityNow.Add(-time.Hour)
	f.message(t, "reply", recent, false)
	requirements.NoError(f.s.SetReplyToContext(t.Context(), f.source, "reply", "parent"))
	f.refresh(t)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	f.message(t, "parent", activityNow.AddDate(-4, 0, 0), true)
	requirements.NoError(f.s.SetReplyToContext(t.Context(), f.source, "reply", "parent"))
	f.refresh(t)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
	assertions.Equal(recent, f.activity(t, activityNow))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requirements.ErrorIs(f.s.ClearMessageRepliesContext(ctx, f.source, []int64{1}), context.Canceled)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
	requirements.NoError(f.s.SetReplyToContext(t.Context(), f.source, "reply", "missing"))
	f.refresh(t)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
}

func TestSetReplyToPreservesCrossConversationThreadReference(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	parentAt := activityNow.Add(-time.Hour)
	parent := f.message(t, "channel-parent", parentAt, true)
	thread, err := f.s.EnsureConversation(f.source, "thread", "Synthetic thread")
	requirements.NoError(err)
	child, err := f.s.UpsertMessage(&store.Message{
		SourceID: f.source, ConversationID: thread, SourceMessageID: "thread-reply",
		MessageType: "slack", SentAt: sql.NullTime{Time: activityNow, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(f.s.SetReplyToContext(t.Context(), f.source, "thread-reply", "channel-parent"))

	var linkedParent sql.NullInt64
	requirements.NoError(f.s.DB().QueryRow(f.s.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child).Scan(&linkedParent))
	assertions.True(linkedParent.Valid)
	assertions.Equal(parent, linkedParent.Int64)
	assertions.Equal(parentAt, f.activity(t, activityNow))
	activity, err := f.s.ConversationLastActivityContext(t.Context(), thread, activityNow)
	requirements.NoError(err)
	assertions.True(activity.IsZero())
}

func TestConversationPolicySelfRevisionReattributesHistory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	_, err := f.s.RemoveAccountIdentity(f.source, "self@example.test")
	requirements.NoError(err)
	id := f.message(t, "mention", activityNow.Add(-time.Hour), false)
	requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{f.self}, nil))
	settings := conversationpolicy.DefaultSettings()
	_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	requirements.NoError(f.s.AddAccountIdentity(f.source, "self@example.test", "manual"))
	_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
	assertions.Equal(activityNow.Add(-time.Hour), f.activity(t, activityNow))
	_, err = f.s.RemoveAccountIdentity(f.source, "self@example.test")
	requirements.NoError(err)
	_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	assertions.True(f.activity(t, activityNow).IsZero())
}
func TestConversationPolicyExpiryDoesNotRecomputeActivity(t *testing.T) {
	requirements := require.New(t)

	f := newActivityFixture(t)
	f.message(t, "own", activityNow, true)
	_, err := f.s.RefreshConversationPolicy(t.Context(), []int64{f.room}, activityNow)
	requirements.NoError(err)
	// Hide the activity signal and bodies: expiry may read participant bindings
	// for people retraction, but must not recompute activity from history.
	_, err = f.s.DB().Exec(`ALTER TABLE messages RENAME COLUMN source_is_from_me TO activity_hidden_own_signal`)
	requirements.NoError(err)
	// PostgreSQL's existing embed_gen update journal resolves message_bodies
	// even without a body change. Keep that unrelated trigger relation intact.
	if !f.s.IsPostgreSQL() {
		_, err = f.s.DB().Exec(`ALTER TABLE message_bodies RENAME TO activity_hidden_bodies`)
		requirements.NoError(err)
	}
	_, err = f.s.ConversationLastActivityContext(t.Context(), f.room, activityNow)
	requirements.Error(err, "the actual history reader must fail with its signal column hidden")
	_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow.Add(180*24*time.Hour+time.Nanosecond))
	requirements.NoError(err)
	assert.Equal(t, conversationpolicy.Cold, f.tier(t))
}

// Future signals cannot hide the maximum past signal, in either insertion
// order, including a room already cold. The oracle is the generated past time.
func FuzzConversationPolicyActivityFutureDoesNotMaskRecent(f *testing.F) {
	f.Add(uint32(1), uint32(1), false, false, int16(0))
	f.Add(uint32(86400), uint32(86400), true, true, int16(0))
	f.Add(uint32(0), uint32(0), true, false, int16(0))
	f.Add(uint32(180*24*3600), uint32(1), false, true, int16(12*60))
	f.Fuzz(func(t *testing.T, pastSeconds, futureSeconds uint32, reverse, cold bool, offsetMinutes int16) {
		fixture := newActivityFixture(t)
		past := activityNow.Add(-time.Duration(uint64(pastSeconds)%(365*24*3600)) * time.Second)
		future := activityNow.Add(time.Duration(1+uint64(futureSeconds)%(365*24*3600)) * time.Second)
		offset := int(offsetMinutes) % (14*60 + 1) * 60
		storedPast := past.In(time.FixedZone("past", offset))
		storedFuture := future.In(time.FixedZone("future", -offset))
		if reverse {
			fixture.message(t, "future", storedFuture, true)
			fixture.message(t, "past", storedPast, true)
		} else {
			fixture.message(t, "past", storedPast, true)
			fixture.message(t, "future", storedFuture, true)
		}
		fixture.message(t, "older", past.Add(-time.Hour), true)
		tier := conversationpolicy.Full
		if cold {
			tier = conversationpolicy.Cold
		}
		require.NoError(t, fixture.s.SetConversationPolicyOverride(t.Context(), fixture.source, "room", tier))
		assert.Equal(t, past, fixture.activity(t, activityNow))
	})
}

func TestConversationPolicyActivityUnchangedConfigureDoesNotScanHistory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	f.message(t, "own", activityNow, true)
	settings := conversationpolicy.DefaultSettings()
	before, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	_, err = f.s.DB().Exec(`ALTER TABLE messages RENAME TO activity_hidden_messages`)
	requirements.NoError(err)
	_, err = f.s.DB().Exec(`ALTER TABLE message_bodies RENAME TO activity_hidden_bodies`)
	requirements.NoError(err)
	after, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(before.Revision, after.Revision)
	assertions.Empty(after.ChangedConversationIDs)
}

func TestConversationPolicyActivityTimestampFallbackAndSourceSelf(t *testing.T) {
	for _, field := range []string{"received", "internal", "unconfirmed source", "all resolved participants", "from snapshot"} {
		t.Run(field, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newActivityFixture(t)
			at := activityNow.Add(-time.Hour)
			msg := &store.Message{SourceID: f.source, ConversationID: f.room, SourceMessageID: "signal", MessageType: "slack", IsFromMe: true}
			switch field {
			case "received":
				msg.ReceivedAt = sql.NullTime{Time: at, Valid: true}
			case "internal":
				msg.InternalDate = sql.NullTime{Time: at, Valid: true}
			default:
				msg.SentAt = sql.NullTime{Time: at, Valid: true}
				msg.IsFromMe = false
			}
			if field == "unconfirmed source" {
				other, err := f.s.GetOrCreateSource("slack", "other:self")
				requirements.NoError(err)
				msg.SourceID = other.ID
				msg.SenderID = sql.NullInt64{Int64: f.self, Valid: true}
			}
			if field == "from snapshot" {
				msg.SenderID = sql.NullInt64{Int64: f.self, Valid: true}
			}
			id, err := f.s.UpsertMessage(msg)
			requirements.NoError(err)
			if field == "all resolved participants" {
				var second int64
				requirements.NoError(f.s.DB().QueryRow(`INSERT INTO participants(display_name) VALUES('Alias') RETURNING id`).Scan(&second))
				requirements.NoError(f.s.SetParticipantIdentifier(second, "email", "SELF@EXAMPLE.TEST"))
				requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{second}, nil))
			}
			if field == "from snapshot" {
				_, err = f.s.DB().Exec(f.s.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'from','peer@example.test')`), id, f.self)
				requirements.NoError(err)
			}
			if field == "unconfirmed source" || field == "from snapshot" {
				assertions.True(f.activity(t, activityNow).IsZero())
			} else {
				assertions.Equal(at, f.activity(t, activityNow))
			}
		})
	}
}

func TestConversationPolicySelfRevisionIgnoresUnrelatedIdentityDrift(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	a, err := f.s.EnsureParticipant("peer-a@example.test", "Peer A", "example.test")
	requirements.NoError(err)
	b, err := f.s.EnsureParticipant("peer-b@example.test", "Peer B", "example.test")
	requirements.NoError(err)
	f.message(t, "own", activityNow, true)
	settings := conversationpolicy.DefaultSettings()
	before, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	_, err = f.s.LinkParticipants(a, b)
	requirements.NoError(err)
	// Any reattribution would fail against the hidden history. Linking two
	// unrelated people must only advance the observed revision marker.
	_, err = f.s.DB().Exec(`ALTER TABLE messages RENAME TO activity_hidden_messages`)
	requirements.NoError(err)
	after, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(before.Revision, after.Revision)
	assertions.Empty(after.ChangedConversationIDs)
	again, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(before.Revision, again.Revision)
}

func TestConversationPolicySelfRevisionResolvesNewParticipantMapping(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	var alias int64
	requirements.NoError(f.s.DB().QueryRow(`INSERT INTO participants(display_name) VALUES('Alias') RETURNING id`).Scan(&alias))
	id := f.message(t, "mention", activityNow.Add(-time.Hour), false)
	requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{alias}, nil))
	settings := conversationpolicy.DefaultSettings()
	_, err := f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, f.tier(t))
	requirements.NoError(f.s.SetParticipantIdentifier(alias, "email", "SELF@EXAMPLE.TEST"))
	_, err = f.s.ConfigureConversationPolicy(t.Context(), settings, activityNow)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, f.tier(t))
}

func TestConversationPolicyActivityBootstrapUsesEachSourceSelf(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	other, err := f.s.GetOrCreateSource("teams", "other@example.test")
	requirements.NoError(err)
	room, err := f.s.EnsureConversation(other.ID, "room", "Quiet other source")
	requirements.NoError(err)
	self, err := f.s.EnsureParticipant("other@example.test", "Other self", "example.test")
	requirements.NoError(err)
	requirements.NoError(f.s.AddAccountIdentity(other.ID, "other@example.test", "manual"))
	id, err := f.s.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: room, SourceMessageID: "mention", MessageType: "teams", SentAt: sql.NullTime{Time: activityNow.Add(-time.Hour), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(f.s.ReplaceMessageRecipientsContext(t.Context(), id, "mention", []int64{self}, nil))
	_, err = f.s.ConfigureConversationPolicy(sourcescope.WithSourceIDs(t.Context(), []int64{f.source}), conversationpolicy.DefaultSettings(), activityNow)
	requirements.NoError(err)
	policies, err := f.s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(policies, 2)
	assertions.Equal(conversationpolicy.Cold, policies[0].EffectiveTier)
	assertions.Equal(conversationpolicy.Full, policies[1].EffectiveTier)
}

func TestConversationPolicyActivityTimezoneOrdering(t *testing.T) {
	f := newActivityFixture(t)
	recent := activityNow.Add(-time.Hour)
	f.message(t, "past offset", recent.In(time.FixedZone("east", 12*3600)), true)
	f.message(t, "future offset", activityNow.Add(time.Hour).In(time.FixedZone("west", -12*3600)), true)
	assert.Equal(t, recent, f.activity(t, activityNow))
}

func TestConversationPolicyExpiryMissingStateDoesNotScanHistory(t *testing.T) {
	requirements := require.New(t)

	f := newActivityFixture(t)
	f.message(t, "own", activityNow, true)
	f.refresh(t)
	_, err := f.s.DB().Exec(f.s.Rebind(`DELETE FROM conversation_policy WHERE conversation_id=?`), f.room)
	requirements.NoError(err)
	_, err = f.s.DB().Exec(`ALTER TABLE messages RENAME COLUMN source_is_from_me TO activity_hidden_own_signal`)
	requirements.NoError(err)
	_, err = f.s.ConversationLastActivityContext(t.Context(), f.room, activityNow)
	requirements.Error(err, "the actual history reader must fail with its signal column hidden")
	_, err = f.s.ExpireConversationPolicy(t.Context(), activityNow)
	requirements.NoError(err)
	assert.Equal(t, conversationpolicy.Cold, f.tier(t))
}
