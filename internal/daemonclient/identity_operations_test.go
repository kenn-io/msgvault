package daemonclient_test

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestNativeIdentityOperationClientPreviewApplyAndReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	key := strings.Repeat("c", 64)
	router := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: key}}, st, nil, slog.New(slog.DiscardHandler)).Router()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: key, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	preview, err := client.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	requirements.NotNil(preview)
	assertions.Empty(preview.Snapshot.Links)
	body := generated.IdentityOperationApplyRequest{
		Operation:           generated.GraphLink,
		Target:              generated.IdentityTarget{ParticipantID: first, OtherParticipantID: &second},
		ExpectedFingerprint: preview.Snapshot.Fingerprint,
		PreviewToken:        preview.PreviewToken,
		IdempotencyKey:      "synthetic-client-link",
	}
	applied, err := client.ApplyIdentityOperation(t.Context(), body)
	requirements.NoError(err)
	requirements.NotNil(applied)
	assertions.True(applied.Changed)
	saved, err := client.GetIdentityOperationReceipt(t.Context(), body.IdempotencyKey)
	requirements.NoError(err)
	requirements.NotNil(saved)
	assertions.Equal(applied.ID, saved.ID)
	assertions.Equal(applied.AfterFingerprint, saved.AfterFingerprint)
	byID, err := client.GetIdentityOperationReceiptByID(t.Context(), saved.ID)
	requirements.NoError(err)
	assertions.Equal(saved, byID)
	body.PreviewToken = "expired-preview"
	retried, err := client.ApplyIdentityOperation(t.Context(), body)
	requirements.NoError(err)
	assertions.Equal(applied.ID, retried.ID)
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Len(current.Links, 1)
}

func TestNativeIdentityOperationClientDelegatedRevocationAndOwnerRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	otherSource, err := st.GetOrCreateSource("gmail", "synthetic-other@example.test")
	requirements.NoError(err)
	participants := make([]int64, 0, 3)
	for index, email := range []string{"synthetic-first@example.test", "synthetic-second@example.test", "synthetic-foreign@example.test"} {
		member, err := st.EnsureParticipant(email, "Synthetic Participant", "example.test")
		requirements.NoError(err)
		participants = append(participants, member)
		selectedSource := source
		if index == 2 {
			selectedSource = otherSource
		}
		thread, err := st.EnsureConversation(selectedSource.ID, "synthetic-thread", "Synthetic Thread")
		requirements.NoError(err)
		_, err = st.UpsertMessage(&store.Message{SourceID: selectedSource.ID, ConversationID: thread, SourceMessageID: email, MessageType: "email", SenderID: sql.NullInt64{Int64: member, Valid: true}})
		requirements.NoError(err)
	}
	key := strings.Repeat("c", 64)
	router := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: key, AgentAccess: true}}, st, nil, slog.New(slog.DiscardHandler)).Router()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: key, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	grant, err := owner.IssueAgentToken(t.Context(), "Synthetic identity scope", []string{"identity.read", "identity.link"}, []int64{source.ID}, nil)
	requirements.NoError(err)
	delegate, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: participants[0], OtherParticipantID: participants[1]}}
	foreign := intent
	foreign.Target.OtherParticipantID = participants[2]
	_, err = delegate.PreviewIdentityOperation(t.Context(), foreign)
	var apiError *daemonclient.APIError
	requirements.ErrorAs(err, &apiError)
	assertions.Equal(http.StatusForbidden, apiError.Status)
	assertions.NotContains(err.Error(), "synthetic-foreign@example.test")
	preview, err := delegate.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	request := generated.IdentityOperationApplyRequest{Operation: generated.GraphLink,
		Target:              generated.IdentityTarget{ParticipantID: participants[0], OtherParticipantID: &participants[1]},
		ExpectedFingerprint: preview.Snapshot.Fingerprint, PreviewToken: preview.PreviewToken, IdempotencyKey: "synthetic-delegate-recovery"}
	receipt, err := delegate.ApplyIdentityOperation(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	_, err = delegate.GetIdentityOperationReceipt(t.Context(), request.IdempotencyKey)
	requirements.ErrorAs(err, &apiError)
	assertions.Equal(http.StatusUnauthorized, apiError.Status)
	_, err = delegate.ApplyIdentityOperation(t.Context(), request)
	requirements.ErrorAs(err, &apiError)
	assertions.Equal(http.StatusUnauthorized, apiError.Status)
	saved, err := owner.GetIdentityOperationReceiptForPrincipal(t.Context(), grant.ID, request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Equal(receipt.ID, saved.ID)
}

func TestNativeIdentityOperationClientRecoversDroppedCommittedResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	key := strings.Repeat("c", 64)
	router := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: key}}, st, nil, slog.New(slog.DiscardHandler)).Router()
	var applies atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/identity/operations/apply" {
			router.ServeHTTP(w, r)
			return
		}
		applies.Add(1)
		// Execute the production route and commit its real transaction, then
		// lose only the acknowledgement at the actual TCP boundary.
		result := httptest.NewRecorder()
		router.ServeHTTP(result, r)
		assert.Equal(t, http.StatusOK, result.Code, "%s", result.Body.String())
		hijacker, ok := w.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		connection, _, err := hijacker.Hijack()
		if !assert.NoError(t, err) {
			return
		}
		assert.NoError(t, connection.Close())
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: key, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	preview, err := client.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	request := generated.IdentityOperationApplyRequest{Operation: generated.GraphLink,
		Target:              generated.IdentityTarget{ParticipantID: first, OtherParticipantID: &second},
		ExpectedFingerprint: preview.Snapshot.Fingerprint, PreviewToken: preview.PreviewToken, IdempotencyKey: "synthetic-lost-response"}
	outcome, err := client.ApplyIdentityOperation(t.Context(), request)
	requirements.Error(err)
	assertions.Nil(outcome)
	var unknown *daemonclient.IdentityOutcomeUnknownError
	requirements.ErrorAs(err, &unknown, "lost acknowledgement must expose a recoverable unknown outcome")
	assertions.Equal(request.IdempotencyKey, unknown.IdempotencyKey)
	assertions.Equal(int64(1), applies.Load(), "client must not replay an unacknowledged write")
	saved, err := client.GetIdentityOperationReceipt(t.Context(), request.IdempotencyKey)
	requirements.NoError(err)
	assertions.True(saved.Changed)
	assertions.Equal(int64(1), applies.Load(), "receipt lookup must not resubmit the mutation")
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Len(current.Links, 1)
}

func TestNativeIdentityOperationClientRefusesRedirectedWrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	key := strings.Repeat("c", 64)
	router := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: key}}, st, nil, slog.New(slog.DiscardHandler)).Router()
	var redirected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/identity/operations/apply":
			http.Redirect(w, r, "/synthetic-redirected-apply", http.StatusTemporaryRedirect)
		case "/synthetic-redirected-apply":
			redirected.Add(1)
			r.URL.Path = "/api/v1/identity/operations/apply"
			router.ServeHTTP(w, r)
		default:
			router.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: key, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	preview, err := client.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	_, err = client.ApplyIdentityOperation(t.Context(), generated.IdentityOperationApplyRequest{Operation: generated.GraphLink,
		Target:              generated.IdentityTarget{ParticipantID: first, OtherParticipantID: &second},
		ExpectedFingerprint: preview.Snapshot.Fingerprint, PreviewToken: preview.PreviewToken, IdempotencyKey: "synthetic-redirect"})
	requirements.Error(err)
	assertions.Zero(redirected.Load())
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Empty(current.Links, "redirect must not forward a reviewed mutation to another endpoint")
}
