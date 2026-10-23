package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

// A competing writer can hold the identity fence longer than a short TTL.
// Activation must start the subscription's times once it holds the fences,
// or it would activate a subscription that has already expired.
func TestMCPActivationTimesStartAfterFencesPG(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := requirePostgreSQLStore(t)
	source, err := st.GetOrCreateSource("gmail", "activation-ttl@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "activation-ttl-thread", "Activation TTL")
	require.NoError(err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)
	input := store.MCPSubscription{
		ID: "sub_activation_ttl", Principal: "owner:synthetic", Name: "msgvault.message_archived",
		Arguments: []byte(`{"conversation_id":"` + strconv.FormatInt(conversation, 10) + `","include_from_me":false}`),
		ScopeKind: "conversation", ScopeID: conversation, CallbackURL: "https://receiver.example.net/events",
		SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1,
	}
	require.NoError(st.BindMCPSubscriptionScope(t.Context(), &input))

	ttl := 200 * time.Millisecond
	release := holdIdentityRow(t, st)
	type result struct {
		sub *store.MCPSubscription
		err error
	}
	done := make(chan result, 1)
	go func() {
		sub, _, err := st.ActivateMCPSubscription(context.Background(), store.MCPActivation{Subscription: input, TTL: ttl})
		done <- result{sub, err}
	}()
	waitForLockWait(t, st, "archive_metadata", "activation must wait at the identity fence")
	// Hold the fence past the requested TTL, as a competing writer could.
	<-time.After(2 * ttl)
	released := time.Now()
	release()
	var got result
	select {
	case got = <-done:
	case <-time.After(pgWaitBudget):
		require.FailNow("activation did not finish after the fence was released")
	}
	require.NoError(got.err)
	require.NotNil(got.sub)
	assert.True(got.sub.ExpiresAt.After(released), "expiry %s must follow the fence release %s", got.sub.ExpiresAt, released)
}
