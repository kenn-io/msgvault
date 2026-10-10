package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationPreviewGateChecksFreshWritesAndReauthorizesRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, target, "synthetic-preview-gated")
	authorityCalls, previewCalls := 0, 0
	authorize := func(context.Context, *IdentitySnapshot) error { authorityCalls++; return nil }
	expired := errors.New("synthetic expired preview")
	verify := func(context.Context, *IdentitySnapshot) error { previewCalls++; return expired }
	_, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, verify)
	requirements.ErrorIs(err, expired)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	first, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, func(context.Context, *IdentitySnapshot) error { previewCalls++; return nil })
	requirements.NoError(err)
	second, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, verify)
	requirements.NoError(err)
	assertions.Equal(first, second)
	assertions.Equal(3, authorityCalls)
	assertions.Equal(2, previewCalls, "an authorized committed retry needs no unexpired preview")
	revoked := errors.New("synthetic revoked authority")
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, func(context.Context, *IdentitySnapshot) error { return revoked }, verify)
	requirements.ErrorIs(err, revoked)
	changed := request
	changed.IdempotencyKey = "synthetic-new-write"
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), changed, authorize, verify)
	requirements.ErrorIs(err, expired, "a different key cannot bypass preview verification")
}

func TestIdentityOperationPreviewGateRequiresBothNativeChecks(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-missing-native-check")
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	_, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, nil, allow)
	requirements.ErrorIs(err, identitycontrol.ErrInvalidRequest)
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, nil)
	requirements.ErrorIs(err, identitycontrol.ErrInvalidRequest)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assert.Empty(t, edges)
}

func TestIdentityOperationReceiptReadbackSurvivesOversizedComponent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-oversized-recovery")
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	committed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)
	for i := range 99 {
		member, err := st.EnsureParticipant(fmt.Sprintf("synthetic-grown-%03d@example.test", i), "Synthetic Member", "example.test")
		requirements.NoError(err)
		_, err = st.LinkParticipants(a, member)
		requirements.NoError(err)
	}
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.ErrorIs(err, ErrIdentityOperationTooLarge, "retry must not bypass complete current scope admission")
	saved, err := st.IdentityOperationReceiptContext(t.Context(), request.Principal, request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Equal(committed, saved)
	byID, err := st.IdentityReceiptByIDContext(t.Context(), committed.ID)
	requirements.NoError(err)
	assertions.Equal(committed, byID, "trusted owner recovery must not replay the mutation")
}
