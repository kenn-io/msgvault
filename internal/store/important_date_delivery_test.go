package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateDeliveryClaimAndUnknown(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, "synthetic-occurrence", "fingerprint-example")
	require.NoError(t, err)
	assert.True(t, claimed)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, "synthetic-occurrence", "fingerprint-example")
	require.NoError(t, err)
	assert.False(t, claimed)
	changed, err := st.AdvanceImportantDateDeliveryContext(ctx, "synthetic-occurrence", store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic-occurrence", store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, "issue-example")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic-occurrence", store.ImportantDateIssueCreated, store.ImportantDateNotifyInFlight, "issue-example")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic-occurrence", store.ImportantDateNotifyInFlight, store.ImportantDateUnknown, "issue-example")
	require.NoError(t, err)
	assert.True(t, changed)
	_, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic-occurrence", store.ImportantDateUnknown, store.ImportantDateNotifyInFlight, "issue-example")
	assert.Error(t, err)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, "synthetic-occurrence", "fingerprint-example")
	require.NoError(t, err)
	assert.False(t, claimed)
	state, issueUID, err := st.ImportantDateDeliveryContext(ctx, "synthetic-occurrence")
	require.NoError(t, err)
	assert.Equal(t, store.ImportantDateUnknown, state)
	assert.Equal(t, "issue-example", issueUID)
}

func TestImportantDatePendingAcrossYears(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	prefix := "person/example:birthday:"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, prefix+"2027", "fingerprint-example")
	require.NoError(t, err)
	require.True(t, claimed)
	pending, err := st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "fingerprint-example")
	require.NoError(t, err)
	assert.True(t, pending)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2027", "fingerprint-example")
	require.NoError(t, err)
	assert.False(t, pending)
	for _, transition := range [][2]store.ImportantDateDeliveryState{
		{store.ImportantDateClaimed, store.ImportantDateIssueInFlight},
		{store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated},
		{store.ImportantDateIssueCreated, store.ImportantDateNotifyInFlight},
		{store.ImportantDateNotifyInFlight, store.ImportantDateDelivered},
	} {
		issue := "issue-example"
		if transition[1] == store.ImportantDateIssueInFlight {
			issue = ""
		}
		changed, err := st.AdvanceImportantDateDeliveryContext(ctx, prefix+"2027", transition[0], transition[1], issue)
		require.NoError(t, err)
		require.True(t, changed)
	}
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "fingerprint-example")
	require.NoError(t, err)
	assert.False(t, pending)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "corrected-fingerprint")
	require.NoError(t, err)
	assert.True(t, pending)
}
