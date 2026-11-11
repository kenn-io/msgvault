package beeper

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMergeSourcesRepairsLiveRoster(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		name := "active"
		if quiet {
			name = "quiet"
		}
		t.Run(name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!merged:example.test", 1, time.Now().Add(-48*time.Hour))
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			provider := f.handler()
			imp.client.http.Transport = handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/accounts/signal" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"accountID":"signal","bridge":{"type":"signal"},"status":"connected"}`))
					return
				}
				provider(w, r)
			}))
			opts := ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			want := rosterMembers(t, st, sum.SourceID, chat.ID)
			require.Len(want, 2)
			peer := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
			person, _, err := st.CreatePersonFromParticipant(peer)
			require.NoError(err)
			query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
			routes, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			require.NoError(err)
			require.Len(routes.Routes.Items, 1)
			require.Equal("archive_verified", routes.Routes.Items[0].Status)
			history, err := st.GetOrCreateSource("beeper", "signal-history")
			require.NoError(err)
			conv, err := st.EnsureConversation(t.Context(), history.ID, chat.ID, "History")
			require.NoError(err)
			departed, err := st.EnsureParticipantByPhone(t.Context(), "+12025550199", "Departed", "signal")
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO conversation_participants(conversation_id, participant_id, role) VALUES (?, ?, 'member')`), conv, departed)
			require.NoError(err)
			historicalID, err := st.UpsertMessage(&store.Message{SourceID: history.ID, ConversationID: conv, SourceMessageID: "historical", MessageType: "beeper"})
			require.NoError(err)
			req := store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: sum.SourceID, DryRun: true}
			_, err = st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			req.DryRun = false
			_, err = st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			assert.Contains(rosterMembers(t, st, sum.SourceID, chat.ID), departed)
			shared, err := st.SharedConversationCountContext(t.Context(), peer, departed)
			require.NoError(err)
			assert.Zero(shared)
			if !quiet {
				f.appendMsg(chat.ID, fakeMsg{ID: "active", SortKey: 99, Timestamp: time.Now(), SenderID: "@signal_ann:beeper.local", Text: "active"})
			}
			imp = NewImporter(st, imp.client)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.ElementsMatch(want, rosterMembers(t, st, sum.SourceID, chat.ID))
			pending, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
			require.NoError(err)
			assert.NotContains(pending, chat.ID)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND id = ?`), sum.SourceID, historicalID).Scan(&count))
			assert.Positive(count)
			routes, err = st.GetPersonMessagingRoutesContext(t.Context(), query)
			require.NoError(err)
			require.Len(routes.Routes.Items, 1)
			assert.Equal("archive_verified", routes.Routes.Items[0].Status)
		})
	}
}

func TestMergeSourcesRestartsBeeperRepairForMovedHistory(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"completed destination", "partial destination", "fresh destination"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			history, err := st.GetOrCreateSource("beeper", "signal-history")
			require.NoError(err)
			live, err := st.GetOrCreateSource("beeper", "signal-live")
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncConfig(t.Context(), live.ID, `{"no_default_identity":true}`))
			const providerProgress = `{"chats":{"!live:example.test":{"newest":"provider-newest","oldest":"provider-oldest","done":true}}}`
			runID, err := st.StartSync(live.ID, "incremental")
			require.NoError(err)
			require.NoError(st.CompleteSync(runID, providerProgress))
			runID, err = st.StartSync(live.ID, "incremental")
			require.NoError(err)
			require.NoError(st.PauseSyncWithCheckpoint(t.Context(), runID, &store.Checkpoint{
				PageToken: `{"chats":{"!live:example.test":{"newest":"provider-checkpoint","done":true}}}`,
			}))
			other, err := st.GetOrCreateSource("beeper", live.Identifier+":other")
			require.NoError(err)
			_, ran, err := rederive.RunIfStale(t.Context(), st, "beeper", other.Identifier, other.ID, nil)
			require.NoError(err)
			require.True(ran)

			historyConv, err := st.EnsureConversation(t.Context(), history.ID, "!history:example.test", "History")
			require.NoError(err)
			liveConv, err := st.EnsureConversation(t.Context(), live.ID, "!live:example.test", "Live")
			require.NoError(err)
			const historicalHTML = `<p>visit <a href="https://example.test/archive">link</a></p>`
			timestamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			// Older Beeper repair versions retained plain text without HTML link targets.
			historicalID, err := st.UpsertMessage(&store.Message{SourceID: history.ID, ConversationID: historyConv, SourceMessageID: "history-message", MessageType: "beeper", SentAt: sql.NullTime{Time: timestamp, Valid: true}})
			require.NoError(err)
			raw, err := json.Marshal(Message{ID: "history-message", Text: historicalHTML, SenderName: "Example Sender", Timestamp: timestamp})
			require.NoError(err)
			require.NoError(st.UpsertMessageRawWithFormat(historicalID, raw, rawArchiveFormat))
			require.NoError(st.UpsertMessageBody(historicalID, sql.NullString{String: "visit link", Valid: true}, sql.NullString{}))
			switch mode {
			case "completed destination":
				sum, ran, err := rederive.RunIfStale(t.Context(), st, "beeper", live.Identifier, live.ID, nil)
				require.NoError(err)
				require.True(ran)
				require.Zero(sum.MessagesScanned)
			case "partial destination":
				seedArchivedBeeperMessages(t, st, live.ID, liveConv, repairBatchSize+1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				sum, ran, err := rederive.RunIfStale(ctx, st, "beeper", live.Identifier, live.ID, func(string) { cancel() })
				require.ErrorIs(err, context.Canceled)
				require.True(ran)
				require.EqualValues(repairBatchSize, sum.MessagesScanned)
				before, err := st.GetSourceByID(live.ID)
				require.NoError(err)
				var state struct {
					Version string `json:"repair_version"`
					After   int64  `json:"repair_after_id"`
				}
				require.NoError(json.Unmarshal([]byte(before.SyncConfig.String), &state))
				require.Equal(rederiveVersion, state.Version)
				require.Greater(state.After, historicalID)
			}
			before, err := st.GetSourceByID(live.ID)
			require.NoError(err)
			ledgerKey := rederive.LedgerKey("beeper", live.Identifier, rederiveVersion)
			appliedBefore, err := st.IsMigrationAppliedContext(t.Context(), ledgerKey, 1)
			require.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: history.ID, IntoSourceID: live.ID, DryRun: true,
			})
			require.NoError(err)
			afterPreview, err := st.GetSourceByID(live.ID)
			require.NoError(err)
			assert.Equal(before.SyncConfig, afterPreview.SyncConfig, "dry run preserves repair checkpoints")
			applied, err := st.IsMigrationAppliedContext(t.Context(), ledgerKey, 1)
			require.NoError(err)
			assert.Equal(appliedBefore, applied, "dry run preserves completed repairs")

			merged, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			require.Equal(int64(1), merged.MessagesMoved)
			require.Zero(merged.DuplicatesHidden)
			afterMerge, err := st.GetSourceByID(live.ID)
			require.NoError(err)
			assert.JSONEq(`{"no_default_identity":true}`, afterMerge.SyncConfig.String)
			applied, err = st.IsMigrationAppliedContext(t.Context(), ledgerKey, 1)
			require.NoError(err)
			assert.False(applied, "committed merge invalidates completed repairs")
			progress, err := NewImporter(st, nil).loadResumeState(t.Context(), live.ID, false)
			require.NoError(err)
			require.Contains(progress.Chats, "!live:example.test")
			assert.Equal("provider-checkpoint", progress.Chats["!live:example.test"].Newest)
			assert.Equal("provider-oldest", progress.Chats["!live:example.test"].Oldest)
			assert.True(progress.Chats["!live:example.test"].Done)
			_, ran, err = rederive.RunIfStale(t.Context(), st, "beeper", other.Identifier, other.ID, nil)
			require.NoError(err)
			assert.False(ran, "another source's completed repair remains current")

			sum, ran, err := rederive.RunIfStale(t.Context(), st, "beeper", live.Identifier, live.ID, nil)
			require.NoError(err)
			require.True(ran, "moved history must receive the current repair")
			wantScanned := int64(1)
			if mode == "partial destination" {
				wantScanned = repairBatchSize + 2
			}
			assert.Equal(wantScanned, sum.MessagesScanned)
			var html sql.NullString
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_html FROM message_bodies WHERE message_id = ?`), historicalID).Scan(&html))
			assert.Equal(historicalHTML, html.String, "automatic repair restores moved history's link targets")
			full, err := rederive.Run(t.Context(), st, "beeper", live.Identifier, live.ID, nil)
			require.NoError(err)
			assert.Equal(wantScanned, full.MessagesScanned, "automatic repair visits the same history as an explicit full pass")
			merged, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			assert.True(merged.AlreadyMerged)
			_, ran, err = rederive.RunIfStale(t.Context(), st, "beeper", live.Identifier, live.ID, nil)
			require.NoError(err)
			assert.False(ran, "retrying a committed merge does not repeat completed repair")
		})
	}
}
