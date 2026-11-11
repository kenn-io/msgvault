package beeper

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Each INSERT takes less than StopAt's window. The total full-roster snapshot
// exceeds the window; unchanged conflict/no-op rows have no INSERT delay.
func TestRosterAggregateInsertCostProgresses(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses SQLite to inject per-new-member insert cost")
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := newFakeBeeper(t)
		chat := rosterRetryChat()
		f.addChat(chat)
		f.addChat(budgetTestChat("!later-budget:beeper.local", 2, time.Now().Add(-72*time.Hour)))
		imp, st := newHandlerTestImporter(t, f)
		t.Cleanup(func() { require.NoError(st.Close()) })
		st.DB().SetMaxOpenConns(1)
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER review_new_member_cost
            AFTER INSERT ON conversation_participants
            BEGIN SELECT review_new_member_cost(); END`)
		require.NoError(err)
		totalInsertAttempts := 0
		for range 3 {
			registerSQLiteFunc(t, st, "review_new_member_cost", func() int {
				totalInsertAttempts++
				time.Sleep(20 * time.Millisecond)
				return 0
			})
			sum, err := imp.Import(t.Context(), ImportOptions{
				AccountID: "signal", Scheduled: true, NoMedia: true, StopAt: time.Now().Add(time.Second),
			})
			require.NoError(err)
			assert.Zero(sum.Errors)
			run, err := st.GetLastSuccessfulSync(sum.SourceID)
			require.NoError(err)
			assert.Equal("completed", run.Status)
		}
		require.Positive(totalInsertAttempts, "the injected INSERT cost was exercised")
		assert.Equal(4, countBeeperMessages(t, imp),
			"aggregate roster cost must not prevent progress across budgeted runs")
	})
}

func TestRosterRouteProofStopLeavesChatResumable(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses SQLite triggers to yield during roster work")
	chatID := "!review-count:beeper.local"
	trigger := `AFTER UPDATE OF metadata ON conversations WHEN json_extract(NEW.metadata, '$.messaging_route.membership_complete') = 1`
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat(chatID, 2, time.Now().Add(-48*time.Hour))
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

	st.DB().SetMaxOpenConns(1)
	yield := false
	registerSQLiteFunc(t, st, "yield_roster_work", func() int {
		yield = true
		return 0
	})
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER yield_roster_work `+trigger+`
            BEGIN SELECT yield_roster_work(); END`)
	require.NoError(err)
	sum, err := imp.Import(t.Context(), ImportOptions{
		AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return yield },
	})
	require.NoError(err)
	require.True(yield, "the selected roster stop point must be reached")
	require.True(sum.Stopped)
	assert.Zero(sum.Errors)

	require.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 2)
	assert.Zero(countBeeperMessages(t, imp), "stop before message work must leave it resumable")
	run, err := st.GetLastSuccessfulSync(sum.SourceID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT participant_count FROM conversations WHERE source_conversation_id=?`, chat.ID).Scan(&count))
	assert.Equal(2, count, "a clean yield must expose the committed roster count")
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID, "a stop after route proof leaves the committed roster resolved")

	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER yield_roster_work`)
	require.NoError(err)

	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)

	state := latestBeeperState(t, imp)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	yield = false
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER yield_roster_work `+trigger+` BEGIN SELECT yield_roster_work(); END`)
	require.NoError(err)
	_, _, err = imp.syncChat(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", NoMedia: true, ShouldStop: func() bool { return yield }}, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	require.True(yield)
	unresolved, err = st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID)
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER yield_roster_work`)
	require.NoError(err)
	_, complete, err := imp.syncChat(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", NoMedia: true}, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	unresolved, err = st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID)

	assert.Equal(2, countBeeperMessages(t, imp))
}

func TestRosterMatchingStopLeavesChatResumable(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses SQLite triggers to yield during roster work")
	chatID := "!observe-stop:beeper.local"
	trigger := `AFTER UPDATE OF service_id ON participant_identifiers`
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat(chatID, 2, time.Now().Add(-48*time.Hour))
	chat.ParticipantsTruncated = true
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

	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	f.mu.Lock()
	chat.Participants[0]["username"] = "newname"
	chat.LastActivity = time.Now().Add(time.Hour)
	f.mu.Unlock()

	st.DB().SetMaxOpenConns(1)
	yield := false
	registerSQLiteFunc(t, st, "yield_roster_work", func() int {
		yield = true
		return 0
	})
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER yield_roster_work `+trigger+`
            BEGIN SELECT yield_roster_work(); END`)
	require.NoError(err)
	sum, err := imp.Import(t.Context(), ImportOptions{
		AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return yield },
	})
	require.NoError(err)
	require.True(yield, "the selected roster stop point must be reached")
	require.True(sum.Stopped)
	assert.Zero(sum.Errors)

	assert.Equal(2, countBeeperMessages(t, imp), "earlier archived messages remain available")
	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID])
	assert.True(state.Chats[chat.ID].Done)
	assert.False(state.Chats[chat.ID].Visited, "matching stopped before this visit finished")
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID, "completed membership stays resolved while enrichment waits")
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_id = ? LIMIT 1`, sum.SourceID).Scan(&messageID))
	membership, err := st.AttachmentConversationMembershipContext(t.Context(), messageID)
	require.NoError(err)
	assert.True(membership.RosterArchived)
	peer := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	routes, err := st.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	require.NoError(err)
	require.Len(routes.Routes.Items, 1)
	assert.Equal("archive_verified", routes.Routes.Items[0].Status)
	require.NoError(st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{SourceAttachmentID: "beeper:mxc://example.test/pending", MediaType: "image", State: attachmentpolicy.StatePending}))
	pending, err := st.ListBeeperRetryableAttachmentMessages(t.Context(), sum.SourceID, attachmentpolicy.Policy{MaxParticipants: 2})
	require.NoError(err)
	assert.Len(pending, 1, "matching stops preserve media eligibility")
	require.NotNil(state.Chats[chat.ID].RosterWork)
	assert.Contains([]string{"matching", "matching_retry"}, state.Chats[chat.ID].RosterWork.Phase)
	f.mu.Lock()
	f.reqs = nil
	chat.Msgs = append(chat.Msgs, fakeMsg{ID: "resume", SortKey: 99, Timestamp: time.Now(), Text: "new", SenderID: "@signal_ann:beeper.local"})
	chat.LastActivity = time.Now()
	f.mu.Unlock()
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true})
	require.NoError(err)
	assert.Equal(3, countBeeperMessages(t, imp))
	routes, err = st.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	require.NoError(err)
	require.Len(routes.Routes.Items, 1)
	assert.Equal("archive_verified", routes.Routes.Items[0].Status)
	f.mu.Lock()
	for _, req := range f.reqs {
		assert.False(strings.HasPrefix(req, "/v1/chats/"+chat.ID+"?"), "message turn must not fetch detail")
	}
	f.mu.Unlock()

	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER yield_roster_work`)
	require.NoError(err)

	for range 2 {
		_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
		require.NoError(err)
	}

	unresolved, err = st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID)
	state = latestBeeperState(t, imp)
	assert.Equal("done", state.Chats[chat.ID].RosterWork.Phase, "quiet repair must finish deferred enrichment")

	assert.Equal(3, countBeeperMessages(t, imp))
}

// a cooperative yield during a quiet-room roster repair must not
// back the room off for a day; an identical retry should finish the repair.
func TestQuietRosterYieldRetriesWithoutBackoff(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	testutil.SkipIfPostgres(t, "sqlite trigger")
	f := newFakeBeeper(t)
	quiet := mediaChat()
	f.addChat(quiet)
	active := mediaChat()
	active.ID = "!active-probe:beeper.local"
	active.LastActivity = time.Now().UTC()
	active.Msgs = nil
	f.addChat(active)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	opts := ImportOptions{AccountID: "signal", NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE conversations SET metadata = '{}'
		WHERE source_id = ? AND source_conversation_id = ?`), source.ID, quiet.ID)
	require.NoError(err)
	f.mu.Lock()
	for _, n := range []string{"bob", "cy", "di"} {
		quiet.Participants = append(quiet.Participants, map[string]any{"id": "@signal_" + n + ":beeper.local", "fullName": n})
	}
	f.mu.Unlock()
	st.DB().SetMaxOpenConns(1)
	yield := false
	registerSQLiteFunc(t, st, "probe_yield", func() int { yield = true; return 0 })
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER probe_yield AFTER INSERT ON participants BEGIN SELECT probe_yield(); END`)
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return yield }})
	require.NoError(err)
	require.True(yield)
	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[quiet.ID])
	assert.Empty(state.Chats[quiet.ID].RosterRetryAfter, "a yield is not a failure; no 24h backoff")
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER probe_yield`)
	require.NoError(err)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	ids, err := st.ListUnresolvedConversationIDs(t.Context(), source.ID)
	require.NoError(err)
	assert.NotContains(ids, quiet.ID, "identical retry should advance the repair")
}

// a stop during the message walk, after membership committed and
// route proof stored, should not leave the committed roster marked unresolved.
func TestMessageWalkStopKeepsCommittedRosterResolved(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	f.pageSize = 1
	chat := budgetTestChat("!probe-walk:beeper.local", 3, time.Now().UTC().Add(-48*time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: stopAfterMessagePages(f, 1)})
	require.NoError(err)
	require.True(sum.Stopped)
	require.Less(countBeeperMessages(t, imp), 3, "walk was interrupted")
	require.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 2)
	ids, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(ids, chat.ID, "roster and route proof are committed and complete")
}

type rosterTransactionCost struct {
	slog.Handler

	begins  int
	onBegin func()
}

func (*rosterTransactionCost) Enabled(context.Context, slog.Level) bool { return true }
func (h *rosterTransactionCost) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "sql tx begin" {
		h.begins++
		h.onBegin()
	}
	return nil
}

func rosterRetryChat() *fakeChat {
	chat := budgetTestChat("!retry-budget:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Type = "group"
	for i := 2; i < 64; i++ {
		chat.Participants = append(chat.Participants, map[string]any{
			"id": fmt.Sprintf("@signal_retry%d:example.test", i), "fullName": fmt.Sprintf("Synthetic Member %d", i),
		})
	}
	return chat
}

// richRetryChat gives every added member a synthetic phone or email.
func richRetryChat() *fakeChat {
	chat := budgetTestChat("!rich-retry:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Type = "group"
	for i := 2; i < 64; i++ {
		member := map[string]any{
			"id": fmt.Sprintf("@signal_rich%d:example.test", i), "fullName": fmt.Sprintf("Synthetic Rich %d", i),
		}
		if i%2 == 0 {
			member["phoneNumber"] = fmt.Sprintf("+1202555%04d", 100+i)
		} else {
			member["email"] = fmt.Sprintf("rich%d@example.test", i)
		}
		chat.Participants = append(chat.Participants, member)
	}
	return chat
}

// registerSQLiteFunc registers fn on the store's single SQLite connection.
func registerSQLiteFunc(t *testing.T, st *store.Store, name string, fn any) {
	t.Helper()
	conn, err := st.DB().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("expected SQLite driver connection, got %T", raw)
		}
		return sqliteConn.RegisterFunc(name, fn, false)
	}))
	require.NoError(t, conn.Close())
}

// serial: SQL logging and the default logger are process-wide.
func TestRosterProgressSurvivesFreshImporters(t *testing.T) {
	for _, tc := range []struct {
		name string
		chat func() *fakeChat
	}{
		{"bare IDs", rosterRetryChat},
		{"phone and email", richRetryChat},
		{"whitespace names", func() *fakeChat {
			chat := rosterRetryChat()
			for i, member := range chat.Participants {
				member["fullName"] = "   "
				member["phoneNumber"] = fmt.Sprintf("+1202555%04d", 100+i)
			}
			return chat
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			chat := tc.chat()
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			previous := slog.Default()
			handler := &rosterOperationQuota{Handler: slog.DiscardHandler}
			slog.SetDefault(slog.New(handler))
			store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
			t.Cleanup(func() { slog.SetDefault(previous); store.ConfigureSQLLogging(store.SQLLogOptions{}) })
			var sourceID int64
			for range 30 {
				handler.operations = 0
				imp = NewImporter(st, imp.client)
				sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return handler.operations >= 130 }})
				require.NoError(err)
				require.Zero(sum.Errors)
				sourceID = sum.SourceID
				if countBeeperMessages(t, imp) == 2 && len(rosterMembers(t, st, sourceID, chat.ID)) == 64 {
					break
				}
			}
			assert.Equal(2, countBeeperMessages(t, imp))
			assert.Len(rosterMembers(t, st, sourceID, chat.ID), 64)
		})
	}
}

type rosterOperationQuota struct {
	slog.Handler

	operations int
}

func (*rosterOperationQuota) Enabled(context.Context, slog.Level) bool { return true }
func (h *rosterOperationQuota) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "sql tx begin" || record.Message == "sql slow" {
		h.operations++
	}
	return nil
}

// serial: SQL logging and the default logger are process-wide.
func TestRosterMatchingProgressSurvivesFreshImporters(t *testing.T) {
	for _, changing := range []bool{false, true} {
		t.Run(fmt.Sprintf("changing=%t", changing), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			room := budgetTestChat("!quota-room:example.test", 2, time.Now().Add(-48*time.Hour))
			aliases := budgetTestChat("!quota-aliases:example.test", 2, time.Now().Add(-72*time.Hour))
			room.Type, aliases.Type = "group", "group"
			for i := range 62 {
				native := fmt.Sprintf("quota-native-%d", i)
				room.Participants = append(room.Participants, map[string]any{"id": fmt.Sprintf("@signal_quota%d:example.test", i), "providerID": native})
				aliases.Participants = append(aliases.Participants, map[string]any{"id": fmt.Sprintf("@signal_aliasquota%d:example.test", i), "providerID": native})
			}
			aliases.Participants = append(aliases.Participants, map[string]any{"id": "@signal_quota_alias_late:example.test", "providerID": "quota-native-late"})
			f.addChat(room)
			f.addChat(aliases)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(err)
			require.True(linked(t, st, participantForBeeperUser(t, st, "@signal_quota61:example.test"), participantForBeeperUser(t, st, "@signal_aliasquota61:example.test")))
			f.mu.Lock()
			room.Participants = append(room.Participants, map[string]any{"id": "@signal_quota_late:example.test", "providerID": "quota-native-late"})
			room.LastActivity = time.Now().Add(time.Hour)
			f.mu.Unlock()
			previous := slog.Default()
			handler := &rosterOperationQuota{Handler: slog.DiscardHandler}
			slog.SetDefault(slog.New(handler))
			store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
			t.Cleanup(func() { slog.SetDefault(previous); store.ConfigureSQLLogging(store.SQLLogOptions{}) })
			for attempt := range 30 {
				if changing && attempt > 0 {
					f.mu.Lock()
					room.Participants = append(room.Participants, map[string]any{"id": fmt.Sprintf("@signal_new%02d:example.test", attempt)})
					f.mu.Unlock()
				}
				handler.operations = 0
				imp = NewImporter(st, imp.client)
				sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return handler.operations >= 160 }})
				require.NoError(err)
				require.Zero(sum.Errors)
				first, _, err := st.ParticipantByIdentifierContext(t.Context(), "beeper", imp.res.identifierForUser("@signal_quota_late:example.test"))
				require.NoError(err)
				if first != 0 {
					obs, err := st.ListParticipantObservationsContext(t.Context(), first, true)
					require.NoError(err)
					if len(obs) > 0 && linked(t, st, first, participantForBeeperUser(t, st, "@signal_quota_alias_late:example.test")) {
						state := latestBeeperState(t, imp).Chats[room.ID]
						if state != nil && state.RosterWork != nil && state.RosterWork.Phase == "done" {
							break
						}
					}
				}
			}
			first := participantForBeeperUser(t, st, "@signal_quota_late:example.test")
			alias := participantForBeeperUser(t, st, "@signal_quota_alias_late:example.test")
			assert.True(linked(t, st, first, alias))
			observations, err := st.ListParticipantObservationsContext(t.Context(), first, true)
			require.NoError(err)
			assert.NotEmpty(observations)
			assert.Equal("done", latestBeeperState(t, imp).Chats[room.ID].RosterWork.Phase)
		})
	}
}

func TestRosterMembershipResumesAfterStop(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger requests stop at first membership write")
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := rosterRetryChat()
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	st.DB().SetMaxOpenConns(1)
	stopped := false
	registerSQLiteFunc(t, st, "stop_membership_batch", func() int { stopped = true; return 0 })
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER stop_membership_batch AFTER INSERT ON conversation_participants BEGIN SELECT stop_membership_batch(); END`)
	require.NoError(err)
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return stopped }})
	require.NoError(err)
	require.True(sum.Stopped)
	run, err := st.GetLastSuccessfulSync(sum.SourceID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID].RosterWork)
	members := rosterMembers(t, st, sum.SourceID, chat.ID)
	assert.NotEmpty(members)
	assert.LessOrEqual(len(members), 1, "only the in-flight member may finish")
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER stop_membership_batch`)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 64)
	assert.Equal(2, countBeeperMessages(t, imp))
}

func TestRosterCleanupResumesAgainstChangedSnapshot(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprintf("prune=%t", prune), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			chat := rosterRetryChat()
			f.addChat(chat)
			other := budgetTestChat("!other:example.test", 1, time.Now())
			f.addChat(other)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(err)
			otherReceipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(other.ID))
			require.NoError(err)
			require.NotEmpty(otherReceipts)
			work := &rosterWork{}
			restoredMember := chat.Participants[40]
			f.mu.Lock()
			chat.Participants = chat.Participants[:24]
			chat.Participants[1]["isAdmin"] = true
			f.mu.Unlock()
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			opts := ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
				var count int
				err := st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = (SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?)`), sum.SourceID, chat.ID).Scan(&count)
				require.NoError(err)
				if prune {
					receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
					require.NoError(err)
					return len(receipts) <= 48
				}
				return count <= 48
			}}
			_, _, _, err = imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, opts, &ImportSummary{}) //nolint:dogsled // Only the stop error is needed.
			require.ErrorIs(err, errBeeperBudgetExpired)
			if prune {
				receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
				require.NoError(err)
				require.Len(receipts, 48)
				assert.Equal("members", work.Phase)
				assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 24)
			} else {
				assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 48)
			}
			unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
			require.NoError(err)
			assert.Contains(unresolved, chat.ID)
			f.mu.Lock()
			chat.Participants = append(chat.Participants, restoredMember)
			f.mu.Unlock()
			detail, err = imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			imp = NewImporter(st, imp.client)
			_, complete, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
			require.NoError(err)
			assert.True(complete)
			members := rosterMembers(t, st, sum.SourceID, chat.ID)
			assert.Len(members, 25)
			restoredID, ok := restoredMember["id"].(string)
			require.True(ok)
			restored := participantForBeeperUser(t, st, restoredID)
			assert.Contains(members, restored)
			adminID, ok := chat.Participants[1]["id"].(string)
			require.True(ok)
			admin := participantForBeeperUser(t, st, adminID)
			var role string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE participant_id = ? AND conversation_id = (SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?)`), admin, sum.SourceID, chat.ID).Scan(&role))
			assert.Equal("admin", role)
			receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
			require.NoError(err)
			expected := make([]string, 0, len(chat.Participants))
			for _, member := range chat.Participants {
				expected = append(expected, imp.res.identifierForUser(member["id"].(string)))
			}
			actual := make([]string, 0, len(receipts))
			for key := range receipts {
				actual = append(actual, key)
			}
			assert.ElementsMatch(expected, actual)
			otherAfter, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(other.ID))
			require.NoError(err)
			assert.Equal(otherReceipts, otherAfter)
			assert.Equal("done", work.Phase)
		})
	}
}

// serial: SQL logging and the default logger are process-wide.
func TestRosterResumeUsesArchivedBindingsAfterCheckpointLossAndJoin(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := richRetryChat()
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_joined:example.test", "fullName": "Joined", "email": "joined@example.test"})
	f.mu.Unlock()
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	handler := &rosterTransactionCost{Handler: slog.DiscardHandler, onBegin: func() {}}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	imp = NewImporter(st, imp.client)
	opts := ImportOptions{AccountID: "signal", ShouldStop: func() bool { return handler.begins >= 20 }}
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, opts, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	assert.True(complete)
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 65)
}

func TestFinalMemberStopKeepsMessageTurn(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to stop during the last member's capture")
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!final-member:example.test", 2, time.Now().Add(-48*time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state := latestBeeperState(t, imp)
	f.mu.Lock()
	chat.Participants[1]["username"] = "newname"
	chat.Msgs = append(chat.Msgs, fakeMsg{ID: chat.ID + "-2", SortKey: 2, Timestamp: time.Now().Add(-time.Hour), Text: "msg 2", SenderID: "@signal_ann:beeper.local", SenderName: "Ann"})
	chat.LastActivity = time.Now().Add(-time.Hour)
	f.mu.Unlock()
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	st.DB().SetMaxOpenConns(1)
	stop := false
	registerSQLiteFunc(t, st, "yield_final_member", func() int { stop = true; return 0 })
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER yield_final_member AFTER UPDATE OF service_id ON participant_identifiers
		WHEN NEW.identifier_value = '`+imp.res.identifierForUser("@signal_ann:beeper.local")+`' BEGIN SELECT yield_final_member(); END`)
	require.NoError(err)
	opts := ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { return stop }}
	_, _, err = imp.syncChat(t.Context(), 0, sum.SourceID, detail, opts, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	require.True(stop, "the last member's capture must request the stop")
	require.NotNil(state.Chats[chat.ID].RosterWork, "a stop after the last member keeps the message turn")
	assert.False(state.Chats[chat.ID].RosterWork.MemberFirst)
	assert.Equal(2, countBeeperMessages(t, imp), "no message work starts after the stop")

	stop = false
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER yield_proof AFTER UPDATE OF metadata ON conversations
		WHEN json_extract(NEW.metadata, '$.messaging_route.membership_complete') = 1 BEGIN SELECT yield_final_member(); END`)
	require.NoError(err)
	_, _, err = imp.syncChat(t.Context(), 0, sum.SourceID, detail, opts, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	require.True(stop, "the message turn's proof refresh must request the stop")
	assert.False(state.Chats[chat.ID].RosterWork.MemberFirst, "a message turn spent on proof is not used up")
	assert.Equal(2, countBeeperMessages(t, imp))

	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER yield_proof`)
	require.NoError(err)
	stop = false
	_, _, err = imp.syncChat(t.Context(), 0, sum.SourceID, detail, opts, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	assert.False(stop, "the message turn does not match members again")
	assert.Equal(3, countBeeperMessages(t, imp))
}
