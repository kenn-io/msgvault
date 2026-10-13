package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Observe actual SerialOperationGate admission without replacing native gate
// acquisition or Store authorization, transactions, identity effects or receipts.
type identityAdmissionGate struct {
	*SerialOperationGate

	admissions int
	onAdmitted func()
}

func (g *identityAdmissionGate) BeginRequestWorkContext(ctx context.Context, label string) (func(), bool) {
	release, ok := g.SerialOperationGate.BeginRequestWorkContext(ctx, label)
	if ok {
		g.admissions++
		if g.onAdmitted != nil {
			g.onAdmitted()
		}
	}
	return release, ok
}

func TestNativeIdentityOperationsRechecksRevocationAfterGateAdmission(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-selected@example.test")
	requirements.NoError(err)
	a, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	b, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	thread, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
	requirements.NoError(err)
	for i, participant := range []int64{a, b} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: thread, SourceMessageID: []string{"synthetic-first", "synthetic-second"}[i], MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
		requirements.NoError(err)
	}
	gate := &identityAdmissionGate{SerialOperationGate: NewSerialOperationGate()}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	id, secret, _, err := srv.agentGrants.Issue("Synthetic selected account", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	w := identityNativeRequest(t, srv, http.MethodPost, identityOperationsPath+"/preview", secret, true, intent)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var preview identityOperationPreviewResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &preview))
	requirements.NotNil(preview.Snapshot)
	assertions.Zero(gate.admissions, "preview reads must not acquire the mutation gate")
	revoked := false
	gate.onAdmitted = func() { revoked = srv.agentGrants.Revoke(id) }
	key := "synthetic-revoked-after-gate"
	body := identityOperationApplyRequest{Operation: intent.Operation, Target: intent.Target, ExpectedFingerprint: preview.Snapshot.Fingerprint, PreviewToken: preview.PreviewToken, IdempotencyKey: key}
	w = identityNativeRequest(t, srv, http.MethodPost, identityOperationsPath+"/apply", secret, true, body)
	requirements.Equal(http.StatusForbidden, w.Code, w.Body.String())
	assertions.True(revoked, "revocation occurs after actual gate admission")
	assertions.Equal(1, gate.admissions)
	assertions.False(gate.HasRequestWaiters())
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Equal(preview.Snapshot.Fingerprint, current.Fingerprint)
	assertions.Empty(current.Links)
	_, err = st.IdentityOperationReceiptContext(t.Context(), id, key)
	requirements.ErrorIs(err, store.ErrIdentityOperationReceiptNotFound)
	release, ok := gate.BeginWorkContext(t.Context())
	requirements.True(ok, "denied write must release its actual gate hold")
	release()
}

func TestNativeIdentityOperationsForeignAndRevokedCallersDoNotObserveBusyGate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
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
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "Synthetic private holder")
	requirements.True(ok)
	defer release()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger(), OperationGate: gate})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	id, secret, _, err := srv.agentGrants.Issue("Synthetic selected account", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, []agentgrant.SourceRef{{ID: selected.ID, Type: selected.SourceType, Identifier: selected.Identifier}}, time.Time{})
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}}
	before, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	key := "synthetic-denied-busy-gate"
	apply := identityOperationApplyRequest{Operation: intent.Operation, Target: intent.Target, ExpectedFingerprint: before.Fingerprint, PreviewToken: "synthetic-unapproved-preview", IdempotencyKey: key}
	for _, methodAndBody := range []struct {
		path string
		body any
	}{{"/preview", intent}, {"/apply", apply}} {
		w := identityNativeRequest(t, srv, http.MethodPost, identityOperationsPath+methodAndBody.path, secret, true, methodAndBody.body)
		assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), "Synthetic private holder")
		assertions.NotContains(w.Body.String(), "synthetic-second@example.test")
		assertions.False(gate.HasRequestWaiters())
	}
	requirements.True(srv.agentGrants.Revoke(id))
	for _, denied := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/preview", intent},
		{http.MethodPost, "/apply", apply},
		{http.MethodGet, "/receipt?idempotency_key=" + key, nil},
	} {
		w := identityNativeRequest(t, srv, denied.method, identityOperationsPath+denied.path, secret, true, denied.body)
		assertions.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), "Synthetic private holder")
		assertions.False(gate.HasRequestWaiters())
	}
	after, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Equal(before.Fingerprint, after.Fingerprint)
	assertions.Empty(after.Links)
	_, err = st.IdentityOperationReceiptContext(t.Context(), id, key)
	assertions.ErrorIs(err, store.ErrIdentityOperationReceiptNotFound)
}
