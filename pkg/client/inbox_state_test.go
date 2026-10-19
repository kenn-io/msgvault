package client

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedInboxStateValidatesItemAndFolderResponses(t *testing.T) {
	observedAt := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	item := inboxcontrol.State{
		Target: inboxcontrol.Target{
			SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test",
			Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "synthetic-message",
		},
		Read:       new(false),
		ObservedAt: observedAt,
	}
	folder := inboxcontrol.State{
		Source:     inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test"},
		Folders:    []inboxcontrol.Folder{{ID: "synthetic-label", Name: "Todo"}},
		ObservedAt: observedAt,
	}

	for _, tc := range []struct {
		name  string
		state inboxcontrol.State
	}{
		{name: "item", state: item},
		{name: "folder", state: folder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			response, err := json.Marshal(tc.state)
			requirements.NoError(err)
			var sdkState generated.InboxState
			requirements.NoError(json.Unmarshal(response, &sdkState))
			requirements.NoError(sdkState.Validate(), string(response))
			assertions.NotEqual(inboxcontrol.State{}, tc.state)

			invalid := sdkState
			if tc.name == "item" {
				invalid.Target.ProviderID = ""
			} else {
				invalid.Source.AccountID = ""
			}
			assertions.Error(invalid.Validate(), "the populated binding must still be validated")
		})
	}
}
