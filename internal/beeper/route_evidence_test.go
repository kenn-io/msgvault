package beeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The real importer must capture authoritative bridge proof and the exact
// roster. A display label or provider user ID must not classify the network.
func TestImportMessagingRouteBridgeProof(t *testing.T) {
	for _, test := range []struct{ name, accountJSON, status, reason string }{
		{"bridge", "{\"accountID\":\"account-a\",\"bridge\":{\"type\":\"whatsapp\"},\"status\":\"connected\"}", "archive_verified", ""},
		{"label only", "{\"accountID\":\"account-a\",\"network\":\"WhatsApp\"}", "unresolved", "network_unverified"},
		{"mismatched account", "{\"accountID\":\"account-b\",\"bridge\":{\"type\":\"whatsapp\"}}", "unresolved", "account_binding_mismatch"},
		{"service unavailable", "", "unresolved", "account_lookup_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := newFakeBeeper(t)
			f.addChat(&fakeChat{ID: "!route:example.test", AccountID: "account-a", Network: "Display label", Type: "single", Title: "Avery Example", Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}, {"id": "@whatsapp_synthetic:example.test", "fullName": "Avery Example"}}})
			provider := f.handler()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/accounts/account-a" {
					assertions.Equal(http.MethodGet, r.Method)
					if test.accountJSON == "" {
						// The provider requests immediate retries; exhaust the
						// real retry loop without a long backoff.
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(test.accountJSON))
					return
				}
				provider(w, r)
			}))
			t.Cleanup(srv.Close)
			st := testutil.NewSQLiteTestStore(t)
			imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
			_, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
			requirements.NoError(err)
			var peer int64
			requirements.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE display_name='Avery Example'`).Scan(&peer))
			person, _, err := st.CreatePersonFromParticipant(peer)
			requirements.NoError(err)
			page, err := st.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
			requirements.NoError(err)
			requirements.Len(page.Routes.Items, 1)
			assertions.Equal(test.status, page.Routes.Items[0].Status)
			if test.reason != "" {
				assertions.Contains(page.Routes.Items[0].Reasons, test.reason)
			} else {
				assertions.Equal("whatsapp", page.Routes.Items[0].Network)
			}
			if test.reason == "account_lookup_failed" {
				var failure string
				requirements.NoError(st.DB().QueryRowContext(t.Context(),
					"SELECT failure FROM source_messaging_route_failures WHERE source_id = ?",
					page.Routes.Items[0].SourceID,
				).Scan(&failure))
				assertions.Equal("account_lookup_failed", failure)
			}
			var raw string
			requirements.NoError(st.DB().QueryRow(`SELECT metadata FROM conversations`).Scan(&raw))
			var metadata struct {
				Route *store.MessagingRouteEvidence `json:"messaging_route"`
			}
			requirements.NoError(json.Unmarshal([]byte(raw), &metadata))
			requirements.NotNil(metadata.Route)
			assertions.Len(metadata.Route.ParticipantIDs, 2)
			assertions.Len(metadata.Route.SelfParticipantIDs, 1)
		})
	}
}

func TestCopySubsetPreservesImportedMessagingRouteFreshness(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newFakeBeeper(t)
	now := time.Now().UTC()
	f.addChat(&fakeChat{
		ID: "!route:example.test", AccountID: "account-a", Type: "single",
		Title: "Avery Example", LastActivity: now,
		Participants: []map[string]any{
			{"id": "@self:example.test", "isSelf": true},
			{"id": "@peer:example.test", "fullName": "Avery Example"},
		},
		Msgs: []fakeMsg{
			{ID: "message-a", SortKey: 1, Timestamp: now.Add(-time.Minute), Text: "Hello", SenderID: "@peer:example.test", SenderName: "Avery Example"},
			{ID: "message-b", SortKey: 2, Timestamp: now, Text: "Hi", SenderID: "@self:example.test", IsSender: true},
		},
	})
	provider := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/account-a" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"connected"}`))
			return
		}
		provider(w, r)
	}))
	t.Cleanup(srv.Close)
	sourcePath := filepath.Join(t.TempDir(), "msgvault.db")
	source, err := store.Open(sourcePath)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(source.Close()) })
	requirements.NoError(source.InitSchema())
	imp := NewImporter(source, NewClient(srv.URL, testToken, 10000))
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
	requirements.NoError(err)
	account, err := source.GetSourceByTypeAndIdentifier("beeper", "account-a")
	requirements.NoError(err)
	requirements.True(account.LastSyncAt.Valid)
	// Older Beeper imports recorded freshness only in sync history.
	_, err = source.DB().Exec(`UPDATE sources SET last_sync_at=NULL WHERE id=?`, account.ID)
	requirements.NoError(err)
	var peer int64
	requirements.NoError(source.DB().QueryRow(`SELECT id FROM participants WHERE display_name='Avery Example'`).Scan(&peer))
	person, _, err := source.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	before, err := source.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(before.Routes.Items, 1)
	requirements.Equal("archive_verified", before.Routes.Items[0].Status)
	requirements.NotNil(before.Routes.Items[0].SourceLastSyncAt)

	destinationDir := filepath.Join(t.TempDir(), "subset")
	result, err := store.CopySubset(sourcePath, destinationDir, 2, false)
	requirements.NoError(err)
	requirements.EqualValues(2, result.Messages)
	requirements.EqualValues(2, result.Participants, "the complete route roster must survive the copy")
	destination, err := store.Open(filepath.Join(destinationDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(destination.Close()) })
	after, err := destination.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(after.Routes.Items, 1)
	assertions.Equal("archive_verified", after.Routes.Items[0].Status, "reasons: %v", after.Routes.Items[0].Reasons)
	assertions.Equal(before.Routes.Items[0].SourceLastSyncAt, after.Routes.Items[0].SourceLastSyncAt)

	stale := now.Add(-8 * 24 * time.Hour)
	_, err = destination.DB().Exec(`UPDATE sources SET last_sync_at=? WHERE id=?`, stale, account.ID)
	requirements.NoError(err)
	aged, err := destination.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(aged.Routes.Items, 1)
	requirements.Contains(aged.Routes.Items[0].Reasons, "source_stale")

	resync := NewImporter(destination, NewClient(srv.URL, testToken, 10000))
	f.setMessageListFailure("!route:example.test", true)
	_, err = resync.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
	var partial *PartialSyncError
	requirements.ErrorAs(err, &partial)
	failedAccount, err := destination.GetSourceByID(account.ID)
	requirements.NoError(err)
	assertions.Equal(stale, failedAccount.LastSyncAt.Time, "a partial run must not refresh source freshness")
	f.setMessageListFailure("!route:example.test", false)
	summary, err := resync.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
	requirements.NoError(err)
	requirements.Zero(summary.Errors)
	refreshed, err := destination.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(refreshed.Routes.Items, 1)
	assertions.Equal("archive_verified", refreshed.Routes.Items[0].Status, "reasons: %v", refreshed.Routes.Items[0].Reasons)
	requirements.NotNil(refreshed.Routes.Items[0].SourceLastSyncAt)
	assertions.True(refreshed.Routes.Items[0].SourceLastSyncAt.After(stale))
}

func TestImportKeepsSourceRouteFailureForInvalidAccountMetadata(t *testing.T) {
	for _, test := range []struct {
		name, accountJSON, reason string
	}{
		{
			name:        "disconnected account",
			accountJSON: `{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"disconnected"}`,
			reason:      "account_not_connected",
		},
		{
			name:        "account binding mismatch",
			accountJSON: `{"accountID":"account-b","bridge":{"type":"whatsapp"},"status":"connected"}`,
			reason:      "account_binding_mismatch",
		},
		{
			name:        "missing bridge proof",
			accountJSON: `{"accountID":"account-a","status":"connected"}`,
			reason:      "network_unverified",
		},
		{
			name:        "missing bridge type",
			accountJSON: `{"accountID":"account-a","bridge":{},"status":"connected"}`,
			reason:      "network_unverified",
		},
		{
			name:        "empty bridge type",
			accountJSON: `{"accountID":"account-a","bridge":{"type":""},"status":"connected"}`,
			reason:      "network_unverified",
		},
		{
			name:        "whitespace bridge type",
			accountJSON: `{"accountID":"account-a","bridge":{"type":"  "},"status":"connected"}`,
			reason:      "network_unverified",
		},
		{
			name:        "missing status",
			accountJSON: `{"accountID":"account-a","bridge":{"type":"whatsapp"}}`,
			reason:      "account_not_connected",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := newFakeBeeper(t)
			provider := f.handler()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/accounts/account-a" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(test.accountJSON))
					return
				}
				provider(w, r)
			}))
			t.Cleanup(srv.Close)

			st := testutil.NewSQLiteTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "account-a")
			requirements.NoError(err)
			_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
			requirements.NoError(err)
			participantID, err := st.EnsureParticipantByIdentifier("email", "avery@example.test", "Avery Example")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participantID)
			requirements.NoError(err)
			conversationID, err := st.EnsureConversationWithType(source.ID, "!quiet:example.test", "direct_chat", "Avery Example")
			requirements.NoError(err)
			requirements.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
			requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conversationID, store.MessagingRouteEvidence{
				ChatID:             "!quiet:example.test",
				AccountID:          "account-a",
				Network:            "whatsapp",
				ProviderType:       "single",
				ObservedAt:         time.Now().UTC(),
				MembershipComplete: true,
				ParticipantIDs:     []int64{participantID},
				SelfParticipantIDs: []int64{},
				MemberChatIDs:      []string{},
			}))
			query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
			before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(before.Routes.Items, 1)
			assertions.Equal("archive_verified", before.Routes.Items[0].Status)

			imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
			requirements.NoError(err)
			assertions.Zero(sum.ChatsProcessed, "the quiet archived chat was not revisited")

			after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(after.Routes.Items, 1)
			assertions.Equal("unresolved", after.Routes.Items[0].Status)
			assertions.Contains(after.Routes.Items[0].Reasons, test.reason)
			var failure string
			requirements.NoError(st.DB().QueryRow(`SELECT failure FROM source_messaging_route_failures WHERE source_id=?`, source.ID).Scan(&failure))
			assertions.Equal(test.reason, failure)
		})
	}
}

func TestImportAccountLookupCancellationInvalidatesMessagingRoutes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	accountLookupStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts/account-a" {
			http.NotFound(w, r)
			return
		}
		close(accountLookupStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("email", "avery@example.test", "Avery Example")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!account-cancel:example.test", "direct_chat", "Avery Example")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
	requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conversationID, store.MessagingRouteEvidence{
		ChatID: "!account-cancel:example.test", AccountID: "account-a", Network: "whatsapp", ProviderType: "single",
		ObservedAt: time.Now().UTC(), MembershipComplete: true, ParticipantIDs: []int64{participantID},
		SelfParticipantIDs: []int64{}, MemberChatIDs: []string{},
	}))
	requirements.NoError(st.SetSourceMessagingRouteFailureContext(t.Context(), source.ID, ""))
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(before.Routes.Items, 1)
	assertions.Equal("archive_verified", before.Routes.Items[0].Status)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		<-accountLookupStarted
		cancel()
	}()
	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	_, err = imp.Import(ctx, ImportOptions{AccountID: "account-a", NoMedia: true})
	requirements.ErrorIs(err, context.Canceled)
	select {
	case <-accountLookupStarted:
	default:
		requirements.FailNow("the account lookup did not start")
	}

	after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(after.Routes.Items, 1)
	assertions.Equal("unresolved", after.Routes.Items[0].Status)
	assertions.Contains(after.Routes.Items[0].Reasons, "account_lookup_failed")
	var failure string
	requirements.NoError(st.DB().QueryRow(`SELECT failure FROM source_messaging_route_failures WHERE source_id=?`, source.ID).Scan(&failure))
	assertions.Equal("account_lookup_failed", failure)
}

func TestImportClearsSourceRouteFailureAfterAccountLookupRecovers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newFakeBeeper(t)
	provider := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/account-a" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"connected"}`))
			return
		}
		provider(w, r)
	}))
	t.Cleanup(srv.Close)

	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("email", "quiet@example.test", "Quiet Example")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!quiet:example.test", "direct_chat", "Quiet Example")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
	requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conversationID, store.MessagingRouteEvidence{
		ChatID:             "!quiet:example.test",
		AccountID:          "account-a",
		Network:            "whatsapp",
		ProviderType:       "single",
		ObservedAt:         time.Now().UTC(),
		MembershipComplete: true,
		ParticipantIDs:     []int64{participantID},
		SelfParticipantIDs: []int64{},
		MemberChatIDs:      []string{},
	}))
	requirements.NoError(st.SetSourceMessagingRouteFailureContext(t.Context(), source.ID, "account_lookup_failed"))

	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
	requirements.NoError(err)

	page, err := st.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: person.VCardUID})
	requirements.NoError(err)
	requirements.Len(page.Routes.Items, 1)
	assertions.Equal("archive_verified", page.Routes.Items[0].Status)
	assertions.NotContains(page.Routes.Items[0].Reasons, "account_lookup_failed")
}

func TestImportMissingChatInvalidatesMessagingRoute(t *testing.T) {
	for _, test := range []struct {
		name      string
		limit     int
		completed bool
		full      bool
	}{
		{name: "unfinished backfill", limit: 1},
		{name: "completed backfill", completed: true},
		{name: "completed backfill full sync", completed: true, full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
			f := newFakeBeeper(t)
			f.addChat(&fakeChat{
				ID: "!missing-route:example.test", AccountID: "account-a", Network: "Display label",
				Type: "single", Title: "Avery Example", LastActivity: base.Add(time.Minute),
				Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}, {"id": "@avery:example.test", "fullName": "Avery Example"}},
				Msgs: []fakeMsg{
					{ID: "route-one", SortKey: 1, Timestamp: base, Text: "one", SenderID: "@avery:example.test", SenderName: "Avery Example"},
					{ID: "route-two", SortKey: 2, Timestamp: base.Add(time.Minute), Text: "two", SenderID: "@avery:example.test", SenderName: "Avery Example"},
				},
			})
			f.addChat(&fakeChat{
				ID: "!surviving-anchor:example.test", AccountID: "account-a", Network: "Display label",
				Type: "group", Title: "Surviving anchor", LastActivity: time.Now().UTC().Truncate(time.Second),
				Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}},
				Msgs: []fakeMsg{{
					ID: "surviving-anchor-message", SortKey: 1, Timestamp: time.Now().UTC().Truncate(time.Second),
					Text: "anchor", SenderID: "@self:example.test", SenderName: "Self",
				}},
			})
			provider := f.handler()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/accounts/account-a" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"connected"}`))
					return
				}
				provider(w, r)
			}))
			t.Cleanup(srv.Close)

			st := testutil.NewSQLiteTestStore(t)
			imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
			first, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", Limit: test.limit, NoMedia: true})
			requirements.NoError(err)
			assertions.Equal(int64(2), first.ChatsProcessed)
			var peer int64
			requirements.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE display_name='Avery Example'`).Scan(&peer))
			person, _, err := st.CreatePersonFromParticipant(peer)
			requirements.NoError(err)
			query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
			verified, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(verified.Routes.Items, 1)
			assertions.Equal("archive_verified", verified.Routes.Items[0].Status)

			source, err := st.GetOrCreateSource("beeper", "account-a")
			requirements.NoError(err)
			firstRun, err := st.GetLastSuccessfulSync(source.ID)
			requirements.NoError(err)
			firstState, err := LoadSyncState(firstRun.CursorAfter.String)
			requirements.NoError(err)
			requirements.NotNil(firstState.Chats["!missing-route:example.test"])
			requirements.Equal(test.completed, firstState.Chats["!missing-route:example.test"].Done)

			// One chat disappears while another anchor survives, allowing sync to proceed.
			f.mu.Lock()
			for i, chat := range f.chats {
				if chat.ID == "!missing-route:example.test" {
					f.chats = append(f.chats[:i], f.chats[i+1:]...)
					break
				}
			}
			f.mu.Unlock()
			if test.completed && !test.full {
				// Activity-filtered listings cannot establish that a quiet chat disappeared.
				_, err = imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
				requirements.NoError(err)
				filtered, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
				requirements.NoError(err)
				requirements.Len(filtered.Routes.Items, 1)
				assertions.Equal("archive_verified", filtered.Routes.Items[0].Status)

				oldInterval := tailScanInterval
				tailScanInterval = 0
				t.Cleanup(func() { tailScanInterval = oldInterval })
				// A failed unfiltered listing must not turn omissions into disappearance.
				f.setChatSearchFailure(true)
				_, err = imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
				requirements.Error(err)
				failed, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
				requirements.NoError(err)
				requirements.Len(failed.Routes.Items, 1)
				assertions.Equal("archive_verified", failed.Routes.Items[0].Status)
				f.setChatSearchFailure(false)
			}
			second, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true, Full: test.full})
			requirements.NoError(err)
			assertions.Equal(int64(1), second.ChatsProcessed, "the surviving anchor chat is the only one revisited")

			after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(after.Routes.Items, 1)
			assertions.Equal("unresolved", after.Routes.Items[0].Status)
			assertions.Contains(after.Routes.Items[0].Reasons, "membership_incomplete")
			assertions.NotContains(after.Routes.Items[0].Reasons, "account_lookup_failed")

			lastRun, err := st.GetLastSuccessfulSync(source.ID)
			requirements.NoError(err)
			state, err := LoadSyncState(lastRun.CursorAfter.String)
			requirements.NoError(err)
			requirements.NotNil(state.Chats["!missing-route:example.test"])
			assertions.True(state.Chats["!missing-route:example.test"].Done)

			if test.completed {
				var archived int
				requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id IN ('route-one', 'route-two') AND deleted_at IS NULL`).Scan(&archived))
				assertions.Equal(2, archived, "chat disappearance must retain archived messages")
			}
		})
	}
}

func TestImportUnfinishedChatFetchFailureInvalidatesMessagingRoute(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	chatID := "!unfinished-fetch-error:example.test"
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{
		ID: chatID, AccountID: "account-a", Network: "Display label",
		Type: "single", Title: "Avery Example", LastActivity: base.Add(time.Minute),
		Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}, {"id": "@avery:example.test", "fullName": "Avery Example"}},
		Msgs: []fakeMsg{
			{ID: "unfinished-fetch-one", SortKey: 1, Timestamp: base, Text: "one", SenderID: "@avery:example.test", SenderName: "Avery Example"},
			{ID: "unfinished-fetch-two", SortKey: 2, Timestamp: base.Add(time.Minute), Text: "two", SenderID: "@avery:example.test", SenderName: "Avery Example"},
		},
	})
	f.addChat(&fakeChat{
		ID: "!surviving-fetch-anchor:example.test", AccountID: "account-a", Network: "Display label",
		Type: "group", Title: "Surviving anchor", LastActivity: time.Now().UTC().Truncate(time.Second),
		Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}},
		Msgs: []fakeMsg{{
			ID: "surviving-fetch-anchor-message", SortKey: 1, Timestamp: time.Now().UTC().Truncate(time.Second),
			Text: "anchor", SenderID: "@self:example.test", SenderName: "Self",
		}},
	})
	provider := f.handler()
	accountLookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/account-a" {
			accountLookups++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"connected"}`))
			return
		}
		provider(w, r)
	}))
	t.Cleanup(srv.Close)

	st := testutil.NewSQLiteTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	first, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", Limit: 1, NoMedia: true})
	requirements.NoError(err)
	assertions.Equal(int64(2), first.ChatsProcessed)
	var peer int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE display_name='Avery Example'`).Scan(&peer))
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	verified, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(verified.Routes.Items, 1)
	assertions.Equal("archive_verified", verified.Routes.Items[0].Status)

	// The unfinished chat is outside the incremental listing window, so the
	// importer probes it directly. A transient detail failure must invalidate
	// its old roster proof even though the account lookup still succeeds.
	f.setChatGetFailure(chatID, true)
	second, err := imp.Import(t.Context(), ImportOptions{AccountID: "account-a", NoMedia: true})
	requirements.Error(err)
	requirements.ErrorContains(err, "partial Beeper sync: 1 fetch error(s)")
	assertions.Equal(int64(1), second.ChatsProcessed, "only the surviving anchor is processed")
	assertions.Equal(int64(1), second.FetchErrors)
	assertions.Equal(2, accountLookups, "both account lookups succeed")

	after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(after.Routes.Items, 1)
	assertions.Equal("unresolved", after.Routes.Items[0].Status)
	assertions.Contains(after.Routes.Items[0].Reasons, "membership_fetch_failed")
	assertions.NotContains(after.Routes.Items[0].Reasons, "account_lookup_failed")
	assertions.Contains(f.requests(), "/v1/chats/!unfinished-fetch-error:example.test?maxParticipantCount=-1")
}

func TestEnumerateUnfinishedChatBudgetExpiredProbeInvalidatesMessagingRoute(t *testing.T) {
	testEnumerateUnfinishedChatProbeInterruptionInvalidatesMessagingRoute(t, false)
}

func TestEnumerateUnfinishedChatCanceledProbeInvalidatesMessagingRoute(t *testing.T) {
	testEnumerateUnfinishedChatProbeInterruptionInvalidatesMessagingRoute(t, true)
}

func testEnumerateUnfinishedChatProbeInterruptionInvalidatesMessagingRoute(t *testing.T, cancelParent bool) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-30 * 24 * time.Hour)
	chatID := "!unfinished-budget-error:example.test"
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{
		ID: chatID, AccountID: "account-a", Network: "Display label",
		Type: "single", Title: "Avery Example", LastActivity: base.Add(time.Minute),
		Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}, {"id": "@avery:example.test", "fullName": "Avery Example"}},
		Msgs: []fakeMsg{
			{ID: "unfinished-budget-one", SortKey: 1, Timestamp: base, Text: "one", SenderID: "@avery:example.test", SenderName: "Avery Example"},
			{ID: "unfinished-budget-two", SortKey: 2, Timestamp: base.Add(time.Minute), Text: "two", SenderID: "@avery:example.test", SenderName: "Avery Example"},
		},
	})
	f.addChat(&fakeChat{
		ID: "!surviving-budget-anchor:example.test", AccountID: "account-a", Network: "Display label",
		Type: "group", Title: "Surviving anchor", LastActivity: now,
		Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}},
		Msgs: []fakeMsg{{
			ID: "surviving-budget-anchor-message", SortKey: 1, Timestamp: now,
			Text: "anchor", SenderID: "@self:example.test", SenderName: "Self",
		}},
	})
	provider := f.handler()
	probeStarted := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/account-a" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountID":"account-a","bridge":{"type":"whatsapp"},"status":"connected"}`))
			return
		}
		if r.URL.Path == "/v1/chats/"+chatID {
			close(probeStarted)
			<-r.Context().Done()
			return
		}
		provider(w, r)
	})
	client := NewClient("http://beeper.test", testToken, 10000)
	client.http.Transport = handlerTransport(handler)

	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("email", "avery@example.test", "Avery Example")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, chatID, "direct_chat", "Avery Example")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
	requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conversationID, store.MessagingRouteEvidence{
		ChatID: chatID, AccountID: "account-a", Network: "whatsapp", ProviderType: "single",
		ObservedAt: now, MembershipComplete: true, ParticipantIDs: []int64{participantID},
		SelfParticipantIDs: []int64{}, MemberChatIDs: []string{},
	}))
	account, err := client.GetAccount(t.Context(), "account-a")
	requirements.NoError(err)
	requirements.Empty(accountRouteProofFailure(account, "account-a"))
	requirements.NoError(st.SetSourceMessagingRouteFailureContext(t.Context(), source.ID, ""))
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	verified, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(verified.Routes.Items, 1)
	assertions.Equal("archive_verified", verified.Routes.Items[0].Status)

	state := NewSyncState()
	state.ListWatermark = formatWatermark(now)
	state.EnsureChat(chatID).Done = false
	syncID, err := st.StartSync(source.ID, sourceTypeBeeper)
	requirements.NoError(err)
	imp := NewImporter(st, client)
	sum := &ImportSummary{}
	opts := ImportOptions{
		AccountID: "account-a", NoMedia: true, routeAccount: account,
	}
	ctx := t.Context()
	if cancelParent {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		t.Cleanup(cancel)
		go func() {
			<-probeStarted
			cancel()
		}()
	} else {
		opts.StopAt = time.Now().Add(2 * time.Second)
	}
	chats, err := imp.enumerateChats(ctx, syncID, source.ID, opts, state, now.Add(-reconcileWindow), false, sum)
	if cancelParent {
		requirements.ErrorIs(err, context.Canceled)
		assertions.False(sum.Stopped)
		assertions.Empty(chats)
	} else {
		requirements.NoError(err)
		assertions.True(sum.Stopped)
		assertions.Len(chats, 1, "the old unfinished chat is excluded from search and its probe stops the pass")
	}
	select {
	case <-probeStarted:
	default:
		requirements.FailNow("the direct unfinished-chat probe did not start")
	}
	after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(after.Routes.Items, 1)
	assertions.Equal("unresolved", after.Routes.Items[0].Status)
	if cancelParent {
		assertions.Contains(after.Routes.Items[0].Reasons, "membership_incomplete")
	} else {
		assertions.Contains(after.Routes.Items[0].Reasons, "membership_fetch_failed")
	}
	assertions.NotContains(after.Routes.Items[0].Reasons, "account_lookup_failed")
	requirements.NoError(st.CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, source.ID, ""))
}

func TestMessagingRouteRefreshInvalidatesIncompleteRoster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newFakeBeeper(t)
	srv := f.server()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	opts := ImportOptions{AccountID: "account-a", routeAccount: &Account{AccountID: "account-a", Bridge: &AccountBridge{Type: "whatsapp"}, Status: "connected"}}
	ch := &Chat{ID: "!refresh:example.test", AccountID: "account-a", Type: "single"}
	ch.Participants.Items = []Participant{{ID: "@self:example.test", IsSelf: true}, {ID: "@avery:example.test", FullName: "Avery Example"}}
	ch.Participants.Total = 2
	conv, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, ch, opts, &ImportSummary{})
	requirements.NoError(err)
	var peer int64
	requirements.NoError(st.DB().QueryRow(`SELECT participant_id FROM conversation_participants WHERE conversation_id=? ORDER BY participant_id DESC LIMIT 1`, conv).Scan(&peer))
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(before.Routes.Items, 1)
	assertions.Equal("archive_verified", before.Routes.Items[0].Status)
	// A failed full-roster read must replace earlier complete proof even though
	// the archive retains members omitted by this truncated listing.
	ch.Participants.HasMore = true
	ch.Participants.Items = ch.Participants.Items[:1]
	refreshed, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, ch, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(conv, refreshed)
	incomplete, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(incomplete.Routes.Items, 1)
	assertions.Equal("unresolved", incomplete.Routes.Items[0].Status)
	assertions.Contains(incomplete.Routes.Items[0].Reasons, "membership_incomplete")
	// A subsequent complete roster with that person removed clears the route.
	ch.Participants.HasMore = false
	ch.Participants.Total = 1
	refreshed, _, _, err = imp.ensureConversation(t.Context(), 0, source.ID, ch, opts, &ImportSummary{})
	requirements.NoError(err)
	assertions.Equal(conv, refreshed)
	removed, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	assertions.Empty(removed.Routes.Items)
}

func TestUnfinishedChatRosterFetchInterruptionInvalidatesMessagingRoute(t *testing.T) {
	for _, test := range []struct {
		name          string
		cancelContext bool
	}{
		{name: "scheduled budget expiry"},
		{name: "caller cancellation", cancelContext: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			requestStarted := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)

			st := testutil.NewSQLiteTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "account-a")
			requirements.NoError(err)
			_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
			requirements.NoError(err)
			imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
			opts := ImportOptions{
				AccountID:    "account-a",
				routeAccount: &Account{AccountID: "account-a", Bridge: &AccountBridge{Type: "whatsapp"}, Status: "connected"},
			}
			ch := &Chat{ID: "!roster-interruption:example.test", AccountID: "account-a", Type: "single"}
			ch.Participants.Items = []Participant{
				{ID: "@self:example.test", IsSelf: true},
				{ID: "@avery:example.test", FullName: "Avery Example"},
			}
			ch.Participants.Total = 2
			conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, ch, opts, &ImportSummary{})
			requirements.NoError(err)
			var participantID int64
			requirements.NoError(st.DB().QueryRow(`SELECT participant_id FROM conversation_participants WHERE conversation_id=? ORDER BY participant_id DESC LIMIT 1`, conversationID).Scan(&participantID))
			person, _, err := st.CreatePersonFromParticipant(participantID)
			requirements.NoError(err)
			query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
			before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(before.Routes.Items, 1)
			assertions.Equal("archive_verified", before.Routes.Items[0].Status)

			ch.Participants.HasMore = true
			ch.Participants.Items = ch.Participants.Items[:1]
			ctx := t.Context()
			if test.cancelContext {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				t.Cleanup(cancel)
				go func() {
					<-requestStarted
					cancel()
				}()
			} else {
				opts.StopAt = time.Now().Add(500 * time.Millisecond)
			}
			_, _, _, err = imp.ensureConversation(ctx, 0, source.ID, ch, opts, &ImportSummary{})
			if test.cancelContext {
				requirements.ErrorIs(err, context.Canceled)
			} else {
				requirements.ErrorIs(err, errBeeperBudgetExpired)
			}
			select {
			case <-requestStarted:
			default:
				requirements.FailNow("the full-roster request did not start")
			}

			after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Len(after.Routes.Items, 1)
			assertions.Equal("unresolved", after.Routes.Items[0].Status)
			assertions.Contains(after.Routes.Items[0].Reasons, "membership_incomplete")
		})
	}
}

func TestMediaChatRefreshInvalidatesPriorRouteMembershipProof(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newFakeBeeper(t)
	chat := &fakeChat{ID: "!media-refresh:example.test", AccountID: "account-a", Type: "single"}
	chat.Participants = []map[string]any{{"id": "@avery:example.test", "fullName": "Avery Example"}}
	f.addChat(chat)

	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("email", "avery@example.test", "Avery Example")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, chat.ID, "direct_chat", "Avery Example")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))
	requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conversationID, store.MessagingRouteEvidence{
		ChatID:             chat.ID,
		AccountID:          "account-a",
		Network:            "whatsapp",
		ProviderType:       "single",
		ObservedAt:         time.Now().UTC(),
		MembershipComplete: true,
		ParticipantIDs:     []int64{participantID},
		SelfParticipantIDs: []int64{},
		MemberChatIDs:      []string{},
	}))
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(before.Routes.Items, 1)
	assertions.Equal("archive_verified", before.Routes.Items[0].Status)

	srv := f.server()
	t.Cleanup(srv.Close)
	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	refresh, err := imp.refreshChatContext(t.Context(), 0, source.ID, conversationID, chat.ID, &ImportSummary{})
	requirements.NoError(err)
	requirements.NotNil(refresh)
	assertions.True(refresh.found)

	after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(after.Routes.Items, 1)
	assertions.Equal("unresolved", after.Routes.Items[0].Status)
	assertions.False(after.Routes.Items[0].MembershipComplete)
	assertions.Contains(after.Routes.Items[0].Reasons, "membership_incomplete")
}

func TestResumedTailScanSkipsChatsAlreadyConfirmedGone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	now := time.Now().UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{
		ID: "!surviving:example.test", AccountID: "account-a", Type: "group", Title: "Surviving",
		LastActivity: now, Participants: []map[string]any{{"id": "@self:example.test", "isSelf": true}},
	})
	provider := f.handler()
	var mu sync.Mutex
	probes := map[string]int{}
	blockNextProbe := true
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatID, isProbe := strings.CutPrefix(r.URL.Path, "/v1/chats/!gone-")
		if !isProbe {
			provider(w, r)
			return
		}
		mu.Lock()
		probes[chatID]++
		block := blockNextProbe && probes[chatID] == 1 && len(probes) == 2
		mu.Unlock()
		if block {
			// The second distinct probe of the first run outlives its budget.
			<-r.Context().Done()
			return
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	client := NewClient("http://beeper.test", testToken, 10000)
	client.http.Transport = handlerTransport(handler)

	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	state := NewSyncState()
	state.TailScanStarted = tailScanCycleID(now)
	for _, chatID := range []string{"!gone-a:example.test", "!gone-b:example.test", "!gone-c:example.test"} {
		state.EnsureChat(chatID).Done = true
	}
	imp := NewImporter(st, client)
	enumerate := func(budget time.Duration) ([]chatVisit, *ImportSummary) {
		syncID, err := st.StartSync(source.ID, sourceTypeBeeper)
		requirements.NoError(err)
		sum := &ImportSummary{}
		opts := ImportOptions{AccountID: "account-a", NoMedia: true, StopAt: time.Now().Add(budget)}
		chats, err := imp.enumerateChats(t.Context(), syncID, source.ID, opts, state, now.Add(-reconcileWindow), true, sum)
		requirements.NoError(err)
		requirements.NoError(st.CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, source.ID, ""))
		return chats, sum
	}

	_, first := enumerate(2 * time.Second)
	assertions.True(first.Stopped, "the blocked probe exhausts the first run's budget")
	mu.Lock()
	blockNextProbe = false
	confirmed := map[string]bool{}
	for chatID, count := range probes {
		if state.Chats["!gone-"+chatID].Gone {
			confirmed[chatID] = true
			assertions.Equal(1, count)
		}
	}
	mu.Unlock()
	requirements.Len(confirmed, 1, "exactly one chat was confirmed gone before the budget expired")

	chats, second := enumerate(time.Minute)
	assertions.False(second.Stopped)
	requirements.Len(chats, 1)
	assertions.Equal("!surviving:example.test", chats[0].ID)
	mu.Lock()
	defer mu.Unlock()
	for chatID := range confirmed {
		assertions.Equal(1, probes[chatID], "a chat confirmed gone is not probed again")
	}
	for _, chatID := range []string{"!gone-a:example.test", "!gone-b:example.test", "!gone-c:example.test"} {
		assertions.True(state.Chats[chatID].Gone)
	}
}
