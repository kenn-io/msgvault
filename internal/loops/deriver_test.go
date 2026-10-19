package loops

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func deriveExec(t *testing.T, f *storetest.Fixture, query string, args ...any) {
	t.Helper()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(query), args...)
	require.NoError(t, err)
}
func deriveStamp(f *storetest.Fixture, at time.Time) any {
	if f.Store.IsPostgreSQL() {
		return at
	}
	return at.UTC().Format(store.SQLiteTimestampLayout)
}
func deriveFixture(t *testing.T) (*storetest.Fixture, config.LoopsConfig, time.Time) {
	t.Helper()
	f := storetest.New(t)
	cfg := config.DefaultLoopsConfig()
	cfg.Lookback = 0
	cfg.SignalsBackfillBatch = 2
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	deriveExec(t, f, "UPDATE conversations SET conversation_type='email_thread',last_message_at=? WHERE id=?", deriveStamp(f, now.Add(-48*time.Hour)), f.ConvID)
	return f, cfg, now
}
func deriveInbound(t *testing.T, f *storetest.Fixture, now time.Time) int64 {
	t.Helper()
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, peer)
	id, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: fmt.Sprintf("inbound-%d", f.ConvID), MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true}, Subject: sql.NullString{String: "A question", Valid: true}})
	require.NoError(t, err)
	deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", id, owner)
	deriveExec(t, f, "UPDATE messages SET content_changed_at=? WHERE id=?", deriveStamp(f, now.Add(-time.Hour)), id)
	return id
}
func TestDeriverUndatedMessageDoesNotStarveOtherConversations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	message := deriveInbound(t, f, now)
	deriveExec(t, f, "UPDATE messages SET sent_at=NULL,received_at=NULL,internal_date=NULL WHERE id=?", message)
	undatedConversation := f.ConvID
	datedConversation, err := f.Store.EnsureConversation(f.Source.ID, "dated-conversation", "Dated conversation")
	requirements.NoError(err)
	f.ConvID = datedConversation
	deriveInbound(t, f, now)
	f.ConvID = undatedConversation
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	undated, err := f.Store.ListLiveConversationLoopsContext(t.Context(), undatedConversation)
	requirements.NoError(err)
	assertions.Empty(undated, "an unknown occurrence time cannot establish an overdue obligation")
	dated, err := f.Store.ListLiveConversationLoopsContext(t.Context(), datedConversation)
	requirements.NoError(err)
	assertions.Len(dated, 1, "one undated row must not poison the conversation queue")
}

func TestDeriverUndatedLiveAnchorRetainsCoverageGap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.Lookback = config.DefaultLoopsConfig().Lookback
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	message := deriveInbound(t, f, now)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	deriveExec(t, f, "UPDATE messages SET sent_at=NULL,received_at=NULL,internal_date=NULL WHERE id=?", message)
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "changed", now))
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	retained, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("open", retained.Status)
	var evidence []Evidence
	requirements.NoError(json.Unmarshal(retained.EvidenceJSON, &evidence))
	requirements.NotEmpty(evidence)
	assertions.True(evidence[0].CoverageGap)
}

func TestDeriverGCInvalidatesConversationAfterSourceDeletedAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	messageID := deriveInbound(t, f, now)
	sourceMessageID := fmt.Sprintf("inbound-%d", f.ConvID)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	requirements.NotNil(live[0].AnchorMessageID)
	assertions.Equal(messageID, *live[0].AnchorMessageID)
	loopID := live[0].ID

	var revision, processedRevision int64
	readQueue := func() {
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT revision,processed_revision
			FROM loop_conversation_queue WHERE conversation_id=?`), f.ConvID).Scan(&revision, &processedRevision))
	}
	readQueue()
	assertions.Equal(revision, processedRevision, "the first projection must drain its queue")
	requirements.NoError(f.Store.MarkMessageDeleted(f.Source.ID, sourceMessageID))
	readQueue()
	assertions.Equal(revision, processedRevision, "source tombstones are consumed by the feed until physical GC")

	plan, err := f.Store.PlanGCContext(t.Context())
	requirements.NoError(err)
	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	requirements.NoError(err)
	requirements.Equal(int64(1), deleted)
	readQueue()
	assertions.Greater(revision, processedRevision, "physical deletion must invalidate the conversation after its anchor is purged")

	d.now = func() time.Time { return now.Add(time.Minute) }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	closed, err := f.Store.GetOpenLoopContext(t.Context(), loopID)
	requirements.NoError(err)
	assertions.Equal("closed", closed.Status)
	requirements.NotNil(closed.ClosedReason)
	assertions.Equal("withdrawn", *closed.ClosedReason)
}

func TestDeriverRefreshesRetainedReplyOwedCounterpartyAfterSenderCorrection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	anchorID := deriveInbound(t, f, now)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	loopID := rows[0].ID
	var originalSender int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT sender_id FROM messages WHERE id=?"), anchorID).Scan(&originalSender))

	correctedSender := f.EnsureParticipant("corrected@example.test", "Corrected Sender", "example.test")
	deriveExec(t, f, "UPDATE messages SET sender_id=? WHERE id=?", correctedSender, anchorID)
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "sender-corrected", now.Add(time.Minute)))
	d.now = func() time.Time { return now.Add(time.Minute) }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	retained, err := f.Store.GetOpenLoopContext(t.Context(), loopID)
	requirements.NoError(err)
	requirements.NotNil(retained.CounterpartyParticipantID)
	assertions.NotEqual(originalSender, *retained.CounterpartyParticipantID)
	assertions.Equal(correctedSender, *retained.CounterpartyParticipantID)
}

func TestDeriverKeepsSnoozedAwaitingLoopWhenOtherRecipientIsMuted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	mutedRecipient := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	activeRecipient := f.EnsureParticipant("blair@example.test", "Blair", "example.test")
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	deriveExec(t, f, "UPDATE conversations SET conversation_type='group_chat',last_message_at=? WHERE id=?", deriveStamp(f, now.Add(-time.Hour)), f.ConvID)
	for _, participantID := range []int64{mutedRecipient, activeRecipient} {
		deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, participantID)
	}

	priorMessageID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "prior-group-message",
			MessageType: "email", SenderID: sql.NullInt64{Int64: activeRecipient, Valid: true},
			SentAt: sql.NullTime{Time: now.Add(-2 * time.Hour), Valid: true}, Subject: sql.NullString{String: "Earlier update", Valid: true}},
		BodyText: sql.NullString{String: "Here is the earlier update.", Valid: true},
	})
	requirements.NoError(err)
	deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", priorMessageID, owner)
	questionID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "group-question",
			MessageType: "email", SenderID: sql.NullInt64{Int64: owner, Valid: true}, IsFromMe: true,
			SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}, Subject: sql.NullString{String: "Review request", Valid: true}},
		BodyText: sql.NullString{String: "Can you review this?", Valid: true},
	})
	requirements.NoError(err)
	for _, participantID := range []int64{mutedRecipient, activeRecipient} {
		deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", questionID, participantID)
	}
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	loopID := live[0].ID
	requirements.Equal("awaiting_reply", live[0].Kind)
	requirements.NotNil(live[0].CounterpartyParticipantID)
	assertions.Equal(activeRecipient, *live[0].CounterpartyParticipantID)
	until := now.Add(24 * time.Hour)
	_, err = f.Store.SnoozeLoopContext(t.Context(), loopID, until, now)
	requirements.NoError(err)
	_, err = f.Store.CreateLoopSuppressionContext(t.Context(), store.LoopSuppressionRequest{
		TargetKind: "participant", ParticipantID: &mutedRecipient,
	}, now.Add(time.Minute))
	requirements.NoError(err)
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "muted-other-recipient", now.Add(time.Minute)))
	d.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)

	retained, err := f.Store.GetOpenLoopContext(t.Context(), loopID)
	requirements.NoError(err)
	assertions.Equal("held", retained.Status)
	requirements.NotNil(retained.CounterpartyParticipantID)
	assertions.Equal(activeRecipient, *retained.CounterpartyParticipantID)
	requirements.NotNil(retained.SnoozedUntil)
	assertions.True(until.Equal(*retained.SnoozedUntil))
	afterRefresh, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(afterRefresh, 1)
	assertions.Equal(loopID, afterRefresh[0].ID)
}

func TestStoredVerdictPreservesEvidenceJSONContract(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name         string
		subject      string
		truncatedRun bool
		want         string
	}{
		{
			name:    "default optional fields omitted",
			subject: "Question",
			want:    `[{"message_id":13,"conversation_id":7,"occurred_at":"2026-10-17T12:00:00Z","subject":"Question"}]`,
		},
		{
			name:         "truncated run retained",
			subject:      "Question",
			truncatedRun: true,
			want:         `[{"message_id":13,"conversation_id":7,"occurred_at":"2026-10-17T12:00:00Z","subject":"Question","truncated_run":true}]`,
		},
		{
			name:    "HTML-sensitive subject remains escaped",
			subject: "<Question>",
			want:    `[{"message_id":13,"conversation_id":7,"occurred_at":"2026-10-17T12:00:00Z","subject":"\u003cQuestion\u003e"}]`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			view := ConversationView{ConversationID: 7, SourceID: 11, ConversationType: "email_thread"}
			anchor := LoopMessage{ID: 13, OccurredAt: at, Subject: tt.subject}
			spec := Spec{Kind: ReplyOwed, Anchor: anchor, LastActivity: anchor, TruncatedRun: tt.truncatedRun}
			got, err := storedVerdict(view, Verdict{Open: &spec})
			require.NoError(t, err)
			require.NotNil(t, got.Open)
			assert.Equal(t, tt.want, string(got.Open.EvidenceJSON))
		})
	}
}

func TestDeriverIdentityRescanCreatesFirstLoopAndShrinkWithdraws(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	deriveInbound(t, f, now)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(live, "unknown owner remains observed")
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	result, err := d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, result.RescanConversations)
	live, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	assertions.Equal("open", live[0].Status)
	cfg.Lookback = 24 * time.Hour
	d, err = NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("closed", got.Status)
	requirements.NotNil(got.ClosedReason)
	assertions.Equal("withdrawn", *got.ClosedReason)
}
func TestDeriverRebuildPreservesDoneAndSnooze(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	deriveInbound(t, f, now)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	until := now.Add(24 * time.Hour)
	_, err = f.Store.SnoozeLoopContext(t.Context(), live[0].ID, until, now)
	requirements.NoError(err)
	deriveExec(t, f, "UPDATE open_loops SET agenda_state='dismissed' WHERE id=?", live[0].ID)
	requirements.NoError(d.Rebuild(t.Context()))
	state, err := d.State(t.Context())
	requirements.NoError(err)
	assertions.Nil(state.FeedCursorAt)
	assertions.Equal("rebuild", state.RescanReason)
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("dismissed", got.AgendaState)
	requirements.NotNil(got.SnoozedUntil)
	assertions.True(until.Equal(*got.SnoozedUntil))
	_, err = f.Store.CloseLoopContext(t.Context(), got.ID, "handled", now)
	requirements.NoError(err)
	requirements.NoError(d.Rebuild(t.Context()))
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(live)
}

func TestDeriverKeepsBulkReplyLoopAfterOwnerParticipation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.ConversationMessageLimit = 3
	cfg.ConversationMessageLimitExtended = 3
	cfg.RunMessageLimit = 3
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")

	ownerMessage := &store.Message{
		SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "owner-participation",
		MessageType: "email", SenderID: sql.NullInt64{Int64: owner, Valid: true},
		SentAt: sql.NullTime{Time: now.Add(-72 * time.Hour), Valid: true}, IsFromMe: true,
		Subject: sql.NullString{String: "Here is the update", Valid: true},
	}
	ownerMessageID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: ownerMessage, BodyText: sql.NullString{String: "Here is the update.", Valid: true},
	})
	requirements.NoError(err)
	deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", ownerMessageID, peer)

	bulkSignals := store.AutomationSignalsFromMIME(mime.AutomationSignals{PrecedenceBulk: true})
	bulkMessage := &store.Message{
		SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "bulk-reply",
		MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true},
		SentAt:  sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true},
		Subject: sql.NullString{String: "Automatic update", Valid: true},
	}
	bulkMessageID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: bulkMessage, BodyText: sql.NullString{String: "A bulk-marked reply.", Valid: true},
		AutomationSignals: &bulkSignals,
	})
	requirements.NoError(err)
	deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", bulkMessageID, owner)
	deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, peer)
	deriveExec(t, f, "UPDATE conversations SET last_message_at=? WHERE id=?", deriveStamp(f, now.Add(-48*time.Hour)), f.ConvID)

	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	requirements.NotNil(live[0].AnchorMessageID)
	assertions.Equal(bulkMessageID, *live[0].AnchorMessageID)
	until := now.Add(24 * time.Hour)
	_, err = f.Store.SnoozeLoopContext(t.Context(), live[0].ID, until, now)
	requirements.NoError(err)
	for i, at := range []time.Time{now.Add(-time.Hour), now.Add(-30 * time.Minute)} {
		neutral := &store.Message{
			SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: fmt.Sprintf("neutral-followup-%d", i),
			MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true},
			SentAt: sql.NullTime{Time: at, Valid: true}, Subject: sql.NullString{String: "Thread observation", Valid: true},
		}
		_, err = f.Store.UpsertMessage(neutral)
		requirements.NoError(err)
	}
	deriveExec(t, f, "UPDATE conversations SET last_message_at=? WHERE id=?", deriveStamp(f, now.Add(-30*time.Minute)), f.ConvID)

	now = now.Add(time.Minute)
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "bulk-reply-refresh", now))
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	preserved, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("open", preserved.Status)
	assertions.Contains(string(preserved.EvidenceJSON), fmt.Sprintf(`"owner_participation_message_id":%d`, ownerMessageID))
	assertions.Contains(string(preserved.EvidenceJSON), `"coverage_gap":true`, "saved participation outside the extended window remains uncertain")
	requirements.NotNil(preserved.SnoozedUntil)
	assertions.True(until.Equal(*preserved.SnoozedUntil))
	afterRefresh, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(afterRefresh, 1)
	assertions.Equal(live[0].ID, afterRefresh[0].ID)
}

func TestDeriverWithdrawsBulkReplyDiscoveredByBackfillWithoutOwnerParticipation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, peer)

	message := &store.Message{
		SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "historical-bulk-without-owner-turn",
		MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true},
		SentAt:  sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true},
		Subject: sql.NullString{String: "Weekly update", Valid: true},
	}
	messageID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: message, BodyText: sql.NullString{String: "A mailing list update.", Valid: true},
		RawFormat: "mime", RawMIME: []byte("From: peer@example.test\r\nTo: owner@example.test\r\nPrecedence: bulk\r\nSubject: Weekly update\r\n\r\nA mailing list update."),
	})
	requirements.NoError(err)
	deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", messageID, owner)
	deriveExec(t, f, "UPDATE conversations SET last_message_at=? WHERE id=?", deriveStamp(f, now.Add(-48*time.Hour)), f.ConvID)

	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1, "the first drain runs before MIME signal backfill")
	assertions.Equal(messageID, *live[0].AnchorMessageID)
	loopID := live[0].ID

	var precedenceBulk bool
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT precedence_bulk FROM message_automation_signals WHERE message_id=?"), messageID).Scan(&precedenceBulk))
	assertions.True(precedenceBulk, "the background MIME pass discovers the bulk header")

	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(live, "a backfilled bulk anchor without owner participation is not a reply owed")
	closed, err := f.Store.GetOpenLoopContext(t.Context(), loopID)
	requirements.NoError(err)
	requirements.NotNil(closed.ClosedReason)
	assertions.Equal("withdrawn", *closed.ClosedReason)
}

func TestDeriverExcludesImportedUnsentSynctechSMSFromTurnDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  synctechsms.SMSType
	}{
		{name: "draft", typ: synctechsms.SMSTypeDraft},
		{name: "outbox", typ: synctechsms.SMSTypeOutbox},
		{name: "failed", typ: synctechsms.SMSTypeFailed},
		{name: "queued", typ: synctechsms.SMSTypeQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, cfg, now := deriveFixture(t)
			ownerPhone := "+15550000001"
			peerPhone := "+15550000002"
			xml := fmt.Sprintf(`<smses count="2">
  <sms address="%s" date="%d" type="%d" body="Can you send the file?" read="1" status="-1" />
  <sms address="%s" date="%d" type="%d" body="Unsent reply" read="1" status="0" />
</smses>`, peerPhone, now.Add(-48*time.Hour).UnixMilli(), synctechsms.SMSTypeInbox,
				peerPhone, now.Add(-47*time.Hour).UnixMilli(), tc.typ)
			summary := importSynctechMessages(t, f, ownerPhone, xml)
			requirements.Len(summary.MessageIDs, 2)
			var conversationID int64
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT conversation_id FROM messages WHERE id=?"), summary.MessageIDs[0]).Scan(&conversationID))

			d, err := NewDeriver(f.Store, &cfg)
			requirements.NoError(err)
			d.now = func() time.Time { return now }
			_, err = d.RunOnce(t.Context())
			requirements.NoError(err)
			live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), conversationID)
			requirements.NoError(err)
			requirements.Len(live, 1)
			assertions.Equal(ReplyOwed, live[0].Kind)
			requirements.NotNil(live[0].AnchorMessageID)
			assertions.Equal(summary.MessageIDs[0], *live[0].AnchorMessageID)
		})
	}
}

func TestDeriverBackfillsOldSynctechUnsentStateBeforeDetection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	ownerPhone := "+15550000001"
	peerPhone := "+15550000002"
	xml := fmt.Sprintf(`<smses count="2">
  <sms address="%s" date="%d" type="%d" body="Can you send the file?" read="1" status="-1" />
  <sms address="%s" date="%d" type="%d" body="Unsent reply" read="1" status="0" />
</smses>`, peerPhone, now.Add(-48*time.Hour).UnixMilli(), synctechsms.SMSTypeInbox,
		peerPhone, now.Add(-47*time.Hour).UnixMilli(), synctechsms.SMSTypeOutbox)
	summary := importSynctechMessages(t, f, ownerPhone, xml)
	requirements.Len(summary.MessageIDs, 2)
	unsentID := summary.MessageIDs[1]
	var conversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT conversation_id FROM messages WHERE id=?"), summary.MessageIDs[0]).Scan(&conversationID))
	// Old imports left unsent owner messages at the schema's TRUE default.
	deriveExec(t, f, "UPDATE messages SET is_sent=TRUE WHERE id=?", unsentID)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), conversationID)
	requirements.NoError(err)
	requirements.Len(live, 1, "an archived unsent reply must not close the inbound request")
	assertions.Equal(ReplyOwed, live[0].Kind)
	requirements.NotNil(live[0].AnchorMessageID)
	assertions.Equal(summary.MessageIDs[0], *live[0].AnchorMessageID)
	var sent bool
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT is_sent FROM messages WHERE id=?"), unsentID).Scan(&sent))
	assertions.False(sent, "the native unsent state is persisted before loop derivation")
}

func TestDeriverKeepsUnverifiedSynctechConversationPendingWhileProcessingOtherWork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	emailAnchor := deriveInbound(t, f, now)

	const unsentCount = 101
	var xml strings.Builder
	fmt.Fprintf(&xml, `<smses count="%d">`, unsentCount+1)
	fmt.Fprintf(&xml, `<sms address="+15550000002" date="%d" type="1" body="Can you send the file?" read="1" status="-1" />`, now.Add(-48*time.Hour).UnixMilli())
	for i := range unsentCount {
		fmt.Fprintf(&xml, `<sms address="+15550000002" date="%d" type="4" body="Unsent reply %03d" read="1" status="0" />`, now.Add(-47*time.Hour+time.Duration(i)*time.Second).UnixMilli(), i)
	}
	xml.WriteString(`</smses>`)
	summary := importSynctechMessages(t, f, "+15550000001", xml.String())
	requirements.Len(summary.MessageIDs, unsentCount+1)
	var synctechConversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT conversation_id FROM messages WHERE id=?"), summary.MessageIDs[0]).Scan(&synctechConversationID))
	deriveExec(t, f, "UPDATE messages SET is_sent=TRUE WHERE id IN (SELECT message_id FROM message_raw WHERE raw_format='synctech_sms_xml_json') AND source_id=(SELECT id FROM sources WHERE source_type='synctech_sms') AND source_is_from_me=TRUE")

	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	result, err := d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, result.ProcessedConversations, "the conversation beyond the verified backfill cursor must wait")
	emailLoops, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(emailLoops, 1, "other queued conversations continue to derive")
	requirements.NotNil(emailLoops[0].AnchorMessageID)
	assertions.Equal(emailAnchor, *emailLoops[0].AnchorMessageID)
	synctechLoops, err := f.Store.ListLiveConversationLoopsContext(t.Context(), synctechConversationID)
	requirements.NoError(err)
	assertions.Empty(synctechLoops, "unverified native send state must not affect turn detection")
	pending, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(pending, 1, "the affected conversation remains queued for the next backfill batch")
	assertions.Equal(synctechConversationID, pending[0].ConversationID)

	result, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, result.ProcessedConversations, "the conversation becomes eligible after the remaining send state is verified")
	pending, err = f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	assertions.Empty(pending)
	synctechLoops, err = f.Store.ListLiveConversationLoopsContext(t.Context(), synctechConversationID)
	requirements.NoError(err)
	requirements.Len(synctechLoops, 1)
	assertions.Equal(ReplyOwed, synctechLoops[0].Kind)
	requirements.NotNil(synctechLoops[0].AnchorMessageID)
	assertions.Equal(summary.MessageIDs[0], *synctechLoops[0].AnchorMessageID)
}

func TestDeriverExcludesImportedSynctechCallLogs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	xml := fmt.Sprintf(`<calls count="1"><call number="+15550000002" duration="42" date="%d" type="1" presentation="1" /></calls>`, now.Add(-48*time.Hour).UnixMilli())
	summary := importSynctechRecords(t, f, "+15550000001", xml, false, true)
	requirements.Len(summary.MessageIDs, 1)
	var conversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT conversation_id FROM messages WHERE id=?"), summary.MessageIDs[0]).Scan(&conversationID))

	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), conversationID)
	requirements.NoError(err)
	assertions.Empty(live)
}

func importSynctechMessages(t *testing.T, f *storetest.Fixture, ownerPhone, xml string) synctechsms.ImportSummary {
	t.Helper()
	return importSynctechRecords(t, f, ownerPhone, xml, true, false)
}

func importSynctechRecords(t *testing.T, f *storetest.Fixture, ownerPhone, xml string, includeSMS, includeCalls bool) synctechsms.ImportSummary {
	t.Helper()
	source, err := f.Store.GetOrCreateSource(synctechsms.SourceType, ownerPhone)
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(source.ID, ownerPhone, "account-identifier"))
	file := synctechsms.BackupFile{Name: "synthetic-backup.xml", Opener: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(xml)), nil
	}}
	summary, err := synctechsms.NewImporter(f.Store, synctechsms.ImportOptions{
		OwnerPhone: ownerPhone, IncludeSMS: includeSMS, IncludeCalls: includeCalls,
	}).ImportFilesIntoSource(source.ID, []synctechsms.BackupFile{file})
	require.NoError(t, err)
	return summary
}

func TestDeriverWithdrawsMovedOrReclassifiedAnchor(t *testing.T) {
	for _, correction := range []string{"conversation", "direction"} {
		t.Run(correction, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, cfg, now := deriveFixture(t)
			anchor := deriveInbound(t, f, now)
			// Keep the old conversation eligible after its anchor moves.
			f.CreateMessage("remaining-observation")
			requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
			d, err := NewDeriver(f.Store, &cfg)
			requirements.NoError(err)
			d.now = func() time.Time { return now }
			_, err = d.RunOnce(t.Context())
			requirements.NoError(err)
			live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			requirements.Len(live, 1)
			if correction == "conversation" {
				destination, err := f.Store.EnsureConversation(f.Source.ID, "corrected-thread", "Corrected thread")
				requirements.NoError(err)
				deriveExec(t, f, "UPDATE messages SET conversation_id=? WHERE id=?", destination, anchor)
			} else {
				deriveExec(t, f, "UPDATE messages SET source_is_from_me=TRUE WHERE id=?", anchor)
			}
			now = now.Add(time.Minute)
			_, err = d.RunOnce(t.Context())
			requirements.NoError(err)
			got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
			requirements.NoError(err)
			assertions.Equal("closed", got.Status)
			requirements.NotNil(got.ClosedReason)
			assertions.Equal("withdrawn", *got.ClosedReason)
		})
	}
}

func TestDeriverSmallNativeGroupNeedsNoMention(t *testing.T) {
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	var err error
	f.Source, err = f.Store.GetOrCreateSource("beeper", "synthetic-group-source")
	requirements.NoError(err)
	f.ConvID, err = f.Store.EnsureConversation(f.Source.ID, "small-group", "Small group")
	requirements.NoError(err)
	deriveExec(t, f, "UPDATE conversations SET conversation_type='group_chat' WHERE id=?", f.ConvID)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	for _, participant := range []int64{owner, peer} {
		deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", f.ConvID, participant)
	}
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "group-question", MessageType: "chat", SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true}})
	requirements.NoError(err)
	// Native chat importers store group membership separately and only write
	// message_recipients for explicit mentions. There is no mention here.
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	assert.Equal(t, ReplyOwed, live[0].Kind)
}
func TestDeriverFeedFullPagesTieAndZeroCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.BatchMessages = 1
	cfg.FeedPages = 5
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	for _, id := range []int64{-2, -1, 0, 1, 2, 3} {
		deriveExec(t, f, "INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type)"+override+" VALUES(?,?,?,?,'email')", id, f.Source.ID, f.ConvID, fmt.Sprintf("tie-%d", id))
		deriveExec(t, f, "UPDATE messages SET content_changed_at=? WHERE id=?", deriveStamp(f, now.Add(-time.Hour)), id)
	}
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	result := RunResult{}
	requirements.NoError(d.consumeFeed(t.Context(), now, &result))
	assertions.Equal(5, result.FeedPages)
	assertions.Equal(5, result.ChangedMessages)
	state, err := d.State(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(2), state.FeedCursorMessageID)
	requirements.NotNil(state.FeedFrontier)
	assertions.True(now.Add(-time.Hour).Equal(*state.FeedFrontier))
	pending, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(pending, 1)
	assertions.Nil(pending[0].FeedBound, "initial unknown remains despite later page frontier")
	result = RunResult{}
	requirements.NoError(d.consumeFeed(t.Context(), now, &result))
	assertions.Equal(1, result.ChangedMessages)
	assertions.Equal(2, result.FeedPages)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.True(state.FeedFrontier.After(now.Add(-time.Hour)))
	requirements.
		// A consumed legal ID zero must resume strictly after it.
		NoError(f.Store.CommitLoopFeedContext(t.Context(), nil, store.ChangedMessagesAfter(now.Add(-time.Hour), 0), now.Add(-time.Hour), now))
	cfg.FeedPages = 1
	d.cfg = cfg
	result = RunResult{}
	requirements.NoError(d.consumeFeed(t.Context(), now, &result))
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(1), state.FeedCursorMessageID)
}
func TestDeriverCanceledRunKeepsDurableQueue(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	deriveInbound(t, f, now)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.RunOnce(ctx)
	requirements.ErrorIs(err, context.Canceled)
	state, err := d.State(t.Context())
	requirements.NoError(err)
	assertions.NotEmpty(state.LastError)
	assertions.Nil(state.LastSuccessAt)
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.Empty(state.LastError)
	assertions.NotNil(state.LastSuccessAt)
}

func TestDeriverSignalsBackfillWithdrawsAutomatedLiveLoop(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	id := deriveInbound(t, f, now)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	var raw bytes.Buffer
	w := zlib.NewWriter(&raw)
	_, err := w.Write([]byte("From: peer@example.test\r\nTo: owner@example.test\r\nAuto-Submitted: auto-generated\r\nSubject: A question\r\n\r\nGenerated text"))
	requirements.NoError(err)
	requirements.NoError(w.Close())
	deriveExec(t, f, "INSERT INTO message_raw(message_id,raw_data,raw_format,compression) VALUES(?,?,'mime','zlib')", id, raw.Bytes())
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	result, err := d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, result.BackfillParsed)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("closed", got.Status)
	requirements.NotNil(got.ClosedReason)
	assertions.Equal("withdrawn", *got.ClosedReason)
}

func TestDeriverUsesArchivedFromEnvelopeForAutomationEligibility(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, cfg, now := deriveFixture(t)
	id := deriveInbound(t, f, now)
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	deriveExec(t, f, `INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address)
		VALUES(?,?,'from','peer@example.test')`, id, peer)
	deriveExec(t, f, `UPDATE participants SET email_address='noreply@example.test' WHERE id=?`, peer)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1, "the archived human From address remains substantive after participant metadata changes")
	assertions.Equal(id, *live[0].AnchorMessageID)
}

func TestDeriverTimersPromoteHeldAndExpireSnoozeWithoutNewMessages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.ReplyOwedAfter = 72 * time.Hour
	cfg.ReconcileInterval = 365 * 24 * time.Hour
	deriveInbound(t, f, now)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	assertions.Equal("held", live[0].Status)
	_, err = f.Store.SnoozeLoopContext(t.Context(), live[0].ID, now.Add(time.Hour), now)
	requirements.NoError(err)
	now = now.Add(25 * time.Hour)
	result, err := d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(1), result.Promoted)
	assertions.Equal(int64(1), result.Unsnoozed)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live[0].ID)
	requirements.NoError(err)
	assertions.Equal("open", got.Status)
	assertions.Nil(got.SnoozedUntil)
}
func TestDeriverRescanRestartsMidEpochAndReconciles(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.RescanBatch = 1
	cfg.BatchConversations = 1
	deriveInbound(t, f, now)
	second, err := f.Store.EnsureConversation(f.Source.ID, "second-conversation", "Second conversation")
	requirements.NoError(err)
	deriveExec(t, f, "UPDATE conversations SET conversation_type='email_thread',last_message_at=? WHERE id=?", deriveStamp(f, now), second)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	state, err := d.State(t.Context())
	requirements.NoError(err)
	firstEpoch := state.RescanEpoch
	assertions.NotEmpty(firstEpoch)
	assertions.Zero(state.RulesetVersion)
	assertions.Nil(state.AppliedThrough)
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	result, err := d.RunOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, result.RescanConversations)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.NotEqual(firstEpoch, state.RescanEpoch)
	assertions.Equal(f.ConvID, state.RescanLastConversationID)
	assertions.Zero(state.RulesetVersion)
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.Empty(state.RescanReason)
	assertions.Equal(RulesetVersion, state.RulesetVersion)
	assertions.NotNil(state.AppliedThrough)
	now = now.Add(cfg.ReconcileInterval)
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.Equal("reconcile", state.RescanReason)
	// A changed ruleset certificate restarts even while another scan is running.
	deriveExec(t, f, "UPDATE loop_derivation_state SET ruleset_version=0,rescan_reason='',rescan_epoch=''")
	_, err = d.RunOnce(t.Context())
	requirements.NoError(err)
	state, err = d.State(t.Context())
	requirements.NoError(err)
	assertions.Equal("ruleset", state.RescanReason)
}

type deriveLogObserver struct{ observe func(slog.Record) }

func (h deriveLogObserver) Enabled(context.Context, slog.Level) bool      { return true }
func (h deriveLogObserver) Handle(_ context.Context, r slog.Record) error { h.observe(r); return nil }
func (h deriveLogObserver) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h deriveLogObserver) WithGroup(string) slog.Handler                 { return h }
func TestDeriverPreemptionResumesCommittedConversationAndBoundsBodyReads(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	cfg.RunMessageLimit = 2
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	second, err := f.Store.EnsureConversation(f.Source.ID, "second", "Second")
	requirements.NoError(err)
	for _, conv := range []int64{f.ConvID, second} {
		deriveExec(t, f, "UPDATE conversations SET conversation_type='email_thread',last_message_at=? WHERE id=?", deriveStamp(f, now), conv)
		deriveExec(t, f, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", conv, peer)
		for i := range 4 {
			id, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: conv, SourceMessageID: fmt.Sprintf("outbound-%d-%d", conv, i), MessageType: "email", SenderID: sql.NullInt64{Int64: owner, Valid: true}, SentAt: sql.NullTime{Time: now.Add(time.Duration(i) * time.Minute), Valid: true}, IsFromMe: true})
			requirements.NoError(err)
			deriveExec(t, f, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", id, peer)
			deriveExec(t, f, "INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)", id, "Can you confirm?")
		}
	}
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	d.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bodyReads := 0
	previous := slog.Default()
	slog.SetDefault(slog.New(deriveLogObserver{observe: func(r slog.Record) {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "stmt" && strings.Contains(a.Value.String(), "FROM message_bodies WHERE message_id") {
				bodyReads++
			}
			return true
		})
		if r.Message == "sql tx commit" || r.Message == "sql tx slow" {
			cancel()
		}
	}}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	result := RunResult{}
	err = d.drain(ctx, now, &result)
	slog.SetDefault(previous)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(1, result.ProcessedConversations)
	assertions.Equal(2, bodyReads)
	pending, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(pending, 1)
	result = RunResult{}
	requirements.NoError(d.drain(t.Context(), now, &result))
	assertions.Equal(1, result.ProcessedConversations)
	pending, err = f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	assertions.Empty(pending)
}

func TestDeriverFeedEmptyPageUsesExclusiveDatabaseBound(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	before, err := f.Store.ListChangedMessages(t.Context(), store.ChangedMessagesFrom(time.Time{}), 1)
	requirements.NoError(err)
	result := RunResult{}
	requirements.NoError(d.consumeFeed(t.Context(), now, &result))
	assertions.Equal(1, result.FeedPages)
	assertions.Zero(result.ChangedMessages)
	after, err := f.Store.ListChangedMessages(t.Context(), store.ChangedMessagesFrom(time.Time{}), 1)
	requirements.NoError(err)
	state, err := d.State(t.Context())
	requirements.NoError(err)
	requirements.NotNil(state.FeedFrontier)
	assertions.False(state.FeedFrontier.Before(before.CompleteThrough))
	assertions.False(state.FeedFrontier.After(after.CompleteThrough))
	assertions.Nil(state.FeedCursorAt)
}
func TestDeriverFeedStalledCommitBoundDoesNotClaimWallClock(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, cfg, now := deriveFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL writer can stay open while projection commits")
	}
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), f.Store.Rebind("INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,'pending-message','email')"), f.Source.ID, f.ConvID)
	requirements.NoError(err)
	bound, err := f.Store.ListChangedMessages(t.Context(), store.ChangedMessagesFrom(time.Time{}), 1)
	requirements.NoError(err)
	requirements.Empty(bound.Messages)
	d, err := NewDeriver(f.Store, &cfg)
	requirements.NoError(err)
	for range 2 {
		result := RunResult{}
		requirements.NoError(d.consumeFeed(t.Context(), now, &result))
		assertions.Equal(1, result.FeedPages)
		assertions.Zero(result.ChangedMessages)
		state, err := d.State(t.Context())
		requirements.NoError(err)
		requirements.NotNil(state.FeedFrontier)
		assertions.True(bound.CompleteThrough.Equal(*state.FeedFrontier))
	}
}
