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
	observedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		before := inboxcontrol.State{ObservedAt: observedAt, Target: target, Read: new(false), Tags: values, Flags: values}
		responseJSON, err := json.Marshal(before)
		requirements.NoError(err)
		var sdk generated.InboxState
		requirements.NoError(jsonv1.Unmarshal(responseJSON, &sdk))
		requirements.NoError(sdk.Validate())
		assertions.Equal(values, sdk.Tags)
		assertions.Equal(values, sdk.Flags)
		sdkRequest := generated.InboxRequest{Operation: "set-read", Expected: &sdk}
		requirements.NoError(sdkRequest.Validate())
		requestJSON, err := jsonv1.Marshal(sdkRequest)
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

func TestInboxStateGeneratedSDKValidatesMessageAndSourceAlternatives(t *testing.T) {
	requirements := require.New(t)
	observedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	messageState := generated.InboxState{
		ObservedAt: observedAt,
		Target: generated.InboxTarget{
			SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test",
			AccountID: "owner@example.test", Scope: "message", ItemID: 2, ProviderID: "synthetic-message",
		},
	}
	sourceState := generated.InboxState{
		ObservedAt: observedAt,
		Source: generated.InboxSourceIdentity{
			SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test",
		},
	}
	for _, tc := range []struct {
		name   string
		result generated.InboxResult
	}{
		{name: "message preview", result: generated.InboxResult{Before: &messageState, Projected: &messageState}},
		{name: "message execution", result: generated.InboxResult{After: &messageState}},
		{name: "source-only folder preview", result: generated.InboxResult{Before: &sourceState, Projected: &sourceState}},
		{name: "source-only folder execution", result: generated.InboxResult{After: &sourceState}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := jsonv1.Marshal(tc.result)
			requirements.NoError(err)
			var decoded generated.InboxResult
			requirements.NoError(jsonv1.Unmarshal(encoded, &decoded))
			requirements.NoError(decoded.Validate())
		})
	}
	requirements.Error((generated.InboxState{ObservedAt: observedAt}).Validate(), "an Inbox state requires one active binding")
	bothBindings := messageState
	bothBindings.Source = sourceState.Source
	requirements.Error(bothBindings.Validate(), "message and source bindings are alternatives")
}
