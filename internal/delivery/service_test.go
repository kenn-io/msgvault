package delivery_test

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/delivery"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

func TestExplicitModesRecoverContentAndDenySelfElevation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("peer@example.test", "Example Person", "example.test"))
	requirements.NoError(err)
	service := delivery.Service{Store: f.Store}
	_, err = service.Write(t.Context(), delivery.Authority{PolicyRead: true}, store.DeliveryPolicyWrite{}, false)
	requirements.ErrorIs(err, delivery.ErrPolicyForbidden)
	calls, drafts := 0, 0
	draft := func(context.Context) error { drafts++; return nil }
	send := func(context.Context) error { calls++; return nil }
	content := []byte("synthetic recoverable draft")
	receipt, err := delivery.Execute(t.Context(), f.Store, delivery.DispatchRequest{Mode: delivery.ModeDraft, Content: content}, draft, send)
	requirements.NoError(err)
	assertions.Equal(content, receipt.Content)
	assertions.Equal(1, drafts)
	assertions.Zero(calls)
	receipt, err = delivery.Execute(t.Context(), f.Store, delivery.DispatchRequest{Mode: delivery.ModeSend, Content: content, Recipients: []store.DeliveryRecipient{{PersonUID: p.VCardUID}}}, draft, send)
	requirements.Error(err)
	assertions.Equal("draft_required", receipt.Code)
	assertions.Equal(content, receipt.Content)
	assertions.False(receipt.ProviderAttempted)
	assertions.Equal(1, drafts)
	assertions.Zero(calls)
}
