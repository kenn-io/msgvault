package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineInboxAtomicDedupe(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	other, err := st.GetOrCreateSource("line", "2000000002")
	must.NoError(err)
	botID := "U00000000000000000000000000000001"
	otherBotID := "U00000000000000000000000000000003"
	must.NoError(st.SetLineBotContext(ctx, src.ID, botID))
	must.NoError(st.SetLineBotContext(ctx, other.ID, otherBotID))
	at := time.Now().UTC().Truncate(time.Second)
	event := store.LineWebhookEvent{WebhookEventID: "event-1", EventType: "message", TargetMessageID: "1", EventTimestamp: 10, RawJSON: `{"type":"message"}`, ReceivedAt: at}
	must.NoError(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, []store.LineWebhookEvent{event, event}))
	must.NoError(st.EnqueueLineEventsContext(ctx, other.ID, "2000000002", otherBotID, []store.LineWebhookEvent{event}))
	rows, err := st.DueLineEventsContext(ctx, src.ID, at.Add(time.Second), 100)
	must.NoError(err)
	must.Len(rows, 1)
	checks.Equal("event-1", rows[0].WebhookEventID)
	checks.Equal("1", rows[0].TargetMessageID)
	checks.Equal(src.ID, rows[0].SourceID)
	for _, guard := range []string{"stale channel", "changed bot", "retired source"} {
		t.Run(guard, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("line", "2000000001")
			require.NoError(err)
			require.NoError(st.SetLineBotContext(t.Context(), src.ID, botID))
			channel, bot := "2000000001", botID
			switch guard {
			case "stale channel":
				channel = "2000000000"
			case "changed bot":
				require.NoError(st.SetLineBotContext(t.Context(), src.ID, otherBotID))
			case "retired source":
				into, err := st.GetOrCreateSource("line", "2000000002")
				require.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: into.ID})
				require.NoError(err)
			}
			require.Error(st.EnqueueLineEventsContext(t.Context(), src.ID, channel, bot, []store.LineWebhookEvent{event}))
			status, err := st.LineInboxStatusContext(t.Context(), src.ID)
			require.NoError(err)
			assert.Zero(t, status.Pending)
		})
	}
}

func TestLineInboxRetryDeadlineIgnoresCallerZone(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	botID := "U00000000000000000000000000000001"
	must.NoError(st.SetLineBotContext(ctx, src.ID, botID))
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	must.NoError(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, []store.LineWebhookEvent{{WebhookEventID: "retrying", EventType: "message", EventTimestamp: 10, RawJSON: `{"type":"message"}`, ReceivedAt: base}}))
	rows, err := st.DueLineEventsContext(ctx, src.ID, base, 10)
	must.NoError(err)
	must.Len(rows, 1)
	must.NoError(st.FinishLineEventContext(ctx, rows[0].ID, "retry", "transcoding", base.Add(time.Hour)))

	for _, zone := range []struct {
		name   string
		offset int
	}{{"ahead of UTC", 9 * 3600}, {"behind UTC", -9 * 3600}} {
		loc := time.FixedZone(zone.name, zone.offset)
		due, err := st.DueLineEventsContext(ctx, src.ID, base.Add(30*time.Minute).In(loc), 10)
		must.NoError(err)
		checks.Empty(due, "%s: a retry deadline an hour away is not due after thirty minutes", zone.name)
		due, err = st.DueLineEventsContext(ctx, src.ID, base.Add(2*time.Hour).In(loc), 10)
		must.NoError(err)
		checks.Len(due, 1, "%s: a retry deadline an hour past is due after two hours", zone.name)
	}
}

func TestLineInboxDueAndRetention(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	botID := "U00000000000000000000000000000001"
	must.NoError(st.SetLineBotContext(ctx, src.ID, botID))
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-31 * 24 * time.Hour)
	events := []store.LineWebhookEvent{{WebhookEventID: "original", EventType: "message", EventTimestamp: 20, RawJSON: `{"text":"synthetic"}`, ReceivedAt: old}, {WebhookEventID: "orphan", EventType: "unsend", TargetMessageID: "late", EventTimestamp: 10, RawJSON: `{"unsend":{"messageId":"late"}}`, ReceivedAt: old.Add(-time.Hour)}}
	must.NoError(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, events))
	rows, err := st.DueLineEventsContext(ctx, src.ID, now, 10)
	must.NoError(err)
	must.Len(rows, 2)
	checks.Equal("orphan", rows[0].WebhookEventID)
	must.NoError(st.FinishLineEventContext(ctx, rows[0].ID, "failed", strings.Repeat("界", 200), time.Time{}))
	must.NoError(st.FinishLineEventContext(ctx, rows[1].ID, "retry", "transcoding", now.Add(time.Minute)))
	due, err := st.DueLineEventsContext(ctx, src.ID, now, 10)
	must.NoError(err)
	checks.Empty(due)
	due, err = st.DueLineEventsContext(ctx, src.ID, now.Add(2*time.Minute), 10)
	must.NoError(err)
	must.Len(due, 1)
	checks.Equal(1, due[0].Attempts)
	must.NoError(st.FinishLineEventContext(ctx, rows[1].ID, "processed", "", time.Time{}))
	must.NoError(st.PruneLineEventsContext(ctx, src.ID, now.Add(-30*24*time.Hour)))
	var retained int
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), src.ID, "original").Scan(&retained))
	checks.Zero(retained)
	mutations, err := st.LineMutationsContext(ctx, src.ID, "late")
	must.NoError(err)
	must.Len(mutations, 1)
	checks.Contains(mutations[0].RawJSON, "late")
	checks.Equal(strings.Repeat("界", 166), mutations[0].LastError)
	status, err := st.LineInboxStatusContext(ctx, src.ID)
	must.NoError(err)
	checks.Zero(status.Failed, "retained orphan mutations do not report inbox failures")
	checks.Zero(status.Pending)
	must.NotNil(status.LastReceivedAt)
	checks.True(old.Equal(*status.LastReceivedAt))
}

func TestLineIdentityChannelIsolation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	raw := "U00000000000000000000000000000001"
	a, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", "channel:2000000001:user:"+raw, "Synthetic sender")
	must.NoError(err)
	b, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", "channel:2000000002:user:"+raw, "Synthetic sender")
	must.NoError(err)
	checks.NotEqual(a, b)
	var slug, kind, value string
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT s.slug,p.scope_kind,p.scope_value FROM participant_identifiers p JOIN communication_services s ON s.id=p.service_id WHERE p.participant_id=?`), a).Scan(&slug, &kind, &value))
	checks.Equal("line", slug)
	checks.Equal("line_channel", kind)
	checks.Equal("2000000001", value)
	observation, err := st.RecordContactObservationContext(ctx, a, store.ParticipantContactObservationInput{AddressKind: store.ContactAddressProviderIdentity, ServiceSlug: new("line"), ScopeKind: new(kind), ScopeValue: new(value), ProviderUserID: &raw, OriginalValue: raw, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
	must.NoError(err)
	checks.Equal(raw, observation.Observation.NormalizedValue)
}

func TestLineMembershipOutOfOrder(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "group:C00000000000000000000000000000001", "group_chat", "")
	must.NoError(err)
	member := "channel:2000000001:user:U00000000000000000000000000000001"
	pid, err := st.EnsureParticipantByIdentifier("line_user_id", member, "")
	must.NoError(err)
	base := time.Now().UTC().Truncate(time.Second)
	must.NoError(st.SetLineMembershipContext(ctx, conv, pid, member, false, base.Add(2*time.Hour)))
	must.NoError(st.SetLineMembershipContext(ctx, conv, pid, member, true, base))
	must.NoError(st.SetLineMembershipContext(ctx, conv, pid, member, false, base.Add(time.Hour)))
	var joined, left sql.NullTime
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT joined_at,left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conv, pid).Scan(&joined, &left))
	must.True(left.Valid)
	checks.WithinDuration(base.Add(2*time.Hour), left.Time, time.Millisecond)
	must.NoError(st.SetLineMembershipContext(ctx, conv, pid, member, true, base.Add(3*time.Hour)))
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT joined_at,left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conv, pid).Scan(&joined, &left))
	checks.False(left.Valid, "a rejoined participant must be active to existing messaging-route queries")
}

func TestLineMessagePresenceSkipsUnchangedMembership(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	member := "channel:2000000001:user:U00000000000000000000000000000001"
	participant, err := st.EnsureParticipantByIdentifier("line_user_id", member, "Synthetic sender")
	must.NoError(err)
	base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	persist := func(id string, at time.Time) int64 {
		t.Helper()
		messageID, err := st.PersistLineGroupMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "line", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: "group:C00000000000000000000000000000001", ConversationType: "group_chat", Participants: []store.ConversationParticipantRef{{ParticipantID: participant, Role: "member"}}, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: "synthetic message", Valid: true},
		}, member)
		must.NoError(err)
		return messageID
	}
	first := persist("first", base)
	var conversation int64
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), first).Scan(&conversation))
	type projection struct {
		joined, left sql.NullTime
		revision     int64
	}
	snapshot := func() projection {
		t.Helper()
		_, err := st.ExpandActivityConversationQueueContext(ctx, func() error { return nil })
		must.NoError(err)
		var state projection
		must.NoError(st.DB().QueryRow(st.Rebind(`SELECT cp.joined_at,cp.left_at,q.revision FROM conversation_participants cp
 JOIN activity_projection_queue q ON q.message_id=? WHERE cp.conversation_id=? AND cp.participant_id=?`), first, conversation, participant).Scan(&state.joined, &state.left, &state.revision))
		return state
	}
	initial := snapshot()
	persist("second", base.Add(time.Minute))
	checks.Equal(initial, snapshot(), "an active sender's next message must preserve membership and leave existing history queued at the same revision")
	var presence int64
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT joined_at FROM line_memberships WHERE source_id=? AND member_identifier=?`), source.ID, member).Scan(&presence))
	checks.Equal(base.Add(time.Minute).UnixMilli(), presence, "the operational presence clock still advances")

	leave := base.Add(2 * time.Minute)
	must.NoError(st.SetLineMembershipContext(ctx, conversation, participant, member, false, leave))
	departed := snapshot()
	must.True(departed.left.Valid)
	checks.Equal(leave.UnixMilli(), departed.left.Time.UnixMilli())
	checks.Greater(departed.revision, initial.revision, "a real leave invalidates history")
	must.NoError(st.SetLineMembershipContext(ctx, conversation, participant, member, false, leave))
	checks.Equal(departed, snapshot(), "replaying a leave must preserve membership and queue revisions")
	join := base.Add(3 * time.Minute)
	must.NoError(st.SetLineMembershipContext(ctx, conversation, participant, member, true, join))
	rejoined := snapshot()
	checks.False(rejoined.left.Valid)
	checks.Equal(join.UnixMilli(), rejoined.joined.Time.UnixMilli())
	checks.Greater(rejoined.revision, departed.revision, "a real rejoin invalidates history")
	must.NoError(st.SetLineMembershipContext(ctx, conversation, participant, member, true, join))
	checks.Equal(rejoined, snapshot(), "replaying a join must preserve membership and queue revisions")
}

func TestLineBotConfigMerge(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	ctx := context.Background()
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	must.NoError(st.UpdateSourceSyncConfig(src.ID, `{"no_default_identity":true}`))
	must.NoError(st.SetLineBotContext(ctx, src.ID, "U00000000000000000000000000000002"))
	bot, err := st.LineBotUserIDContext(ctx, src.ID)
	must.NoError(err)
	checks.Equal("U00000000000000000000000000000002", bot)
	var raw string
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT sync_config FROM sources WHERE id=?`), src.ID).Scan(&raw))
	checks.Contains(raw, "no_default_identity")
}

func TestLineServiceMigrationPreservesCuratedPolicy(t *testing.T) {
	for _, curated := range []bool{false, true} {
		t.Run(map[bool]string{false: "seed", true: "curated"}[curated], func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)

			st := testutil.NewTestStore(t)
			ctx := context.Background()
			_, err := st.DB().Exec(`DELETE FROM applied_migrations WHERE name='line_channel_scope_v1'`)
			must.NoError(err)
			_, err = st.DB().Exec(`UPDATE communication_services SET scope_policy='none',default_scope_kind=NULL,normalization='lower',updated_at=created_at WHERE slug='line'`)
			must.NoError(err)
			if curated {
				_, err = st.DB().Exec(`UPDATE communication_services SET display_label='Custom LINE',updated_at='2026-01-01 00:00:00' WHERE slug='line'`)
				must.NoError(err)
			}
			must.NoError(st.InitSchemaContext(ctx))
			var policy, label string
			must.NoError(st.DB().QueryRow(`SELECT scope_policy,display_label FROM communication_services WHERE slug='line'`).Scan(&policy, &label))
			if curated {
				checks.Equal("none", policy)
				checks.Equal("Custom LINE", label)
			} else {
				checks.Equal("optional", policy)
				checks.Equal("LINE", label)
			}
		})
	}
}

func TestLineQuotesStayInConversation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	st := testutil.NewTestStore(t)
	ctx := context.Background()
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "group:synthetic-a", "group_chat", "")
	must.NoError(err)
	other, err := st.EnsureConversationWithType(src.ID, "group:synthetic-b", "group_chat", "")
	must.NoError(err)
	persist := func(key string, conversationID int64, metadata string) int64 {
		t.Helper()
		meta := sql.NullString{String: metadata, Valid: metadata != ""}
		id, err := st.PersistMessageContext(ctx, &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, SourceMessageID: key, MessageType: "line", ConversationID: conversationID}, Metadata: &meta})
		require.NoError(t, err)
		return id
	}
	crossConversation := persist("cross-conversation", conv, `{"quotedMessageId":"other-parent"}`)
	child := persist("child", conv, `{"quotedMessageId":"parent"}`)
	unrelated := persist("unrelated-child", conv, `{"quotedMessageId":"unrelated-parent"}`)
	persist("unrelated-parent", conv, "")
	persist("other-parent", other, "")
	must.NoError(st.ResolveLineQuotesForMessagesContext(ctx, src.ID, "group:synthetic-a", []string{"child", "cross-conversation"}))
	var reply sql.NullInt64
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child).Scan(&reply))
	checks.False(reply.Valid)
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), crossConversation).Scan(&reply))
	checks.False(reply.Valid, "quotes stay within their conversation")
	parent := persist("parent", conv, "")
	must.NoError(st.ResolveLineQuotesForMessagesContext(ctx, src.ID, "group:synthetic-a", []string{"parent"}))
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child).Scan(&reply))
	checks.True(reply.Valid)
	checks.Equal(parent, reply.Int64)
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), unrelated).Scan(&reply))
	checks.False(reply.Valid, "settling a batch leaves unrelated messages alone")
	must.NoError(st.ResolveLineQuotesForMessagesContext(ctx, src.ID, "group:synthetic-a", []string{"unrelated-child"}))
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), unrelated).Scan(&reply))
	checks.True(reply.Valid, "a batch's reply can quote an already archived parent")
}

func TestMergeSourcesRejectsPendingLineInbox(t *testing.T) {
	for _, state := range []string{"pending", "retry"} {
		t.Run(state, func(t *testing.T) {
			must := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("line", "2000000000")
			must.NoError(err)
			into, err := st.GetOrCreateSource("line", "2000000001")
			must.NoError(err)
			botID := "U00000000000000000000000000000001"
			must.NoError(st.SetLineBotContext(t.Context(), source.ID, botID))
			must.NoError(st.EnqueueLineEventsContext(t.Context(), source.ID, "2000000000", botID, []store.LineWebhookEvent{{WebhookEventID: "accepted", EventType: "message", EventTimestamp: 123, RawJSON: `{"type":"message","timestamp":123,"webhookEventId":"accepted","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}`}}))
			rows, err := st.DueLineEventsContext(t.Context(), source.ID, time.Now(), 10)
			must.NoError(err)
			must.Len(rows, 1)
			if state == "retry" {
				must.NoError(st.FinishLineEventContext(t.Context(), rows[0].ID, "retry", "synthetic retry", time.Now().Add(time.Hour)))
			}
			req := store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: into.ID}
			_, err = st.MergeSourcesContext(t.Context(), req)
			must.ErrorIs(err, store.ErrSourceMergeInvalid)
			settings, err := st.GetSourceSettingsContext(t.Context(), source.ID)
			must.NoError(err)
			assert.Zero(t, settings.MergedIntoSourceID)
			must.NoError(st.FinishLineEventContext(t.Context(), rows[0].ID, "processed", "", time.Time{}))
			_, err = st.MergeSourcesContext(t.Context(), req)
			must.NoError(err)
		})
	}
}

func TestLineMembershipClocksSurviveParticipantMerge(t *testing.T) {
	for _, direction := range []string{"departed_into_active", "active_into_departed"} {
		t.Run(direction, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			source, err := st.GetOrCreateSource("line", "2000000001")
			must.NoError(err)
			conversation, err := st.EnsureConversationWithType(source.ID, "group:C00000000000000000000000000000001", "group_chat", "")
			must.NoError(err)
			userA := "channel:2000000001:user:U00000000000000000000000000000002"
			userB := "channel:2000000001:user:U00000000000000000000000000000009"
			departed, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", userA, "")
			must.NoError(err)
			active, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", userB, "")
			must.NoError(err)
			base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
			must.NoError(st.SetLineMembershipContext(ctx, conversation, departed, userA, true, base))
			must.NoError(st.SetLineMembershipContext(ctx, conversation, departed, userA, false, base.Add(10*time.Millisecond)))
			must.NoError(st.SetLineMembershipContext(ctx, conversation, active, userB, true, base.Add(20*time.Millisecond)))
			must.NoError(st.SetLineMembershipContext(ctx, conversation, departed, userA, false, base.Add(30*time.Millisecond)))
			absorbed, survivor := departed, active
			if direction == "active_into_departed" {
				absorbed, survivor = active, departed
			}
			must.NoError(st.MergeParticipantsContext(ctx, absorbed, survivor))
			checkMembership := func() {
				t.Helper()
				var left sql.NullTime
				must.NoError(st.DB().QueryRow(st.Rebind(`SELECT left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversation, survivor).Scan(&left))
				checks.False(left.Valid, "one active linked LINE identity keeps the merged participant active")
				var routes int
				must.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conversation).Scan(&routes))
				checks.Equal(1, routes)
			}
			checkMembership()
			must.NoError(st.SetLineMembershipContext(ctx, conversation, survivor, userA, true, base.Add(-time.Millisecond)))
			checkMembership()
			must.NoError(st.SetLineMembershipContext(ctx, conversation, survivor, userA, true, base.Add(25*time.Millisecond)))
			checkMembership()
		})
	}
}

func TestLineMessageOnlyMembershipSurvivesParticipantMerge(t *testing.T) {
	for _, chat := range []struct{ name, identifierType, identifier string }{
		{"group", "line_user_id", "channel:2000000001:user:U00000000000000000000000000000002"},
		{"anonymous_group", "line_anonymous", "channel:2000000001:group:C00000000000000000000000000000001"},
	} {
		for _, direction := range []string{"message_only_into_departed", "departed_into_message_only"} {
			t.Run(chat.name+"/"+direction, func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				st := testutil.NewTestStore(t)
				ctx := t.Context()
				source, err := st.GetOrCreateSource("line", "2000000001")
				must.NoError(err)
				present, err := st.EnsureParticipantByIdentifierContext(ctx, chat.identifierType, chat.identifier, "")
				must.NoError(err)
				other := "channel:2000000001:user:U00000000000000000000000000000009"
				departed, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", other, "")
				must.NoError(err)
				base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
				persist := func(id string, participant int64, at time.Time) {
					t.Helper()
					_, err := st.PersistLineGroupMessageContext(ctx, &store.MessagePersistData{
						Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "line", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
						Conversation: &store.ConversationPersistData{SourceConversationID: "group:C00000000000000000000000000000001", ConversationType: "group_chat", Participants: []store.ConversationParticipantRef{{ParticipantID: participant, Role: "member"}}, PreserveExistingParticipants: true},
						BodyText:     sql.NullString{String: "message proves this sender is present", Valid: true},
					}, chat.identifier)
					must.NoError(err)
				}
				persist("message-only", present, base)
				var conversation int64
				must.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), source.ID, "group:C00000000000000000000000000000001").Scan(&conversation))
				must.NoError(st.SetLineMembershipContext(ctx, conversation, departed, other, false, base.Add(20*time.Millisecond)))
				absorbed, survivor := present, departed
				if direction == "departed_into_message_only" {
					absorbed, survivor = departed, present
				}
				must.NoError(st.MergeParticipantsContext(ctx, absorbed, survivor))
				leftAt := func() sql.NullTime {
					t.Helper()
					var left sql.NullTime
					must.NoError(st.DB().QueryRow(st.Rebind(`SELECT left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversation, survivor).Scan(&left))
					return left
				}
				checks.False(leftAt().Valid)
				must.NoError(st.SetLineMembershipContext(ctx, conversation, survivor, other, false, base.Add(30*time.Millisecond)))
				checks.False(leftAt().Valid, "message presence survives later events")
				if chat.name == "anonymous_group" {
					return
				}
				must.NoError(st.SetLineMembershipContext(ctx, conversation, survivor, chat.identifier, false, base.Add(40*time.Millisecond)))
				left := leftAt()
				must.True(left.Valid)
				checks.Equal(base.Add(40*time.Millisecond).UnixMilli(), left.Time.UnixMilli())
				persist("delayed-message", survivor, base.Add(5*time.Millisecond))
				checks.True(leftAt().Valid, "out-of-order message presence cannot override a later leave")
			})
		}
	}
}
