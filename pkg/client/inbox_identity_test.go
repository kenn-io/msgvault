package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestInboxGeneratedClientPreservesIMAPIdentityRange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	for _, identity := range []uint32{1, 2147483647, 2147483648, 4294967295} {
		target := inboxcontrol.Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "recorded-copy", Mailbox: "INBOX", UID: identity, UIDValidity: identity}
		request := inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}
		requirements.NoError(request.Validate())
		encoded, err := json.Marshal(request)
		requirements.NoError(err)
		var wire generated.ControlInboxBody
		requirements.NoError(json.Unmarshal(encoded, &wire), "valid IMAP identities must reach the daemon unchanged")
		encoded, err = json.Marshal(wire)
		requirements.NoError(err)
		var restored inboxcontrol.Request
		requirements.NoError(json.Unmarshal(encoded, &restored))
		requirements.NotNil(restored.Target)
		assertions.Equal(target, *restored.Target)

		folder := inboxcontrol.Folder{ID: "Archive", UIDValidity: identity}
		encoded, err = json.Marshal(folder)
		requirements.NoError(err)
		var generatedFolder generated.InboxFolder
		requirements.NoError(json.Unmarshal(encoded, &generatedFolder))
		encoded, err = json.Marshal(generatedFolder)
		requirements.NoError(err)
		var restoredFolder inboxcontrol.Folder
		requirements.NoError(json.Unmarshal(encoded, &restoredFolder))
		assertions.Equal(folder, restoredFolder)
	}
}
