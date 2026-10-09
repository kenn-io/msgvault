package mcp

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestContactRouteCatalogAndReads(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	var routePeer int64
	for i, address := range []string{"avery-one@example.test", "avery-two@example.test"} {
		pid, err := st.EnsureParticipantByIdentifier("email", address, "Avery Example")
		requirements.NoError(err)
		if i == 0 {
			routePeer = pid
		}
		person, _, err := st.CreatePersonFromParticipant(pid)
		requirements.NoError(err)
		if i == 0 {
			_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			requirements.NoError(err)
		}
	}
	source, err := st.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "!direct:example.test", "direct_chat", "Avery Example")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(conv, routePeer, "member"))
	_, err = st.DB().Exec(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`, source.ID)
	requirements.NoError(err)
	requirements.NoError(st.SetConversationMessagingRouteEvidence(t.Context(), conv, store.MessagingRouteEvidence{ChatID: "!direct:example.test", AccountID: "account-a", Network: "whatsapp", ProviderType: "single", ObservedAt: time.Now().UTC(), MembershipComplete: true, ParticipantIDs: []int64{routePeer}, SelfParticipantIDs: []int64{}, MemberChatIDs: []string{}}))
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	daemonHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		daemon.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(daemonHTTP.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemonHTTP.URL, AllowInsecure: true})
	requirements.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, ContactRoutesBackend: client}
	listed := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(listed, "find_contact_candidates")
	requirements.Contains(listed, "get_person_messaging_routes")
	assertions.Equal(true, toolReadOnlyHint(t, listed["find_contact_candidates"]))
	result := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "find_contact_candidates", "arguments": map[string]any{"query": "Avery", "limit": 1}})
	page := toolStructuredContent(t, result.Result)
	assertions.Equal(true, page["ambiguous"])
	assertions.Equal(true, page["has_more"])
	candidates, ok := page["candidates"].([]any)
	requirements.True(ok)
	requirements.Len(candidates, 1)
	candidate, ok := candidates[0].(map[string]any)
	requirements.True(ok)
	uid, ok := candidate["person_uid"].(string)
	requirements.True(ok)
	defaultPage, err := client.GetPersonMessagingRoutesContext(t.Context(), store.PersonMessagingRouteQuery{PersonUID: uid})
	requirements.NoError(err)
	requirements.NotNil(defaultPage)
	routes := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "get_person_messaging_routes", "arguments": map[string]any{"person_uid": uid}})
	routePage := toolStructuredContent(t, routes.Result)
	assertions.Equal(uid, routePage["person_uid"])
	assertions.Equal("archive_only", routePage["freshness"])
	routeJSON, err := json.Marshal(routePage)
	requirements.NoError(err)
	var decoded store.PersonMessagingRoutesPage
	requirements.NoError(json.Unmarshal(routeJSON, &decoded))
	requirements.Len(decoded.Routes.Items, 1)
	requirements.Len(decoded.ContactPoints.Items, 1)
	assertions.Equal("+12025550123", decoded.ContactPoints.Items[0].OriginalValue)
	assertions.Equal("archive_verified", decoded.Routes.Items[0].Status)
	assertions.Equal("!direct:example.test", decoded.Routes.Items[0].ProviderChatID)
	assertions.Equal("account-a", decoded.Routes.Items[0].AccountID)

	var before, after int64
	requirements.NoError(st.DB().QueryRow(`SELECT total_changes()`).Scan(&before))
	noMatches := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "find_contact_candidates", "arguments": map[string]any{"query": "No Such Person"}})
	assertions.NotEqual(true, noMatches.Result["isError"])
	data, err := json.Marshal(toolStructuredContent(t, noMatches.Result))
	requirements.NoError(err)
	assertions.Contains(string(data), `"candidates":[]`)
	requirements.NoError(st.DB().QueryRow(`SELECT total_changes()`).Scan(&after))
	assertions.Equal(before, after)
	missing := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "get_person_messaging_routes", "arguments": map[string]any{"person_uid": "not-a-person"}})
	assertions.Equal(true, missing.Result["isError"])
	assertions.Contains(toolErrorTextFromResult(t, missing.Result), "person_not_found")
	requirements.NoError(st.Close())
	failed := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "find_contact_candidates", "arguments": map[string]any{"query": "Avery"}})
	assertions.Equal(true, failed.Result["isError"])
	assertions.Contains(toolErrorTextFromResult(t, failed.Result), "contact_lookup_unavailable")
}

func TestContactRouteCatalogMissingBackendAndDelegated(t *testing.T) {
	for _, opts := range []ServeOptions{
		{Engine: &querytest.MockEngine{}},
		{Engine: &querytest.MockEngine{}, ContactRoutesBackend: testutil.NewSQLiteTestStore(t), DelegatedOnly: true},
	} {
		listed := toolsByName(t, rawListTools(t, opts, false))
		assert.NotContains(t, listed, "find_contact_candidates")
		assert.NotContains(t, listed, "get_person_messaging_routes")
	}
}
