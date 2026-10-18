package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopDetectorIncludesRecipientsAsEmailParticipants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	sentAt := now.Add(-96 * time.Hour)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	message := f.CreateMessage("outbound-question")
	loopExec(t, f.Store, `UPDATE conversations SET conversation_type='email_thread',last_message_at=NULL WHERE id=?`, f.ConvID)
	loopExec(t, f.Store, `UPDATE messages SET sender_id=?,source_is_from_me=TRUE,sent_at=?,subject='Question',snippet='Could you review this?' WHERE id=?`, owner, sentAt, message)
	loopExec(t, f.Store, `INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`, message, peer)
	requirements.NoError(f.Store.UpsertMessageBody(message,
		sql.NullString{String: "Could you review this?", Valid: true}, sql.NullString{}))

	loaded, err := f.Store.LoadLoopConversationContext(t.Context(), f.ConvID, 100)
	requirements.NoError(err)
	assertions.True(sentAt.Equal(loaded.LastMessageAt), "message timestamps provide the lookback timestamp when conversation metadata is missing")
	view := loopViewFromLoadedConversation(t, loaded, f.Store)
	verdict := loops.Detect(view, nil, now, loops.DefaultThresholds(), loops.Suppressions{})
	requirements.NoError(verdict.Err)
	requirements.NotNil(verdict.Open, "an outbound email question to a recipient must be eligible even without a conversation roster row")
	assertions.Equal(loops.AwaitingReply, verdict.Open.Kind)
	assertions.Equal(peer, verdict.Open.Counterparty.ID)
}

func TestLoopApplyPreservesLiveAnchorWhenOppositeBoundaryIsUnknown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(4)

	completed := loopOpen(t, f, ids[0], ids[0], now)
	applyLoop(t, f, completed, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	_, err = f.Store.CloseLoopContext(t.Context(), rows[0].ID, "handled", now)
	requirements.NoError(err)

	live := loopOpen(t, f, ids[1], ids[2], now)
	live.TurnMessageIDs = ids[1:3]
	live.PreviousTurnKnown = true
	applyLoop(t, f, live, now)

	nudged := loopOpen(t, f, ids[1], ids[3], now.Add(time.Minute))
	nudged.TurnMessageIDs = ids[1:]
	nudged.PreviousTurnKnown = false
	applyLoop(t, f, nudged, now.Add(time.Minute))

	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal("open", rows[0].Status)
	requirements.NotNil(rows[0].AnchorMessageID)
	assertions.Equal(ids[1], *rows[0].AnchorMessageID)
}

func TestLoopApplyRetainsEvidenceForThePreservedAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(3)

	original := loopOpen(t, f, ids[0], ids[1], now)
	applyLoop(t, f, original, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)

	newerEvidence := loopOpen(t, f, ids[1], ids[2], now.Add(time.Minute))
	newerEvidence.TurnMessageIDs = ids[1:]
	newerEvidence.PreviousTurnKnown = true
	applyLoop(t, f, newerEvidence, now.Add(time.Minute))

	got, err := f.Store.GetOpenLoopContext(t.Context(), rows[0].ID)
	requirements.NoError(err)
	requirements.NotNil(got.AnchorMessageID)
	assertions.Equal(ids[0], *got.AnchorMessageID)
	assertions.Equal(ids[2], *got.LastActivityMessageID)
	assertions.Equal(original.OwedSinceAt, got.OwedSinceAt)
	assertions.Equal(original.OpensAt, got.OpensAt)
	assertions.JSONEq(string(original.EvidenceJSON), string(got.EvidenceJSON))
}

func TestLoopApplyRefreshesCorrectedTimestampForSameAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(2)
	original := loopOpen(t, f, ids[0], ids[0], now)
	applyLoop(t, f, original, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)

	corrected := original
	corrected.OwedSinceAt = now.Add(-3 * time.Hour)
	corrected.OpensAt = now.Add(21 * time.Hour)
	applyLoop(t, f, corrected, now.Add(time.Minute))

	got, err := f.Store.GetOpenLoopContext(t.Context(), rows[0].ID)
	requirements.NoError(err)
	assertions.Equal("held", got.Status)
	assertions.True(corrected.OwedSinceAt.Equal(got.OwedSinceAt))
	assertions.True(corrected.OpensAt.Equal(got.OpensAt))
	assertions.Equal(rows[0].Cycle, got.Cycle, "timestamp correction must keep the same turn cycle")
}

func TestLoopSourceOwnershipFollowsReconciliationAndMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.CreateMessage("source-ownership")
	open := loopOpen(t, f, message, message, now)
	applyLoop(t, f, open, now)

	reconciledSource, err := f.Store.GetOrCreateSource("gmail", "reconciled@example.test")
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE conversations SET source_id=? WHERE id=?", reconciledSource.ID, f.ConvID)
	loopExec(t, f.Store, "UPDATE messages SET source_id=? WHERE conversation_id=?", reconciledSource.ID, f.ConvID)
	open.SourceID = reconciledSource.ID
	applyLoop(t, f, open, now.Add(time.Minute))
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(reconciledSource.ID, rows[0].SourceID)
	historicalID := rows[0].ID
	_, err = f.Store.CloseLoopContext(t.Context(), historicalID, "handled", now.Add(2*time.Minute))
	requirements.NoError(err)

	historyMessage := f.CreateMessage("source-ownership-live")
	loopExec(t, f.Store, "UPDATE messages SET source_id=? WHERE id=?", reconciledSource.ID, historyMessage)
	live := loopOpen(t, f, historyMessage, historyMessage, now.Add(3*time.Minute))
	live.SourceID = reconciledSource.ID
	live.Kind = "awaiting_reply"
	live.TurnKey = fmt.Sprintf("turn:%d:awaiting_reply:%d", f.ConvID, historyMessage)
	applyLoop(t, f, live, now.Add(3*time.Minute))
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	liveID := rows[0].ID

	mergedSource, err := f.Store.GetOrCreateSource("gmail", "merged@example.test")
	requirements.NoError(err)
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: reconciledSource.ID,
		IntoSourceID: mergedSource.ID,
	})
	requirements.NoError(err)
	got, err := f.Store.GetOpenLoopContext(t.Context(), historicalID)
	requirements.NoError(err)
	assertions.Equal(mergedSource.ID, got.SourceID)
	got, err = f.Store.GetOpenLoopContext(t.Context(), liveID)
	requirements.NoError(err)
	assertions.Equal(mergedSource.ID, got.SourceID)
}

func TestLoopWritesRetrySQLiteSnapshotBusy(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *storetest.Fixture, time.Time) (int64, error)
		write   func(context.Context, *storetest.Fixture, int64, time.Time) error
	}{
		{
			name: "verdict",
			write: func(ctx context.Context, f *storetest.Fixture, loopID int64, now time.Time) error {
				return f.Store.ApplyLoopVerdictContext(ctx, store.LoopApply{
					ConversationID: f.ConvID,
					Now:            now,
					Verdict: store.LoopVerdict{Close: []store.LoopCloseDecision{{
						ID: loopID, Reason: "withdrawn", At: now,
					}}},
				})
			},
		},
		{
			name: "close",
			write: func(ctx context.Context, f *storetest.Fixture, loopID int64, now time.Time) error {
				_, err := f.Store.CloseLoopContext(ctx, loopID, "handled", now)
				return err
			},
		},
		{
			name: "snooze",
			write: func(ctx context.Context, f *storetest.Fixture, loopID int64, now time.Time) error {
				_, err := f.Store.SnoozeLoopContext(ctx, loopID, now.Add(time.Hour), now)
				return err
			},
		},
		{
			name: "mute",
			write: func(ctx context.Context, f *storetest.Fixture, _ int64, now time.Time) error {
				conversationID := f.ConvID
				_, err := f.Store.CreateLoopSuppressionContext(ctx, store.LoopSuppressionRequest{
					TargetKind: "conversation", ConversationID: &conversationID,
				}, now)
				return err
			},
		},
		{
			name: "unmute",
			prepare: func(t *testing.T, f *storetest.Fixture, now time.Time) (int64, error) {
				t.Helper()
				conversationID := f.ConvID
				result, err := f.Store.CreateLoopSuppressionContext(t.Context(), store.LoopSuppressionRequest{
					TargetKind: "conversation", ConversationID: &conversationID,
				}, now)
				if err != nil {
					return 0, err
				}
				require.NotNil(t, result.Suppression)
				return result.Suppression.ID, nil
			},
			write: func(ctx context.Context, f *storetest.Fixture, suppressionID int64, now time.Time) error {
				return f.Store.DeleteLoopSuppressionContext(ctx, suppressionID, 0, now)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			f := storetest.New(t)
			if f.Store.IsPostgreSQL() {
				t.Skip("SQLite snapshot-upgrade behavior")
			}
			now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			message := f.CreateMessage("busy-retry-" + test.name)
			open := loopOpen(t, f, message, message, now)
			applyLoop(t, f, open, now)
			rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			actionID := rows[0].ID
			if test.prepare != nil {
				actionID, err = test.prepare(t, f, now)
				requirements.NoError(err)
			}

			blocker, err := f.Store.DB().Conn(t.Context())
			requirements.NoError(err)
			_, err = blocker.ExecContext(t.Context(), "BEGIN IMMEDIATE")
			requirements.NoError(err)
			blockerOpen := true
			t.Cleanup(func() {
				if blockerOpen {
					_, _ = blocker.ExecContext(context.Background(), "ROLLBACK")
				}
				_ = blocker.Close()
			})

			signal := &loopBusyRetrySignalHandler{
				Handler:  slog.DiscardHandler,
				observed: make(chan struct{}),
				release:  make(chan struct{}),
			}
			previous := slog.Default()
			slog.SetDefault(slog.New(signal))
			t.Cleanup(func() {
				signal.resume()
				slog.SetDefault(previous)
			})
			done := make(chan error, 1)
			go func() { done <- test.write(t.Context(), f, actionID, now) }()
			select {
			case <-signal.observed:
			case <-time.After(30 * time.Second):
				_, _ = blocker.ExecContext(context.Background(), "ROLLBACK")
				blockerOpen = false
				signal.resume()
				requirements.FailNow("loop write did not expose a retryable SQLite snapshot failure")
			}

			_, err = blocker.ExecContext(t.Context(), "COMMIT")
			requirements.NoError(err)
			blockerOpen = false
			signal.resume()
			requirements.NoError(<-done)
		})
	}
}

type loopBusyRetrySignalHandler struct {
	slog.Handler

	observed    chan struct{}
	release     chan struct{}
	observedOne sync.Once
	releaseOne  sync.Once
}

func (h *loopBusyRetrySignalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *loopBusyRetrySignalHandler) Handle(ctx context.Context, record slog.Record) error {
	locked := record.Message == "sql tx rollback"
	if locked {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "reason" {
				reason := strings.ToLower(attr.Value.String())
				locked = strings.Contains(reason, "locked") || strings.Contains(reason, "busy")
			}
			return true
		})
	}
	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("discard observed log record: %w", err)
	}
	if locked {
		h.observedOne.Do(func() {
			close(h.observed)
			<-h.release
		})
	}
	return nil
}

func (h *loopBusyRetrySignalHandler) resume() {
	h.releaseOne.Do(func() { close(h.release) })
}

func loopViewFromLoadedConversation(t *testing.T, loaded store.LoopConversation, st *store.Store) loops.ConversationView {
	t.Helper()
	view := loops.ConversationView{
		ConversationID:   loaded.ConversationID,
		SourceID:         loaded.SourceID,
		SourceType:       loaded.SourceType,
		ConversationType: loaded.ConversationType,
		LastMessageAt:    loaded.LastMessageAt,
		BodyText: func(messageID int64) (string, error) {
			return st.LoopMessageBodyContext(t.Context(), messageID)
		},
	}
	for _, participant := range loaded.Participants {
		personID := int64(0)
		if participant.PersonID != nil {
			personID = *participant.PersonID
		}
		view.Participants = append(view.Participants, loops.LoopParticipant{
			ID: participant.ParticipantID, PersonID: personID,
			Email: participant.EmailAddress, Phone: participant.PhoneNumber,
			DisplayName: participant.DisplayName, CanonicalID: participant.CanonicalID,
			IsOwner: participant.IsOwner,
		})
	}
	for _, message := range loaded.Messages {
		mapped := loops.LoopMessage{
			ID: message.ID, OccurredAt: message.OccurredAt,
			Direction: loops.Direction(message.Direction), OwnerAddress: message.OwnerAddress,
			Sender: loopParticipantFromStore(message.Sender), ListID: message.ListID,
			Labels: message.Labels, Draft: message.Draft, Deleted: message.Deleted,
			Subject: message.Subject, Snippet: message.Snippet,
		}
		if message.Signals != nil {
			mapped.Signals = &msgmime.AutomationSignals{
				AutoSubmitted:   message.Signals.AutoSubmitted,
				PrecedenceBulk:  message.Signals.PrecedenceBulk,
				ListUnsubscribe: message.Signals.ListUnsubscribe,
				CalendarPart:    message.Signals.CalendarPart,
				ReplyToNoReply:  message.Signals.ReplyToNoReply,
			}
		}
		for _, recipient := range message.Recipients {
			mapped.Recipients = append(mapped.Recipients, loops.LoopRecipient{
				LoopParticipant: loopParticipantFromStore(recipient.LoopParticipant),
				Type:            recipient.RecipientType,
			})
		}
		view.Messages = append(view.Messages, mapped)
	}
	return view
}

func loopParticipantFromStore(participant store.LoopParticipant) loops.LoopParticipant {
	personID := int64(0)
	if participant.PersonID != nil {
		personID = *participant.PersonID
	}
	return loops.LoopParticipant{
		ID: participant.ParticipantID, PersonID: personID,
		Email: participant.EmailAddress, Phone: participant.PhoneNumber,
		DisplayName: participant.DisplayName, CanonicalID: participant.CanonicalID,
		IsOwner: participant.IsOwner,
	}
}
