package mcpevents

import (
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
)

func TestNativePermissionAdmissionFailsBeforeChallengeWithStableReason(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	service, fixture, request := eventService(t)
	resource, err := fixture.Store.GetMCPResource(t.Context(), "source", fixture.Source.ID)
	require.NoError(err)
	require.NoError(fixture.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: fixture.Source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: fixture.Source.Identifier}))

	var calls atomic.Int32
	service.webhook = tlsWebhook(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err = service.Subscribe(t.Context(), service.principal, request)
	var unavailable *Error
	require.ErrorAs(err, &unavailable)
	assert.Equal(-32012, unavailable.Code)
	assert.Equal("native_authority_unavailable", unavailable.Reason)
	assert.Zero(calls.Load(), "unproven native authority must fail before receiver I/O")
	rows, err := fixture.Store.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	assert.Empty(rows)
}
