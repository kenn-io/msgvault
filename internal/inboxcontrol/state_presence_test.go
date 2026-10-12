package inboxcontrol

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxStateJSONPreservesObservedEmptySets(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "imaps://reader@example.test:993", AccountID: "reader", Scope: ScopeMessage, ItemID: 2, ProviderID: "synthetic-message", Mailbox: "INBOX", UIDValidity: 17, UID: 1}
	unknown := State{Target: target, Read: new(false)}
	for _, tc := range []struct {
		name  string
		state State
	}{
		{name: "unknown", state: unknown},
		{name: "known empty", state: State{Target: target, Read: new(false), Tags: []string{}, Flags: []string{}}},
		{name: "known populated", state: State{Target: target, Read: new(true), Tags: []string{"Todo"}, Flags: []string{`\Seen`, "Todo"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			encoded, err := json.Marshal(tc.state)
			requirements.NoError(err)
			var got State
			requirements.NoError(json.Unmarshal(encoded, &got))
			assertions.Equal(tc.state.Tags, got.Tags)
			assertions.Equal(tc.state.Flags, got.Flags)
			wantHash, err := SemanticFingerprint(tc.state)
			requirements.NoError(err)
			gotHash, err := SemanticFingerprint(got)
			requirements.NoError(err)
			assertions.Equal(wantHash, gotHash)
			if tc.name == "known empty" {
				unknownHash, err := SemanticFingerprint(unknown)
				requirements.NoError(err)
				assertions.NotEqual(unknownHash, gotHash, "signed state must distinguish missing from known-empty evidence")
			}
		})
	}
}
