package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/msgvault/internal/agentgrant"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func identityNativeRequest(t *testing.T, srv *Server, method, path, credential string, delegated bool, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if delegated {
		r.Header.Set("X-Msgvault-Agent-Token", credential)
	} else {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestNativeIdentityOperationsOwnerPreviewApplyAndReadback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	w := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", agentTokenTestAPIKey, false, intent)
	requirements.Equal(http.StatusOK, w.Code, "%s", w.Body.String())
	var preview struct {
		Snapshot  store.IdentitySnapshot `json:"snapshot"`
		Token     string                 `json:"preview_token"`
		ExpiresAt time.Time              `json:"expires_at"`
	}
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &preview))
	requirements.NotEmpty(preview.Token)
	requirements.True(preview.ExpiresAt.After(time.Now()))
	before, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Empty(before.Links, "preview must not link the graph")
	body := map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-owner-link"}
	w = identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	requirements.Equal(http.StatusOK, w.Code, "%s", w.Body.String())
	var receipt store.IdentityReceipt
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &receipt))
	assertions.True(receipt.Changed)
	requirements.NotEmpty(receipt.ID)
	replay := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	requirements.Equal(http.StatusOK, replay.Code, "%s", replay.Body.String())
	var saved store.IdentityReceipt
	requirements.NoError(json.Unmarshal(replay.Body.Bytes(), &saved))
	assertions.Equal(receipt, saved)
	body["preview_token"] = "expired-or-missing-synthetic-preview"
	replay = identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	requirements.Equal(http.StatusOK, replay.Code, "%s", replay.Body.String())
	requirements.NoError(json.Unmarshal(replay.Body.Bytes(), &saved))
	assertions.Equal(receipt, saved, "current owner can recover an exact committed retry after preview expiry")
	read := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/identity/operations/receipt?idempotency_key=synthetic-owner-link", agentTokenTestAPIKey, false, nil)
	requirements.Equal(http.StatusOK, read.Code, "%s", read.Body.String())
	requirements.NoError(json.Unmarshal(read.Body.Bytes(), &saved))
	assertions.Equal(receipt, saved)
	restarted := NewServer(srv.cfg, st, nil, testLogger())
	read = identityNativeRequest(t, restarted, http.MethodGet, "/api/v1/identity/operations/receipt?receipt_id="+receipt.ID, agentTokenTestAPIKey, false, nil)
	requirements.Equal(http.StatusOK, read.Code, "%s", read.Body.String())
	requirements.NoError(json.Unmarshal(read.Body.Bytes(), &saved))
	assertions.Equal(receipt, saved)
}

func TestNativeIdentityOperationsDelegatedScopeAndCurrentRevocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
	requirements.NoError(err)
	for i, member := range []int64{a, b} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: []string{"synthetic-first", "synthetic-second"}[i], MessageType: "email", SenderID: sql.NullInt64{Int64: member, Valid: true}})
		requirements.NoError(err)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	ref := agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}
	id, secret, _, err := srv.agentGrants.Issue("Synthetic identity grant", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, []agentgrant.SourceRef{ref})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", secret, true, intent)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var preview struct {
		Snapshot store.IdentitySnapshot `json:"snapshot"`
		Token    string                 `json:"preview_token"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	body := map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-delegate-link"}
	bad := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	assertions.Equal(http.StatusConflict, bad.Code, "owner cannot reuse a delegate's preview")
	good := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", secret, true, body)
	requirements.Equal(http.StatusOK, good.Code, "%s", good.Body.String())
	requirements.True(srv.agentGrants.Revoke(id))
	revoked := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/identity/operations/receipt?idempotency_key=synthetic-delegate-link", secret, true, nil)
	assertions.Equal(http.StatusUnauthorized, revoked.Code)
	recovered := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/identity/operations/receipt?principal="+id+"&idempotency_key=synthetic-delegate-link", agentTokenTestAPIKey, false, nil)
	requirements.Equal(http.StatusOK, recovered.Code, "%s", recovered.Body.String())
}

func TestNativeIdentityOperationsRejectsUnscopedEndpoint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	selected, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("gmail", "synthetic-other@example.test")
	requirements.NoError(err)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	for i, source := range []*store.Source{selected, other} {
		thread, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
		requirements.NoError(err)
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: thread, SourceMessageID: "synthetic-message", MessageType: "email", SenderID: sql.NullInt64{Int64: []int64{a, b}[i], Valid: true}})
		requirements.NoError(err)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	_, secret, _, err := srv.agentGrants.Issue("Synthetic selected account", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, []agentgrant.SourceRef{{ID: selected.ID, Type: selected.SourceType, Identifier: selected.Identifier}})
	requirements.NoError(err)
	response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", secret, true, identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}})
	assertions.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
	assertions.NotContains(response.Body.String(), "synthetic-second@example.test", "denied preview must not disclose native evidence")
}

func TestNativeIdentityOperationsRequireExactDurablePersonScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	selected, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	a, err := st.EnsureParticipant("synthetic-curated@example.test", "Synthetic Curated", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-message@example.test", "Synthetic Message", "example.test")
	requirements.NoError(err)
	thread, err := st.EnsureConversation(selected.ID, "synthetic-thread", "Synthetic Thread")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: selected.ID, ConversationID: thread, SourceMessageID: "synthetic-message", MessageType: "email", SenderID: sql.NullInt64{Int64: b, Valid: true}})
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	ref := agentgrant.SourceRef{ID: selected.ID, Type: selected.SourceType, Identifier: selected.Identifier}
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationPersonLink, Target: identitycontrol.IdentityTarget{ParticipantID: b, PersonID: person.ID}}
	for _, uid := range []string{"", "synthetic-wrong-uid", person.VCardUID} {
		scope := agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{ref}}
		if uid != "" {
			scope.Persons = []agentgrant.PersonRef{{ID: person.ID, UID: uid}}
		}
		_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic explicit person", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, scope)
		requirements.NoError(err)
		response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", secret, true, intent)
		if uid != person.VCardUID {
			assertions.Equal(http.StatusForbidden, response.Code)
			continue
		}
		requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
		var preview struct {
			Snapshot store.IdentitySnapshot `json:"snapshot"`
			Token    string                 `json:"preview_token"`
		}
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
		applied := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", secret, true, map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-person-attach"})
		requirements.Equal(http.StatusOK, applied.Code, "%s", applied.Body.String())
		after, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
		requirements.NoError(err)
		assertions.Contains(after.Bindings, store.IdentityBindingEvidence{ParticipantID: b, PersonID: person.ID})
	}
}

func TestNativeIdentityOperationsRechecksNewSourceOccurrenceAtApply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	selected, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("gmail", "synthetic-other@example.test")
	requirements.NoError(err)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	thread, err := st.EnsureConversation(selected.ID, "synthetic-selected-thread", "Synthetic Thread")
	requirements.NoError(err)
	for i, member := range []int64{a, b} {
		_, err = st.UpsertMessage(&store.Message{SourceID: selected.ID, ConversationID: thread, SourceMessageID: []string{"synthetic-first", "synthetic-second"}[i], MessageType: "email", SenderID: sql.NullInt64{Int64: member, Valid: true}})
		requirements.NoError(err)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	id, secret, _, err := srv.agentGrants.Issue("Synthetic explicit source", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, []agentgrant.SourceRef{{ID: selected.ID, Type: selected.SourceType, Identifier: selected.Identifier}})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", secret, true, intent)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var preview struct {
		Snapshot store.IdentitySnapshot `json:"snapshot"`
		Token    string                 `json:"preview_token"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	otherThread, err := st.EnsureConversation(other.ID, "synthetic-other-thread", "Synthetic Thread")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: otherThread, SourceMessageID: "synthetic-new-occurrence", MessageType: "email", SenderID: sql.NullInt64{Int64: b, Valid: true}})
	requirements.NoError(err)
	applied := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", secret, true, map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-changed-source"})
	assertions.Equal(http.StatusForbidden, applied.Code, "%s", applied.Body.String())
	_, err = st.IdentityOperationReceiptContext(t.Context(), id, "synthetic-changed-source")
	requirements.ErrorIs(err, store.ErrIdentityOperationReceiptNotFound)
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Empty(current.Links, "out-of-grant occurrence must prevent the mutation")
}

// Only the external cache refresher fails; every identity and receipt operation
// executes against the real native Store.
type identityOperationFailedCacheStore struct {
	*store.Store

	refreshCalls int
}

func (s *identityOperationFailedCacheStore) RefreshIdentityDatasets(context.Context) (int64, error) {
	s.refreshCalls++
	return 0, errors.New("synthetic external cache unavailable")
}

func TestNativeIdentityOperationsCacheFailureKeepsCommittedReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	backend := &identityOperationFailedCacheStore{Store: st}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}}, backend, nil, testLogger())
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", agentTokenTestAPIKey, false, intent)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var preview struct {
		Snapshot store.IdentitySnapshot `json:"snapshot"`
		Token    string                 `json:"preview_token"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	assertions.Equal(0, backend.refreshCalls, "preview must not refresh the cache")
	body := map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-cache-failure"}
	applied := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	requirements.Equal(http.StatusOK, applied.Code, "%s", applied.Body.String())
	var outcome struct {
		store.IdentityReceipt

		CacheState string `json:"cache_state"`
	}
	requirements.NoError(json.Unmarshal(applied.Body.Bytes(), &outcome))
	assertions.True(outcome.Changed)
	assertions.Equal("stale", outcome.CacheState)
	assertions.Equal(1, backend.refreshCalls)
	saved, err := st.IdentityOperationReceiptContext(t.Context(), "owner", "synthetic-cache-failure")
	requirements.NoError(err)
	assertions.Equal(outcome.IdentityReceipt, *saved)
	replay := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/apply", agentTokenTestAPIKey, false, body)
	requirements.Equal(http.StatusOK, replay.Code, "%s", replay.Body.String())
	requirements.NoError(json.Unmarshal(replay.Body.Bytes(), &outcome))
	assertions.Equal("unknown", outcome.CacheState, "exact retry does not claim current cache publication")
	assertions.Equal(1, backend.refreshCalls, "receipt replay must not launch another cache build")
}

func TestNativeIdentityOperationsDeniedWriteDoesNotEnterBusyGate(t *testing.T) {
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-read-only@example.test")
	requirements.NoError(err)
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "Synthetic competing writer")
	requirements.True(ok)
	defer release()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	_, secret, _, err := srv.agentGrants.Issue("Synthetic identity read only", []agentgrant.Permission{agentgrant.PermissionIdentityRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}})
	requirements.NoError(err)
	data, err := json.Marshal(map[string]any{"operation": identitycontrol.OperationGraphLink, "target": identitycontrol.IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}, "expected_fingerprint": strings.Repeat("a", 64), "preview_token": "synthetic-unapproved", "idempotency_key": "synthetic-denied"})
	requirements.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/identity/operations/apply", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Msgvault-Agent-Token", secret)
	// A cancelled context makes a wait fail immediately, without a throughput
	// assertion. Permission denial must happen before any attempt to wait.
	cancelled, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(cancelled)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	requirements.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
	assert.NotContains(t, response.Body.String(), "busy")
}

func TestNativeIdentityOperationsRequiresReadPermissionBeforeLookup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-unrelated-permission@example.test")
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	for _, permission := range []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionIdentityLink} {
		_, secret, _, err := srv.agentGrants.Issue("Synthetic no identity read", []agentgrant.Permission{permission}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}})
		requirements.NoError(err)
		response := identityNativeRequest(t, srv, http.MethodPost, "/api/v1/identity/operations/preview", secret, true, identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}})
		assertions.Equal(http.StatusForbidden, response.Code, "%s", response.Body.String())
		assertions.NotContains(response.Body.String(), "not_found", "caller without read authority must not learn target existence")
	}
}
