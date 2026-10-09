package beeper

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelOnBeginHandler cancels synchronously at an actual transaction boundary,
// so the tests exercise the real store without timing waits or production hooks.
type cancelOnBeginHandler struct {
	slog.Handler

	mu          sync.Mutex
	cancel      context.CancelFunc
	cancelAt    int
	cancelEvent string
	events      int
	canceled    bool
	begins      int
	afterCancel int
	warnings    int
}

func (*cancelOnBeginHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelOnBeginHandler) Handle(ctx context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record.Level >= slog.LevelWarn {
		h.warnings++
	}
	if record.Message == "sql tx begin" {
		h.begins++
		if h.canceled {
			h.afterCancel++
		}
	}
	if record.Message == h.cancelEvent {
		h.events++
		if h.events == h.cancelAt {
			h.cancel()
			h.canceled = true
		}
	}
	return nil
}

func installCancelOnBegin(t *testing.T, cancel context.CancelFunc, at int) *cancelOnBeginHandler {
	t.Helper()
	handler := &cancelOnBeginHandler{
		Handler:     slog.DiscardHandler,
		cancel:      cancel,
		cancelAt:    at,
		cancelEvent: "sql tx begin",
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return handler
}

func TestEnsureConversationCancellation(t *testing.T) {
	for _, tc := range []struct {
		count        int
		afterCapture bool
	}{{count: 1}, {count: 8}, {count: 1, afterCapture: true}} {
		count := tc.count
		t.Run(fmt.Sprintf("%d members afterCapture=%t", count, tc.afterCapture), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			participants := make([]map[string]any, 0, count)
			for i := range count {
				participants = append(participants, map[string]any{
					"id":       fmt.Sprintf("@telegram_member%d:beeper.local", i),
					"fullName": fmt.Sprintf("Member %d", i),
				})
			}
			chat := observationChat("telegram", "Telegram", "@telegram_member0:beeper.local", participants...)
			f.addChat(chat)
			imp, st, closeServer := newTestImporter(t, f)
			defer closeServer()
			sourceID := newBeeperTestSource(t, st, "telegram")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			detail, err := imp.client.GetChat(ctx, chat.ID)
			require.NoError(err)
			imp.res.accountID = "telegram"
			for i := range detail.Participants.Items {
				_, err := imp.res.resolveUser(&detail.Participants.Items[i].User)
				require.NoError(err)
			}
			_, _, err = imp.obs.services.resolveBridge(ctx, "telegram", "Telegram", "telegram")
			require.NoError(err)

			// Each cached resolver still checks legacy identifier adoption in
			// a transaction. Roster replacement follows those checks, then the
			// first anchor classification begins with the import context.
			handler := installCancelOnBegin(t, cancel, count+2)
			if tc.afterCapture {
				// Cancel after the first observation's transaction commits,
				// before its result can start identity matching transactions.
				handler.cancelEvent = "sql tx commit"
				handler.cancelAt = count + 3
			}
			sum := &ImportSummary{}
			_, complete, _, err := imp.ensureConversation(ctx, 0, sourceID, detail, ImportOptions{AccountID: "telegram"}, sum)
			require.ErrorIs(ctx.Err(), context.Canceled, "the classification boundary must be reached")
			require.ErrorIs(err, context.Canceled)
			assert.False(complete)
			assert.Zero(handler.afterCancel, "no remaining member may attempt BeginTx")
			assert.Zero(handler.warnings, "expected cancellation must not log warnings")
			assert.Zero(sum.Errors)

			// A fresh import rebuilds run-local caches and retries enrichment
			// skipped by the canceled visit, using the same archived membership.
			slog.SetDefault(slog.New(slog.DiscardHandler))
			_, err = NewImporter(st, imp.client).Import(context.Background(), ImportOptions{AccountID: "telegram", NoMedia: true})
			require.NoError(err)
			for _, participant := range detail.Participants.Items {
				pid := participantForBeeperUser(t, st, participant.ID)
				observations, err := st.ListParticipantObservationsContext(context.Background(), pid, true)
				require.NoError(err)
				assert.NotEmpty(observations, "a resumed import must enrich every member")
			}
		})
	}
}

func TestEnsureConversationCancellationBeforeMembership(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(fmt.Sprintf("begin %d", at), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			sourceID := newBeeperTestSource(t, st, "telegram")
			imp := NewImporter(st, nil)
			imp.res.accountID = "telegram"
			oldID, err := imp.res.resolveUser(&User{ID: "@telegram_old:beeper.local"})
			require.NoError(err)
			newUser := User{ID: "@telegram_new:beeper.local"}
			_, err = imp.res.resolveUser(&newUser)
			require.NoError(err)
			convID, err := st.EnsureConversationWithType(sourceID, "!cancel:example.com", "group", "Members")
			require.NoError(err)
			require.NoError(st.EnsureConversationParticipant(convID, oldID, "member"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Begin 1 is the final resolver adoption check; begin 2 is roster replacement.
			handler := installCancelOnBegin(t, cancel, at)
			_, complete, _, err := imp.ensureConversation(ctx, 0, sourceID, &Chat{
				ID: "!cancel:example.com", Type: "group", Title: "Members", Network: "Telegram",
				Participants: ChatParticipants{Items: []Participant{{User: newUser}}},
			}, ImportOptions{AccountID: "telegram"}, &ImportSummary{})
			require.ErrorIs(err, context.Canceled)
			assert.False(complete)
			assert.Zero(handler.afterCancel)
			assert.Zero(handler.warnings)
			var memberID int64
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT participant_id FROM conversation_participants WHERE conversation_id = ?"), convID).Scan(&memberID))
			assert.Equal(oldID, memberID, "canceled replacement must preserve the existing roster")
		})
	}
}

func TestEnsureConversationCancellationAtEntry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	sourceID := newBeeperTestSource(t, st, "telegram")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, complete, _, err := NewImporter(st, nil).ensureConversation(ctx, 0, sourceID, &Chat{
		ID: "!cancel:example.com", Type: "group", Title: "Members",
	}, ImportOptions{AccountID: "telegram"}, &ImportSummary{})
	require.ErrorIs(err, context.Canceled)
	assert.False(complete)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM conversations WHERE source_id = ?"), sourceID).Scan(&count))
	assert.Zero(count)
}

func TestProcessMessageCancellation(t *testing.T) {
	for _, event := range []string{"normal", "deleted", "hidden", "reaction"} {
		t.Run(event, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			sourceID := newBeeperTestSource(t, st, "telegram")
			convID, err := st.EnsureConversationWithType(sourceID, "!events:example.com", "group", "Events")
			require.NoError(err)
			_, err = st.UpsertMessage(&store.Message{
				SourceID: sourceID, ConversationID: convID, SourceMessageID: "message-1",
				MessageType: "beeper", Snippet: sql.NullString{String: "original", Valid: true},
			})
			require.NoError(err)
			message := &Message{ID: "message-1", Text: "changed", Type: "TEXT"}
			switch event {
			case "deleted":
				message.IsDeleted = true
			case "hidden":
				message.IsHidden = true
			case "reaction":
				message.Type = "REACTION"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			sum := &ImportSummary{}
			err = NewImporter(st, nil).processMessage(ctx, &chatScope{
				sourceID: sourceID, convID: convID, cs: &ChatState{}, membershipComplete: true,
			}, message, false, sum)
			require.ErrorIs(err, context.Canceled)
			assert.Zero(sum.MessagesProcessed)
			var snippet string
			var deletedFromSourceAt sql.NullTime
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet, deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?"), sourceID, "message-1").Scan(&snippet, &deletedFromSourceAt))
			assert.Equal("original", snippet)
			assert.False(deletedFromSourceAt.Valid)
		})
	}
}
