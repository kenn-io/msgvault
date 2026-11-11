package beeper

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelOnLogHandler cancels synchronously when the store logs a chosen event,
// so the tests exercise the real store without timing waits or production hooks.
type cancelOnLogHandler struct {
	slog.Handler

	mu          sync.Mutex
	cancel      context.CancelFunc
	cancelWhen  func(slog.Record) bool
	cancelAt    int
	events      int
	canceled    bool
	afterCancel int
	warnings    []string
}

func (*cancelOnLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelOnLogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record.Level >= slog.LevelWarn {
		h.warnings = append(h.warnings, record.Message)
	}
	if record.Message == "sql tx begin" && h.canceled {
		h.afterCancel++
	}
	if h.cancelWhen(record) {
		h.events++
		if h.events == h.cancelAt {
			h.cancel()
			h.canceled = true
		}
	}
	return nil
}

// installCancelOnLog cancels on the at-th log record matching when. An at of
// zero never cancels and only counts warnings.
//
// Slow-query logging stays off while the handler is installed. Otherwise a
// statement or commit that crosses the threshold on a loaded runner logs
// "sql slow" or "sql tx slow" in place of the line a trigger counts, and a
// very slow one logs a warning.
func installCancelOnLog(
	t *testing.T, cancel context.CancelFunc, at int, when func(slog.Record) bool,
) *cancelOnLogHandler {
	t.Helper()
	handler := &cancelOnLogHandler{
		Handler:    slog.DiscardHandler,
		cancel:     cancel,
		cancelWhen: when,
		cancelAt:   at,
	}
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return handler
}

func logMessage(msg string) func(slog.Record) bool {
	return func(record slog.Record) bool { return record.Message == msg }
}

func rosterMembers(t *testing.T, st *store.Store, sourceID int64, chatID string) []int64 {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT participant_id FROM conversation_participants
		WHERE conversation_id = (
			SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?
		)
		ORDER BY participant_id`), sourceID, chatID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var members []int64
	for rows.Next() {
		var participantID int64
		require.NoError(t, rows.Scan(&participantID))
		members = append(members, participantID)
	}
	require.NoError(t, rows.Err())
	return members
}

// serial: changes slog.Default and SQL logging.
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
			chat := observationChat(
				"telegram", "Telegram", "@telegram_member0:beeper.local", participants...,
			)
			f.addChat(chat)
			imp, st, closeServer := newTestImporter(t, f)
			defer closeServer()
			sourceID := newBeeperTestSource(t, st, "telegram")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			detail, err := imp.client.GetChat(ctx, chat.ID)
			require.NoError(err)
			imp.res.accountID = "telegram"
			for i := range detail.Participants.Items {
				_, err := imp.res.resolveUser(t.Context(), &detail.Participants.Items[i].User)
				require.NoError(err)
			}
			_, _, err = imp.obs.services.resolveBridge(ctx, "telegram", "Telegram", "telegram")
			require.NoError(err)

			work := &rosterWork{}
			when, at := logMessage("sql tx begin"), 1
			wantObserved := 0
			if tc.afterCapture {
				when, at = logMessage("sql tx commit"), 2
				wantObserved = 1
			}
			handler := installCancelOnLog(t, cancel, at, func(record slog.Record) bool {
				return work.Phase == "matching" && when(record)
			})
			sum := &ImportSummary{}
			_, complete, _, err := imp.ensureRosterWork(
				ctx, 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, sum,
			)
			require.ErrorIs(ctx.Err(), context.Canceled, "the classification boundary must be reached")
			require.ErrorIs(err, context.Canceled)
			assert.True(complete, "matching cancellation preserves committed membership")
			assert.Zero(handler.afterCancel, "no remaining member may attempt BeginTx")
			assert.Empty(handler.warnings, "expected cancellation must not log warnings")
			assert.Zero(sum.Errors)

			// Pin where cancellation landed. A change in the number of
			// transactions before it must fail here rather than quietly move
			// the test to an earlier boundary.
			assert.Len(rosterMembers(t, st, sourceID, chat.ID), count,
				"the full roster must be committed before matching")
			observed := 0
			for _, participant := range detail.Participants.Items {
				pid := participantForBeeperUser(t, st, participant.ID)
				observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
				require.NoError(err)
				if len(observations) > 0 {
					observed++
				}
			}
			assert.Equal(wantObserved, observed, "members with observations before cancellation")

			// A fresh import rebuilds run-local caches and retries enrichment
			// skipped by the canceled visit, using the same archived membership.
			slog.SetDefault(slog.New(slog.DiscardHandler))
			_, err = NewImporter(st, imp.client).Import(
				t.Context(), ImportOptions{AccountID: "telegram", NoMedia: true},
			)
			require.NoError(err)
			for _, participant := range detail.Participants.Items {
				pid := participantForBeeperUser(t, st, participant.ID)
				observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
				require.NoError(err)
				assert.NotEmpty(observations, "a resumed import must enrich every member")
			}
		})
	}
}

// serial: changes slog.Default and SQL logging.
func TestEnsureConversationCancellationDuringMembership(t *testing.T) {
	// Each member's resolution and membership commit are separate transactions.
	for _, at := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("begin %d", at), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			sourceID := newBeeperTestSource(t, st, "telegram")
			imp := NewImporter(st, nil)
			imp.res.accountID = "telegram"
			oldID, err := imp.res.resolveUser(t.Context(), &User{ID: "@telegram_old:beeper.local"})
			require.NoError(err)
			newUsers := []User{{ID: "@telegram_new1:beeper.local"}, {ID: "@telegram_new2:beeper.local"}}
			participants := make([]Participant, 0, len(newUsers))
			var firstID int64
			for i := range newUsers {
				id, err := imp.res.resolveUser(t.Context(), &newUsers[i])
				require.NoError(err)
				if i == 0 {
					firstID = id
				}
				participants = append(participants, Participant{User: newUsers[i]})
			}
			convID, err := st.EnsureConversationWithType(sourceID, "!cancel:example.com", "group", "Members")
			require.NoError(err)
			require.NoError(st.EnsureConversationParticipant(convID, oldID, "member"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			handler := installCancelOnLog(t, cancel, at, logMessage("sql tx begin"))
			_, complete, _, err := imp.ensureRosterWork(ctx, 0, sourceID, &Chat{
				ID: "!cancel:example.com", Type: "group", Title: "Members", Network: "Telegram",
				Participants: ChatParticipants{Items: participants},
			}, ImportOptions{AccountID: "telegram"}, &ImportSummary{})
			require.ErrorIs(err, context.Canceled)
			assert.False(complete)
			assert.Zero(handler.afterCancel, "no later member or roster write may attempt BeginTx")
			assert.Empty(handler.warnings)
			want := []int64{oldID}
			if at >= 5 {
				want = append(want, firstID)
			}
			assert.Equal(want, rosterMembers(t, st, sourceID, "!cancel:example.com"))
		})
	}
}

// A chat whose full roster cannot be fetched keeps its archived members and
// adds the listed ones one write at a time. Cancellation between those writes
// must stop the remaining members without counting or logging failures.
// serial: changes slog.Default and SQL logging.
func TestEnsureConversationCancellationDuringPartialRoster(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	const chatID = "!partial:example.com"
	f := newFakeBeeper(t)
	f.setChatGetFailure(chatID, true)
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	sourceID := newBeeperTestSource(t, st, "telegram")
	syncID, err := st.StartSync(sourceID, sourceTypeBeeper)
	require.NoError(err)
	participants := make([]Participant, 0, 3)
	for i := range 3 {
		user := User{ID: fmt.Sprintf("@telegram_listed%d:beeper.local", i)}
		participants = append(participants, Participant{User: user})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := installCancelOnLog(t, cancel, 4, logMessage("sql tx commit"))
	sum := &ImportSummary{}
	_, complete, _, err := imp.ensureRosterWork(ctx, syncID, sourceID, &Chat{
		ID: chatID, Type: "group", Title: "Partial", Network: "Telegram",
		Participants: ChatParticipants{Items: participants, HasMore: true},
	}, ImportOptions{AccountID: "telegram"}, sum)
	require.ErrorIs(err, context.Canceled)
	assert.False(complete)
	assert.Empty(handler.warnings, "no member write may run with the canceled context")
	assert.Equal(int64(1), sum.FetchErrors)
	assert.Equal(sum.FetchErrors, sum.Errors, "cancellation must not count as a member write error")
	first := participantForBeeperUser(t, st, participants[0].ID)
	assert.Equal([]int64{first}, rosterMembers(t, st, sourceID, chatID),
		"only the member written before cancellation joins the roster")
}

// serial: changes slog.Default and SQL logging.
func TestEnsureConversationCancellationAtEntry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	sourceID := newBeeperTestSource(t, st, "telegram")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	handler := installCancelOnLog(t, cancel, 0, logMessage(""))
	_, complete, _, err := imp.ensureRosterWork(ctx, 0, sourceID, &Chat{
		ID: "!cancel:example.com", Type: "group", Title: "Members",
		Participants: ChatParticipants{HasMore: true},
	}, ImportOptions{AccountID: "telegram"}, &ImportSummary{})
	require.ErrorIs(err, context.Canceled)
	assert.False(complete)
	assert.Empty(f.requests(), "an already-canceled import must not fetch the roster")
	assert.Empty(handler.warnings, "an already-canceled import must not attempt archive writes")
	var count int
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
		st.Rebind("SELECT COUNT(*) FROM conversations WHERE source_id = ?"), sourceID,
	).Scan(&count))
	assert.Zero(count)
}

func TestProcessMessageCancellation(t *testing.T) {
	t.Parallel()
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
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			sum := &ImportSummary{}
			err = NewImporter(st, nil).processMessage(ctx, &chatScope{
				sourceID: sourceID, convID: convID, cs: &ChatState{}, membershipComplete: true,
			}, message, false, sum)
			require.ErrorIs(err, context.Canceled)
			assert.Zero(sum.MessagesProcessed)
			var snippet string
			var deletedFromSourceAt sql.NullTime
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT snippet, deleted_from_source_at FROM messages
				WHERE source_id = ? AND source_message_id = ?`), sourceID, "message-1",
			).Scan(&snippet, &deletedFromSourceAt))
			assert.Equal("original", snippet)
			assert.False(deletedFromSourceAt.Valid)
		})
	}
}

// A stop that cancels a reply-link write must leave the chat's pending pairs
// in its state. The stopped run still saves that state, so clearing the pairs
// there would lose the links for good.
func TestFlushRepliesCancellationKeepsPendingReplies(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!flush:beeper.local", AccountID: "signal", Network: "Signal", Title: "Flush", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i := range 3 {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: fmt.Sprintf("f%d", i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      fmt.Sprintf("flush %d", i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	ch.Msgs[2].LinkedMessageID = "f0"
	ch.LastActivity = ch.Msgs[2].Timestamp
	f.addChat(ch)

	imp, st, done := newTestImporter(t, f)
	defer done()

	// The first page holds every message and the empty second page ends the
	// backfill, so the next transaction after that page is the reply-link
	// write. A stop request cancels the run as that transaction begins.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var walked atomic.Bool
	f.cancelAfterPages = 2
	f.cancelFn = func() { walked.Store(true) }
	installCancelOnLog(t, cancel, 1, func(record slog.Record) bool {
		return record.Message == "sql tx begin" && walked.Load()
	})

	sum, err := imp.Import(ctx, ImportOptions{AccountID: "signal", ShouldStop: walked.Load})
	require.NoError(err)
	require.True(sum.Stopped)

	cs := latestBeeperState(t, imp).Chats[ch.ID]
	require.NotNil(cs)
	require.True(cs.Done, "the backfill must finish before cancellation lands")
	require.Equal([][2]string{{"f2", "f0"}}, cs.PendingReplies)

	// The next run flushes the kept pair.
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var parentID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "f0").Scan(&parentID))
	var linked sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE source_message_id = ?`), "f2").Scan(&linked))
	require.Equal(sql.NullInt64{Int64: parentID, Valid: true}, linked)
}

// A stop that cancels the tombstone write of a backfill page's last item must
// leave the chat's resume cursor on that page. The failed write is only
// counted as an item error, so a checkpoint past the page would leave the
// resumed backfill nothing to revisit.
func TestBackfillAuxiliaryWriteCancellationRetriesPage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!tombstone:beeper.local", AccountID: "signal", Network: "Signal", Title: "Tombstone", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
		Msgs: []fakeMsg{{
			ID: "d0", Timestamp: base, IsDeleted: true,
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		}},
		LastActivity: base,
	}
	f.addChat(ch)

	imp, st, done := newTestImporter(t, f)
	defer done()

	// An earlier archive holds the message Beeper now reports deleted.
	sourceID := newBeeperTestSource(t, st, "signal")
	convID, err := st.EnsureConversationWithType(sourceID, ch.ID, "single", ch.Title)
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: convID, SourceMessageID: "d0", MessageType: "beeper",
	})
	require.NoError(err)

	// The page's only item is the tombstone, so the first transaction after
	// the page is served is its write. A stop request cancels the run as that
	// transaction begins.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var walked atomic.Bool
	f.cancelAfterPages = 1
	f.cancelFn = func() { walked.Store(true) }
	handler := installCancelOnLog(t, cancel, 1, func(record slog.Record) bool {
		return record.Message == "sql tx begin" && walked.Load()
	})

	_, firstErr := imp.Import(ctx, ImportOptions{AccountID: "signal", ShouldStop: walked.Load})
	require.True(handler.canceled, "the stop must cancel the tombstone write")

	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var deletedFromSourceAt sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT deleted_from_source_at FROM messages
		WHERE source_id = ? AND source_message_id = ?`), sourceID, "d0",
	).Scan(&deletedFromSourceAt))
	assert.True(deletedFromSourceAt.Valid, "the resumed backfill must apply the tombstone the stopped run missed")
	assert.ErrorIs(firstErr, context.Canceled)
}

// The incremental walk has the same gap: a stop that cancels the tombstone
// write of a head page's last item must leave the chat's forward cursor before
// that page, so the next run re-reads it.
func TestIncrementalAuxiliaryWriteCancellationRetriesPage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!headtombstone:beeper.local", AccountID: "signal", Network: "Signal", Title: "Head", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
		Msgs: []fakeMsg{{
			ID: "h0", Timestamp: base, Text: "soon deleted",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		}},
		LastActivity: base,
	}
	f.addChat(ch)

	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	require.True(latestBeeperState(t, imp).Chats[ch.ID].Done)

	// Beeper now reports the archived message deleted at the head of the
	// chat, outside the reconciliation window.
	f.mu.Lock()
	ch.Msgs = append(ch.Msgs, fakeMsg{
		ID: "h0", SortKey: 1, Timestamp: base, IsDeleted: true,
		SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})
	ch.LastActivity = time.Now().UTC()
	f.mu.Unlock()

	// The head page's only item is the tombstone, so the first transaction
	// after the page is served is its write. A stop request cancels the run
	// as that transaction begins.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var walked atomic.Bool
	f.cancelMessageListFor(ch.ID, func() { walked.Store(true) })
	handler := installCancelOnLog(t, cancel, 1, func(record slog.Record) bool {
		return record.Message == "sql tx begin" && walked.Load()
	})

	_, firstErr := imp.Import(ctx, ImportOptions{AccountID: "signal", ShouldStop: walked.Load})
	require.True(handler.canceled, "the stop must cancel the tombstone write")

	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var deletedFromSourceAt sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE source_message_id = ?`), "h0",
	).Scan(&deletedFromSourceAt))
	assert.True(deletedFromSourceAt.Valid, "the resumed walk must apply the tombstone the stopped run missed")
	assert.ErrorIs(firstErr, context.Canceled)
}

// Clearing a message's pending markers commits the row replacement before it
// refreshes the message's attachment flags. If a cancel landing between the
// two also stopped the refresh, has_attachments and attachment_count would
// keep counting the deleted markers, and no later run revisits the message.
// serial: changes slog.Default and SQL logging.
func TestClearPendingMarkersRefreshesFlagsAfterCancel(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	sourceID := newBeeperTestSource(t, st, "signal")
	convID, err := st.EnsureConversationWithType(sourceID, "!markers:example.com", "single", "Markers")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: convID, SourceMessageID: "marker-1", MessageType: "beeper",
	})
	require.NoError(err)
	const assetURL = "mxc://example.com/photo"
	require.NoError(st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{{
		Filename: "photo.jpg", MimeType: "image/jpeg", StoragePath: assetURL,
		SourceAttachmentID: beeperAttachmentID(assetURL), State: attachmentpolicy.StatePending,
	}}))
	require.NoError(st.RecomputeMessageAttachmentStatsContext(t.Context(), messageID))
	attachmentFlags := func() (bool, int) {
		var hasAttachments bool
		var attachmentCount int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`), messageID,
		).Scan(&hasAttachments, &attachmentCount))
		return hasAttachments, attachmentCount
	}
	hasAttachments, attachmentCount := attachmentFlags()
	require.True(hasAttachments, "the pending marker counts before it is cleared")
	require.Equal(1, attachmentCount)

	// The marker replacement is the only transaction clearPendingMarkers
	// commits, so the cancel lands after it and before the refresh.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := installCancelOnLog(t, cancel, 1, logMessage("sql tx commit"))
	err = NewImporter(st, nil).clearPendingMarkers(ctx, messageID)
	require.True(handler.canceled, "the cancel must land after the marker replacement commits")
	assert.NoError(err)

	var rows int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&rows))
	assert.Zero(rows, "the pending marker is removed")
	hasAttachments, attachmentCount = attachmentFlags()
	assert.False(hasAttachments, "has_attachments must match the remaining rows")
	assert.Zero(attachmentCount, "attachment_count must match the remaining rows")
}
