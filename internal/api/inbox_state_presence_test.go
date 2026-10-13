package api

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestInboxStateCollectionsRemainOptionalInOpenAPI(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		state := document.Components.Schemas.Map()["InboxState"]
		requirements.NotNil(state)
		for _, field := range []string{"tags", "flags"} {
			requirements.NotNil(state.Properties[field])
			assertions.Equal("array", state.Properties[field].Type)
			assertions.NotContains(state.Required, field)
		}
	}
}

func TestInboxStateGeneratedSDKPreservesExpectedEvidence(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "imaps://reader@example.test:993", AccountID: "reader", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "synthetic-message", Mailbox: "INBOX", UIDValidity: 17, UID: 1}
	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		before := inboxcontrol.State{Target: target, Read: new(false), Tags: values, Flags: values}
		responseJSON, err := json.Marshal(before)
		requirements.NoError(err)
		var sdk generated.InboxState
		requirements.NoError(jsonv1.Unmarshal(responseJSON, &sdk))
		assertions.Equal(values, sdk.Tags)
		assertions.Equal(values, sdk.Flags)
		requestJSON, err := jsonv1.Marshal(generated.InboxRequest{Operation: "set-read", Expected: &sdk})
		requirements.NoError(err)
		var request inboxcontrol.Request
		requirements.NoError(json.Unmarshal(requestJSON, &request))
		requirements.NotNil(request.Expected)
		assertions.Equal(values, request.Expected.Tags)
		assertions.Equal(values, request.Expected.Flags)
		wantHash, err := inboxcontrol.SemanticFingerprint(before)
		requirements.NoError(err)
		gotHash, err := inboxcontrol.SemanticFingerprint(*request.Expected)
		requirements.NoError(err)
		assertions.Equal(wantHash, gotHash, "SDK expected-state transport must retain the signed preflight evidence")
	}
}
