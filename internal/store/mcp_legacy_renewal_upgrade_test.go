package store_test

import (
	"crypto/sha256"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested tests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested tests.
	"go.kenn.io/msgvault/internal/store"
)

// Rejecting every renewal of an adopted pre-reference subscription would lose
// the original subscription identity and queued occurrence at the upgrade.
func TestMCPAdoptedLegacySubscriptionRenewsIntoCurrentConversationReference(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := openMCPEventsLegacyFixture(t)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	old := rows[0]
	require.Zero(old.ScopeReferenceID)
	_, err = st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(old.Principal, true))
	require.NoError(err)
	grantSyntheticSourceAuthority(t, st, old.SourceID)
	input := old
	input.ExpiresAt = old.UpdatedAt.Add(23 * time.Hour)
	require.NoError(st.BindMCPSubscriptionScope(t.Context(), &input))
	require.Positive(input.ScopeReferenceID)
	current, _, err := st.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: old.Generation, ExpectedState: old.State, ExpectedSecretRevision: old.SecretRevision, Now: old.UpdatedAt})
	require.NoError(err)
	require.NotNil(current)
	assert.Equal(old.ID, current.ID)
	assert.Equal(input.ScopeReferenceID, current.ScopeReferenceID)
	assert.Equal(old.Generation, current.Generation)
	assert.Equal(old.PendingSeq, current.PendingSeq)
	assert.Equal(old.CursorSeq, current.CursorSeq)
	assert.Equal(sha256.Sum256(old.PendingEnvelope), sha256.Sum256(current.PendingEnvelope))
}
