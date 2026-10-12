package mcp

import (
	"encoding/json/jsontext"
	"fmt"
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
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type identityMCPFixture struct {
	client *daemonclient.Client
	store  *store.Store
	first  int64
	second int64
}

func nativeIdentityMCPFixture(t *testing.T, decorate ...func(http.Handler) http.Handler) identityMCPFixture {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("synthetic-first@example.test", "Synthetic First", "example.test")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("synthetic-second@example.test", "Synthetic Second", "example.test")
	requirements.NoError(err)
	key := strings.Repeat("e", 64)
	router := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: key, AgentAccess: true}}, st, nil, slog.New(slog.DiscardHandler)).Router()
	if len(decorate) > 0 {
		router = decorate[0](router)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: key, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	return identityMCPFixture{client: client, store: st, first: first, second: second}
}

func TestNativeIdentityMCPPreservesUnknownOutcomeAndReadOnlyRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var attempts atomic.Int64
	fixture := nativeIdentityMCPFixture(t, func(router http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/identity/operations/apply" {
				router.ServeHTTP(w, r)
				return
			}
			attempts.Add(1)
			result := httptest.NewRecorder()
			router.ServeHTTP(result, r)
			assertions.Equal(http.StatusOK, result.Code, "%s", result.Body.String())
			hijacker, ok := w.(http.Hijacker)
			if !assertions.True(ok) {
				return
			}
			connection, _, err := hijacker.Hijack()
			if !assertions.NoError(err) {
				return
			}
			assertions.NoError(connection.Close())
		})
	})
	backend, first, second := fixture.client, fixture.first, fixture.second
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend, AllowIdentityDecisions: true}
	preview, err := backend.PreviewIdentityOperation(t.Context(), identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}})
	requirements.NoError(err)
	result := confirmedCallTool(t, options, "link_participant_identity", map[string]any{
		"participant_id": first, "other_participant_id": second, "expected_fingerprint": preview.Snapshot.Fingerprint,
		"preview_token": preview.PreviewToken, "idempotency_key": "synthetic-mcp-lost-ack",
	}, true)
	assertions.Equal(true, result["isError"])
	assertions.Contains(toolErrorTextFromResult(t, result), "identity_outcome_unknown")
	assertions.Contains(toolErrorTextFromResult(t, result), "get_identity_receipt")
	assertions.Equal(int64(1), attempts.Load())
	read := rawCallTool(t, options, "get_identity_receipt", map[string]any{"idempotency_key": "synthetic-mcp-lost-ack"})
	requirements.NotEqual(true, read["isError"], "%v", read)
	assertions.Equal(true, toolStructuredContent(t, read)["changed"])
	assertions.Equal(int64(1), attempts.Load(), "MCP recovery must not replay the write")
}

func TestNativeIdentityMCPRejectsForgedPreviewWithSafeConflict(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := nativeIdentityMCPFixture(t)
	backend, st, first, second := fixture.client, fixture.store, fixture.first, fixture.second
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend, AllowIdentityDecisions: true}
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	preview, err := backend.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	result := confirmedCallTool(t, options, "link_participant_identity", map[string]any{
		"participant_id": first, "other_participant_id": second,
		"expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": "synthetic-forged-preview", "idempotency_key": "synthetic-forged-request",
	}, true)
	assertions.Equal(true, result["isError"])
	assertions.Contains(toolErrorTextFromResult(t, result), "identity_conflict")
	assertions.NotContains(fmt.Sprint(result), "synthetic-first@example.test")
	current, err := st.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Empty(current.Links)
}

func TestNativeIdentityMCPGraphAndPersonActionsStaySeparate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := nativeIdentityMCPFixture(t)
	backend, st, first, second := fixture.client, fixture.store, fixture.first, fixture.second
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend, AllowIdentityDecisions: true}
	var personID int64
	for _, action := range []struct {
		tool      string
		operation identitycontrol.Operation
		endpoint  string
	}{
		{"link_participant_identity", identitycontrol.OperationGraphLink, "other_participant_id"},
		{"unlink_participant_identity", identitycontrol.OperationGraphUnlink, "other_participant_id"},
		{"link_participant_to_person", identitycontrol.OperationPersonLink, "person_id"},
		{"unlink_participant_from_person", identitycontrol.OperationPersonUnlink, "person_id"},
	} {
		if action.operation == identitycontrol.OperationPersonLink {
			person, _, err := st.CreatePersonFromParticipant(first)
			requirements.NoError(err)
			personID = person.ID
		}
		participant, endpoint := first, second
		if personID != 0 {
			participant, endpoint = second, personID
		}
		previewResult := rawCallTool(t, options, "preview_identity_operation", map[string]any{
			"operation": string(action.operation), "participant_id": participant, action.endpoint: endpoint,
		})
		requirements.NotEqual(true, previewResult["isError"], "%v", previewResult)
		preview := toolStructuredContent(t, previewResult)
		snapshot, ok := preview["snapshot"].(map[string]any)
		requirements.True(ok)
		result := confirmedCallTool(t, options, action.tool, map[string]any{
			"participant_id": participant, action.endpoint: endpoint, "expected_fingerprint": snapshot["fingerprint"],
			"preview_token": preview["preview_token"], "idempotency_key": "synthetic-" + string(action.operation),
		}, true)
		requirements.NotEqual(true, result["isError"], "%v", result)
		assertions.Equal(true, toolStructuredContent(t, result)["changed"])
	}
	current, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: second, PersonID: personID})
	requirements.NoError(err)
	assertions.Empty(current.Links)
	for _, binding := range current.Bindings {
		assertions.NotEqual(second, binding.ParticipantID, "explicit detach must remove the selected binding")
	}
}

func TestNativeIdentityMCPPersonConfirmationNamesWholeComponent(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	fixture := nativeIdentityMCPFixture(t)
	_, err := fixture.store.LinkParticipants(fixture.first, fixture.second)
	requirements.NoError(err)
	person, _, err := fixture.store.CreatePersonFromParticipant(fixture.first)
	requirements.NoError(err)
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: fixture.client, AllowIdentityDecisions: true}
	preview, err := fixture.client.PreviewIdentityOperation(t.Context(), identitycontrol.PreviewRequest{Operation: identitycontrol.OperationPersonUnlink, Target: identitycontrol.IdentityTarget{ParticipantID: fixture.first, PersonID: person.ID}})
	requirements.NoError(err)
	requirements.Len(preview.Snapshot.ComponentMembers, 2)
	result := confirmedCallTool(t, options, "unlink_participant_from_person", map[string]any{
		"participant_id": fixture.first, "person_id": person.ID, "expected_fingerprint": preview.Snapshot.Fingerprint,
		"preview_token": preview.PreviewToken, "idempotency_key": "synthetic-component-unlink",
	}, true, func(message string) {
		assertions.Contains(message, "whole linked component")
		assertions.Contains(message, fmt.Sprintf("participant %d", fixture.first))
		assertions.Contains(message, fmt.Sprintf("person %d", person.ID))
	})
	requirements.NotEqual(true, result["isError"], "%v", result)
	current, err := fixture.store.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: fixture.first, PersonID: person.ID})
	requirements.NoError(err)
	assertions.Empty(current.Bindings, "the confirmed detach affects every linked member")
	assertions.Len(current.ComponentMembers, 2, "person detachment preserves graph links")
}

func TestNativeIdentityMCPToolsRequireBackendAndIdentityWriteOptIn(t *testing.T) {
	assertions := assert.New(t)
	backend := nativeIdentityMCPFixture(t).client
	without := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, true))
	assertions.NotContains(without, "preview_identity_operation")
	for _, writes := range []bool{false, true} {
		options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend}
		listed := toolsByName(t, rawListTools(t, options, writes))
		assertions.Contains(listed, "preview_identity_operation")
		assertions.Contains(listed, "get_identity_receipt")
		for _, name := range []string{"link_participant_identity", "unlink_participant_identity", "link_participant_to_person", "unlink_participant_from_person"} {
			assertions.NotContains(listed, name)
		}
	}
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend, AllowIdentityDecisions: true}
	readOnly := toolsByName(t, rawListTools(t, options, false))
	assertions.NotContains(readOnly, "link_participant_identity")
	writable := toolsByName(t, rawListTools(t, options, true))
	for _, name := range []string{"link_participant_identity", "unlink_participant_identity", "link_participant_to_person", "unlink_participant_from_person"} {
		assertions.Contains(writable, name)
	}
}

func TestNativeIdentityMCPLinksVerifiedPairWithoutReviewCandidate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := nativeIdentityMCPFixture(t)
	backend, st, first, second := fixture.client, fixture.store, fixture.first, fixture.second
	options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: backend, AllowIdentityDecisions: true}
	previewResult := rawCallTool(t, options, "preview_identity_operation", map[string]any{
		"operation": "graph-link", "participant_id": first, "other_participant_id": second,
	})
	requirements.NotEqual(true, previewResult["isError"], "%v", previewResult)
	preview := toolStructuredContent(t, previewResult)
	snapshot, ok := preview["snapshot"].(map[string]any)
	requirements.True(ok)
	args := map[string]any{"participant_id": first, "other_participant_id": second,
		"expected_fingerprint": snapshot["fingerprint"], "preview_token": preview["preview_token"], "idempotency_key": "synthetic-mcp-link"}
	declined := confirmedCallTool(t, options, "link_participant_identity", args, false)
	assertions.Equal(true, declined["isError"])
	intent := identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}
	current, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, intent)
	requirements.NoError(err)
	assertions.Empty(current.Links)
	linked := confirmedCallTool(t, options, "link_participant_identity", args, true, func(message string) {
		assertions.Contains(message, "graph-link")
	})
	requirements.NotEqual(true, linked["isError"], "%v", linked)
	result := toolStructuredContent(t, linked)
	assertions.Equal(true, result["changed"])
	read := rawCallTool(t, options, "get_identity_receipt", map[string]any{"idempotency_key": "synthetic-mcp-link"})
	requirements.NotEqual(true, read["isError"])
	assertions.Equal(result["id"], toolStructuredContent(t, read)["id"])
	args["preview_token"] = "expired-preview"
	retried := confirmedCallTool(t, options, "link_participant_identity", args, true)
	requirements.NotEqual(true, retried["isError"])
	assertions.Equal(result["id"], toolStructuredContent(t, retried)["id"])
	current, err = st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, intent)
	requirements.NoError(err)
	assertions.Len(current.Links, 1)
}

func TestNativeIdentityMCPRejectsRoundedEndpointFractions(t *testing.T) {
	for _, action := range []struct {
		operation      identitycontrol.Operation
		tool, endpoint string
	}{
		{identitycontrol.OperationGraphLink, "link_participant_identity", "other_participant_id"},
		{identitycontrol.OperationGraphUnlink, "unlink_participant_identity", "other_participant_id"},
		{identitycontrol.OperationPersonLink, "link_participant_to_person", "person_id"},
		{identitycontrol.OperationPersonUnlink, "unlink_participant_from_person", "person_id"},
	} {
		for _, key := range []string{"participant_id", action.endpoint} {
			t.Run(string(action.operation)+"/"+key, func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				fixture := nativeIdentityMCPFixture(t)
				participant, endpoint := fixture.first, fixture.second
				target := identitycontrol.IdentityTarget{ParticipantID: participant, OtherParticipantID: endpoint}
				if action.operation == identitycontrol.OperationGraphUnlink {
					_, err := fixture.store.LinkParticipants(participant, endpoint)
					requirements.NoError(err)
				}
				if action.endpoint == "person_id" {
					person, _, err := fixture.store.CreatePersonFromParticipant(fixture.first)
					requirements.NoError(err)
					endpoint = person.ID
					if action.operation == identitycontrol.OperationPersonLink {
						participant = fixture.second
					}
					target = identitycontrol.IdentityTarget{ParticipantID: participant, PersonID: endpoint}
				}
				intent := identitycontrol.PreviewRequest{Operation: action.operation, Target: target}
				preview, err := fixture.client.PreviewIdentityOperation(t.Context(), intent)
				requirements.NoError(err)
				options := ServeOptions{Engine: &querytest.MockEngine{}, IdentityOperations: fixture.client, AllowIdentityDecisions: true}
				args := map[string]any{"participant_id": participant, action.endpoint: endpoint, "operation": string(action.operation)}
				badID := participant
				if key == action.endpoint {
					badID = endpoint
				}
				// Preserve the original fraction through the real SDK adapter. A float64
				// argument map would round it before the identity handler can inspect it.
				args[key] = jsontext.Value(fmt.Sprintf("%d.0000000000000001", badID))
				rejected := rawCallTool(t, options, "preview_identity_operation", args)
				assertions.Equal(true, rejected["isError"], "a fractional endpoint must not receive a signed integer preview")
				delete(args, "operation")
				args["expected_fingerprint"] = preview.Snapshot.Fingerprint
				args["preview_token"] = preview.PreviewToken
				requestKey := "synthetic-fraction-" + string(action.operation) + "-" + key
				args["idempotency_key"] = requestKey
				result := confirmedCallTool(t, options, action.tool, args, true)
				assertions.Equal(true, result["isError"], "a valid integer preview must not authorize a fractional endpoint")
				current, err := fixture.store.IdentityOperationPreviewContext(t.Context(), action.operation, target)
				requirements.NoError(err)
				assertions.Equal(preview.Snapshot.Fingerprint, current.Fingerprint, "fractional input must leave identity evidence unchanged")
				_, err = fixture.store.IdentityOperationReceiptContext(t.Context(), "owner", requestKey)
				assertions.ErrorIs(err, store.ErrIdentityOperationReceiptNotFound)
			})
		}
	}
}
