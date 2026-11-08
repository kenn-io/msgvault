package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateDeliveryClaimAndUnknown(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", "fingerprint-example")
	must.NoError(err)
	checks.True(claimed)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", "fingerprint-example")
	must.NoError(err)
	checks.False(claimed)
	changed, err := st.AdvanceImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
	must.NoError(err)
	checks.True(changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, "issue-example")
	must.NoError(err)
	checks.True(changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", store.ImportantDateIssueCreated, store.ImportantDateNotifyInFlight, "issue-example")
	must.NoError(err)
	checks.True(changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", store.ImportantDateNotifyInFlight, store.ImportantDateUnknown, "issue-example")
	must.NoError(err)
	checks.True(changed)
	_, err = st.AdvanceImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", store.ImportantDateUnknown, store.ImportantDateNotifyInFlight, "issue-example")
	checks.Error(err)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027", "fingerprint-example")
	must.NoError(err)
	checks.False(claimed)
	state, issueUID, err := st.ImportantDateDeliveryContext(ctx, "synthetic:occurrence:2027")
	must.NoError(err)
	checks.Equal(store.ImportantDateUnknown, state)
	checks.Equal("issue-example", issueUID)
}

func TestImportantDatePendingAcrossYears(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	prefix := "person/example:birthday:"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, prefix+"2027", "fingerprint-example")
	must.NoError(err)
	must.True(claimed)
	pending, err := st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "fingerprint-example")
	must.NoError(err)
	checks.True(pending)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, prefix+"2028", "fingerprint-example")
	must.NoError(err)
	checks.False(claimed)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2027", "fingerprint-example")
	must.NoError(err)
	checks.False(pending)
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
		must.NoError(err)
		must.True(changed)
	}
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "fingerprint-example")
	must.NoError(err)
	checks.False(pending)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "corrected-fingerprint")
	must.NoError(err)
	checks.True(pending)
	updated, err := st.AcceptImportantDateCorrectionContext(ctx, prefix, "corrected-fingerprint")
	must.NoError(err)
	checks.Equal(int64(1), updated)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, prefix, prefix+"2028", "corrected-fingerprint")
	must.NoError(err)
	checks.False(pending)
}

func TestImportantDateOperatorResolutionNeverReclaims(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	key := "person/example:birthday:2027"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, key, "fingerprint-example")
	must.NoError(err)
	must.True(claimed)
	changed, err := st.AdvanceImportantDateDeliveryContext(ctx, key, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
	must.NoError(err)
	must.True(changed)
	changed, err = st.ResolveImportantDateDeliveryContext(ctx, key, store.ImportantDateDelivered, "")
	checks.Error(err)
	checks.False(changed)
	changed, err = st.ResolveImportantDateDeliveryContext(ctx, key, store.ImportantDateDelivered, "issue-example")
	must.NoError(err)
	checks.True(changed)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, key, "fingerprint-example")
	must.NoError(err)
	checks.False(claimed)
}
