package store_test

import (
	"context"
	"database/sql"
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
	invalid := event
	invalid.WebhookEventID = ""
	valid := event
	valid.WebhookEventID = "event-2"
	must.Error(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, []store.LineWebhookEvent{valid, invalid}))
	rows, err = st.DueLineEventsContext(ctx, src.ID, at.Add(time.Second), 100)
	must.NoError(err)
	checks.Len(rows, 1, "invalid batch rolls back every insert")
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
	events := []store.LineWebhookEvent{{WebhookEventID: "original", EventType: "message", EventTimestamp: 20, RawJSON: `{"text":"synthetic"}`, ReceivedAt: old}, {WebhookEventID: "orphan", EventType: "unsend", TargetMessageID: "late", EventTimestamp: 10, RawJSON: `{"unsend":{"messageId":"late"}}`, ReceivedAt: old}}
	must.NoError(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, events))
	rows, err := st.DueLineEventsContext(ctx, src.ID, now, 10)
	must.NoError(err)
	must.Len(rows, 2)
	checks.Equal("orphan", rows[0].WebhookEventID)
	must.NoError(st.FinishLineEventContext(ctx, rows[0].ID, "failed", "target not yet received", time.Time{}))
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
	var raw string
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT raw_json FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), src.ID, "original").Scan(&raw))
	checks.Equal("{}", raw)
	must.NoError(st.EnqueueLineEventsContext(ctx, src.ID, "2000000001", botID, events[:1]))
	due, err = st.DueLineEventsContext(ctx, src.ID, now, 10)
	must.NoError(err)
	checks.Empty(due, "retained event id rejects replay after payload cleanup")
	mutations, err := st.LineMutationsContext(ctx, src.ID, "late")
	must.NoError(err)
	must.Len(mutations, 1)
	checks.Contains(mutations[0].RawJSON, "late")
	status, err := st.LineInboxStatusContext(ctx, src.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Failed)
	checks.Zero(status.Pending)
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
	child := persist("child", conv, `{"quotedMessageId":"parent"}`)
	persist("parent", other, "")
	must.NoError(st.ResolveLineQuotesForConversationContext(ctx, src.ID, "group:synthetic-a"))
	var reply sql.NullInt64
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child).Scan(&reply))
	checks.False(reply.Valid)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET conversation_id=? WHERE source_id=? AND source_message_id='parent'`), conv, src.ID)
	must.NoError(err)
	must.NoError(st.ResolveLineQuotesForConversationContext(ctx, src.ID, "group:synthetic-a"))
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child).Scan(&reply))
	checks.True(reply.Valid)
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
