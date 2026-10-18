package api

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"
	"time"

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
	observedAt := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	var itemSDK generated.InboxState
	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		before := inboxcontrol.State{Target: target, Read: new(false), Tags: values, Flags: values, ObservedAt: observedAt}
		responseJSON, err := json.Marshal(before)
		requirements.NoError(err)
		var sdk generated.InboxState
		requirements.NoError(jsonv1.Unmarshal(responseJSON, &sdk))
		requirements.NoError(sdk.Validate(), "item responses validate the populated Target binding")
		itemSDK = sdk
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

	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "imap", SourceIdentifier: "imaps://reader@example.test:993", AccountID: "reader"}
	folderState := inboxcontrol.State{Source: source, ObservedAt: observedAt}
	responseJSON, err := json.Marshal(folderState)
	requirements.NoError(err)
	var folderSDK generated.InboxState
	requirements.NoError(jsonv1.Unmarshal(responseJSON, &folderSDK))
	requirements.NoError(folderSDK.Validate(), "folder responses validate the populated Source binding")
	assertions.Equal(source.AccountID, folderSDK.Source.AccountID)
	assertions.Empty(folderSDK.Target.Scope)

	bothBindings := folderSDK
	bothBindings.Target = itemSDK.Target
	requirements.Error(bothBindings.Validate(), "item and folder bindings cannot be mixed")
	emptyBinding := generated.InboxState{ObservedAt: observedAt}
	requirements.Error(emptyBinding.Validate(), "a state must contain one binding")
}
