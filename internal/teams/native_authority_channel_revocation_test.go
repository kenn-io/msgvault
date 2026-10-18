package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeTeamsChannelRevocationFeed struct {
	mu       sync.Mutex
	failure  string
	messages []map[string]any
}

func (f *nativeTeamsChannelRevocationFeed) isFailing(point string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failure == point
}

func (f *nativeTeamsChannelRevocationFeed) setFailure(point string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = point
}

func newNativeTeamsChannelRevocationFeed(t *testing.T, failure string) (*nativeTeamsChannelRevocationFeed, *Importer, ImportOptions) {
	t.Helper()
	f := &nativeTeamsChannelRevocationFeed{failure: failure}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me":
			_, _ = w.Write([]byte(`{"id":"synthetic-owner-id","mail":"owner@example.test","userPrincipalName":"owner@example.test"}`))
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"event-chat","chatType":"group","topic":"Synthetic event chat"}]}`))
		case "/chats/event-chat/members":
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-owner-id","userId":"synthetic-owner-id","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
		case "/me/chats/event-chat/messages":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"value": f.messages})
		case "/me/joinedTeams":
			_, _ = w.Write([]byte(`{"value":[{"id":"team-1","displayName":"Synthetic Team"}]}`))
		case "/teams/team-1/channels":
			_, _ = w.Write([]byte(`{"value":[{"id":"channel-1","displayName":"Synthetic Channel","membershipType":"standard"}]}`))
		case "/teams/team-1/members":
			if f.isFailing("team_roster") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-owner-id","userId":"synthetic-owner-id","email":"owner@example.test","displayName":"Synthetic Owner"}]}`))
		case "/teams/team-1/channels/channel-1/messages":
			if f.isFailing("channel_messages") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.isFailing("channel_scope_denied") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"root-1","createdDateTime":"2026-01-01T00:00:00Z","lastModifiedDateTime":"2026-01-01T00:00:00Z","body":{"contentType":"text","content":"Synthetic channel root"}}]}`))
		case "/teams/team-1/channels/channel-1/messages/root-1/replies":
			if f.isFailing("channel_replies") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"value":[]}`))
		case "/teams/team-1/channels/channel-1/messages/delta":
			if f.isFailing("channel_delta") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "@odata.deltaLink": "http://" + r.Host + "/delta?token=next"})
		default:
			if strings.HasPrefix(r.URL.Path, "/delta") {
				if f.isFailing("channel_delta_incremental") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"value":[]}`))
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.source_message_archived", SourceType: "teams", Kinds: []string{"message"}},
	}})
	Require.NoError(t, err)
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50, WithGrantedScopesReader(func() ([]string, error) { return []string{"Chat.Read"}, nil }))
	return f, NewImporter(st, client), ImportOptions{Email: "owner@example.test"}
}

func activateNativeTeamsSourceReceipt(t *testing.T, imp *Importer, sourceID int64) (*store.MCPSubscription, time.Time) {
	t.Helper()
	require := Require.New(t)
	resource, err := imp.store.GetMCPResource(t.Context(), "source", sourceID)
	require.NoError(err)
	arguments, err := json.Marshal(map[string]any{
		"source_id":         strconv.FormatInt(sourceID, 10),
		"source_ref":        resource.Reference,
		"include_from_me":   false,
		"include_reactions": true,
	})
	require.NoError(err)
	now := time.Now().UTC()
	sub := &store.MCPSubscription{
		ID: "synthetic-teams-source-receipt", Principal: "synthetic-owner", Name: "msgvault.source_message_archived",
		Arguments: arguments, ScopeKind: "source", ScopeID: sourceID, SourceID: sourceID,
		CallbackURL: "https://receiver.example.test/teams", SecretEnc: []byte("synthetic-encrypted-secret"),
		SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(imp.store.BindMCPSubscriptionScope(t.Context(), sub))
	active, _, err := imp.store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: *sub, Now: now})
	require.NoError(err)
	require.NotNil(active)
	return active, now
}

func TestNativeTeamsChannelAuthenticationRevocationRetiresSourceDeliveryAndReceipts(t *testing.T) {
	for _, failure := range []string{"team_roster", "channel_messages", "channel_replies", "channel_delta", "channel_delta_incremental", "channel_scope_denied"} {
		t.Run(failure, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			initialFailure := failure
			if failure == "channel_delta_incremental" {
				initialFailure = ""
			}
			feed, imp, opts := newNativeTeamsChannelRevocationFeed(t, initialFailure)
			feed.messages = []map[string]any{nativeTeamsEventMessage("history", "2026-01-01T00:00:00Z", "Synthetic historical message", "sender@example.test")}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			sub, now := activateNativeTeamsSourceReceipt(t, imp, sum.SourceID)

			feed.mu.Lock()
			feed.messages = append(feed.messages, nativeTeamsEventMessage("live", "2026-01-02T00:00:00Z", "Synthetic live message", "sender@example.test"))
			feed.mu.Unlock()
			liveOpts := opts
			if failure == "channel_delta_incremental" {
				liveOpts.IncludeChannels = true
			}
			_, err = imp.Import(t.Context(), liveOpts)
			require.NoError(err)
			pending, err := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic-teams-event"}`), nil
			})
			require.NoError(err)
			require.NotNil(pending)
			_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
			require.NoError(err, "the live Teams source receipt is readable before channel revocation")
			var body string
			require.NoError(imp.store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, pending.Event.MessageID, now, func(tx *sql.Tx) error {
				return tx.QueryRow(imp.store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), pending.Event.MessageID).Scan(&body)
			}))
			require.Equal("Synthetic live message", body)

			channelOpts := opts
			channelOpts.IncludeChannels = true
			if failure == "channel_delta_incremental" {
				feed.setFailure(failure)
			}
			_, err = imp.Import(t.Context(), channelOpts)
			if failure == "channel_scope_denied" {
				require.NoError(err, "channel-specific 403 remains local to that channel")
				allowed, permissionErr := imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
				require.NoError(permissionErr)
				assert.True(allowed, "channel-specific denial does not revoke the account's Events grant")
				_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now.Add(time.Second))
				require.NoError(err, "channel-specific denial leaves the retained receipt available")
				return
			}
			assert.Error(err, "account authentication failures during %s must reach the import finalizer", failure)
			allowed, permissionErr := imp.store.NativeSourceMessagePermission(t.Context(), sum.SourceID)
			require.NoError(permissionErr)
			assert.False(allowed, "channel authentication failure must revoke the persisted native Events grant")

			next, deliveryErr := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Second), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic-teams-event"}`), nil
			})
			require.NoError(deliveryErr)
			assert.Nil(next, "retained pending delivery cannot pass a revoked native source grant")
			_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now.Add(time.Second))
			assert.Error(err, "retained source receipt must be inaccessible after account revocation")
			readCalled := false
			err = imp.store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, pending.Event.MessageID, now.Add(time.Second), func(*sql.Tx) error {
				readCalled = true
				return nil
			})
			assert.Error(err, "retained receipt must not authorize archived message content after account revocation")
			assert.False(readCalled, "revoked receipt must be rejected before message content is read")
		})
	}
}
