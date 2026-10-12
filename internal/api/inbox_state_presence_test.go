package api

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestInboxStateCollectionsRemainOptionalInOpenAPI(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		state := document.Components.Schemas.Map()["InboxState"]
		require.NotNil(state)
		for _, field := range []string{"tags", "flags"} {
			require.NotNil(state.Properties[field])
			assert.Equal("array", state.Properties[field].Type)
			assert.NotContains(state.Required, field)
		}
	}
}

func TestInboxStateGeneratedSDKPreservesExpectedEvidence(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "imaps://reader@example.test:993", AccountID: "reader", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "synthetic-message", Mailbox: "INBOX", UIDValidity: 17, UID: 1}
	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		before := inboxcontrol.State{Target: target, Read: new(false), Tags: values, Flags: values}
		responseJSON, err := json.Marshal(before)
		require.NoError(err)
		var sdk generated.InboxState
		require.NoError(jsonv1.Unmarshal(responseJSON, &sdk))
		assert.Equal(values, sdk.Tags)
		assert.Equal(values, sdk.Flags)
		requestJSON, err := jsonv1.Marshal(generated.InboxRequest{Operation: "set-read", Expected: &sdk})
		require.NoError(err)
		var request inboxcontrol.Request
		require.NoError(json.Unmarshal(requestJSON, &request))
		require.NotNil(request.Expected)
		assert.Equal(values, request.Expected.Tags)
		assert.Equal(values, request.Expected.Flags)
		wantHash, err := inboxcontrol.SemanticFingerprint(before)
		require.NoError(err)
		gotHash, err := inboxcontrol.SemanticFingerprint(*request.Expected)
		require.NoError(err)
		assert.Equal(wantHash, gotHash, "SDK expected-state transport must retain the signed preflight evidence")
	}
}
