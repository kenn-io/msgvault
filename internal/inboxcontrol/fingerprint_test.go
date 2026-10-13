package inboxcontrol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestSemanticFingerprintIgnoresObservationTimeAndSetOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "synthetic-message"}
	state := State{Target: target, Tags: []string{"UNREAD", "INBOX", "Todo"}, ObservedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	want, err := SemanticFingerprint(state)
	requirements.NoError(err)
	state.ObservedAt = state.ObservedAt.Add(time.Minute)
	state.Tags = []string{"Todo", "INBOX", "UNREAD"}
	got, err := SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.Equal(want, got)
	state.Tags = []string{"INBOX", "Todo"}
	changed, err := SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.NotEqual(want, changed)
}

func TestSemanticFingerprintBindsUnknownStateAndIncomingWatermark(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "synthetic-account", AccountID: "synthetic-account", Scope: ScopeChat, ItemID: 2, ProviderID: "!synthetic:example.com"}
	state := State{Target: target, LastMessageID: "last-observed-message"}
	want, err := SemanticFingerprint(state)
	requirements.NoError(err)
	unread := false
	state.MarkedUnread = &unread
	changed, err := SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.NotEqual(want, changed)
	state.MarkedUnread = nil
	state.LastMessageID = "new-incoming-message"
	changed, err = SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.NotEqual(want, changed)
}

func TestIntentFingerprintExcludesTransportAndExecutionEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "synthetic-message"}
	request := Request{Operation: OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Todo", "Watch"}}, DryRun: true}
	want, err := IntentFingerprint(request)
	requirements.NoError(err)
	request.DryRun = false
	request.Expected = &State{Target: target, ObservedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	request.IdempotencyKey = "operation"
	request.PreviewToken = "signed-preview"
	request.Tags = &emailtags.MessageTagChange{Add: []string{"Watch", "Todo"}}
	got, err := IntentFingerprint(request)
	requirements.NoError(err)
	assertions.Equal(want, got)
	other := target
	other.AccountID = "other@example.com"
	request.Target = &other
	request.Expected.Target = other
	changed, err := IntentFingerprint(request)
	requirements.NoError(err)
	assertions.NotEqual(want, changed)
}

func TestIntentFingerprintBindsGmailMoveOrigin(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.com", AccountID: "owner@example.com", Scope: ScopeMessage, ItemID: 1, ProviderID: "message-1"}
	request := Request{Operation: OpMove, Target: &target, OriginFolder: &Folder{ID: "LabelOld"}, Destination: &Folder{ID: "LabelNew"}, DryRun: true}
	before, err := IntentFingerprint(request)
	require.NoError(t, err)
	request.OriginFolder = &Folder{ID: "LabelOther"}
	after, err := IntentFingerprint(request)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

func TestSemanticFingerprintCanonicalizesFolderCatalog(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	state := State{Source: SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.com", AccountID: "owner@example.com"}, Folders: []Folder{{ID: "LabelB", Name: "B"}, {ID: "LabelA", Name: "A"}}}
	want, err := SemanticFingerprint(state)
	requirements.NoError(err)
	state.Folders = []Folder{{ID: "LabelA", Name: "A"}, {ID: "LabelB", Name: "B"}}
	got, err := SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.Equal(want, got)
	state.Folders[0].Name = "Renamed"
	got, err = SemanticFingerprint(state)
	requirements.NoError(err)
	assertions.NotEqual(want, got)
}

func TestIntentFingerprintUsesProviderCaseRules(t *testing.T) {
	for _, provider := range []string{"gmail", "msmail", "imap"} {
		t.Run(provider, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			target := Target{SourceID: 1, SourceType: provider, SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: ScopeMessage, ItemID: 2, ProviderID: "message"}
			if provider == "imap" {
				target.Mailbox = "INBOX"
				target.UIDValidity = 42
				target.UID = 3
			}
			request := Request{Operation: OpTags, Target: &target, DryRun: true, Tags: &emailtags.MessageTagChange{Add: []string{"Todo"}}}
			first, err := IntentFingerprint(request)
			requirements.NoError(err)
			request.Tags.Add = []string{"todo"}
			second, err := IntentFingerprint(request)
			requirements.NoError(err)
			if provider == "gmail" {
				assertions.NotEqual(first, second)
			} else {
				assertions.Equal(first, second)
			}
		})
	}
}

func TestIntentFingerprintUsesCanonicalUnicodeCaseEquivalence(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "msmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: ScopeMessage, ItemID: 2, ProviderID: "message"}
	request := Request{Operation: OpTags, Target: &target, DryRun: true, Tags: &emailtags.MessageTagChange{Add: []string{"TodoΣ"}}}
	first, err := IntentFingerprint(request)
	require.NoError(t, err)
	request.Tags.Add = []string{"Todoς"}
	second, err := IntentFingerprint(request)
	require.NoError(t, err)
	assert.Equal(t, first, second, "canonical native tags compare Unicode category names with EqualFold")
}
