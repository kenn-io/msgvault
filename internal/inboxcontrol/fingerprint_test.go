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
	request := Request{Operation: OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Todo", "Watch"}}, DryRun: true}
	want, err := IntentFingerprint(request)
	requirements.NoError(err)
	request.DryRun = false
	request.Expected = &State{Target: target, ObservedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	request.IdempotencyKey = "operation"
	request.PreviewToken = "signed-preview"
	request.Tags = &emailtags.Change{Add: []string{"Watch", "Todo"}}
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
