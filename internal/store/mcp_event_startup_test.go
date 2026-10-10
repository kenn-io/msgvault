package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsStartupReadsSecretsAcrossRevokedPrincipals(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	config := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-old", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}}
	_, err := f.Store.ConfigureMCPEvents(t.Context(), config)
	require.NoError(err)
	now := time.Now().UTC()
	input := store.MCPSubscription{ID: "sub_synthetic_startup", Principal: config.Principal, Name: "msgvault.message_archived", Arguments: []byte(`{}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: "https://receiver.example.net/hook", SecretEnc: []byte("synthetic-secret-ciphertext"), PreviousSecretEnc: []byte("synthetic-previous-ciphertext"), SecretRevision: 2, VerifiedRevision: 2, ExpiresAt: now.Add(time.Hour)}
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	config.Principal = "owner:synthetic-new"
	_, err = f.Store.ConfigureMCPEvents(t.Context(), config)
	require.NoError(err)
	current, err := f.Store.ListMCPSubscriptions(t.Context(), config.Principal)
	require.NoError(err)
	assert.Empty(current)
	retained, err := f.Store.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(retained, 1)
	assert.Equal(input.Principal, retained[0].Principal)
	assert.Equal("principal_revoked", retained[0].StopReason)
	assert.Equal(input.SecretEnc, retained[0].SecretEnc)
	assert.Equal(input.PreviousSecretEnc, retained[0].PreviousSecretEnc)
}
