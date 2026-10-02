package provideridentity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/testutil"
)

type gmailSendAsInventory struct {
	entries []gmail.SendAs
	err     error
}

func (i gmailSendAsInventory) ListSendAs(context.Context) ([]gmail.SendAs, error) {
	return i.entries, i.err
}

func TestGmailSendAsUsesProviderSnapshotPipeline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "inbox@example.test")
	require.NoError(err)
	entries := []gmail.SendAs{
		{Email: "inbox@example.test", Primary: true},
		{Email: "work@example.test", VerificationStatus: "accepted", DisplayName: "Synthetic"},
		{Email: "pending@example.test", VerificationStatus: "pending"},
		{Email: "*@example.test", VerificationStatus: "accepted"},
	}
	snapshot := provideridentity.GmailSnapshot(entries)
	report := identityops.DiscoverResult{}
	identityops.MergeExternalEvidence(&report, snapshot.Evidence)
	require.Len(report.Candidates, 3)
	for _, candidate := range report.Candidates {
		if candidate.Identifier == "pending@example.test" {
			assert.Equal("weak", candidate.Classification)
		}
	}
	require.Len(report.Rejected, 1)
	outcomes, changed, err := provideridentity.ApplyGmailSendAs(t.Context(), st, source.ID, gmailSendAsInventory{entries: entries})
	require.NoError(err)
	assert.True(changed)
	assert.Len(outcomes, 2)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Len(identities, 2)
	_, _, err = provideridentity.ApplyGmailSendAs(t.Context(), st, source.ID, gmailSendAsInventory{err: errors.New("synthetic provider failure")})
	require.ErrorContains(err, "read Gmail send-as identities")
	afterFailure, readErr := st.ListAccountIdentities(source.ID)
	require.NoError(readErr)
	assert.Equal(identities, afterFailure)
	// Inventory order does not create a new state or writes.
	entries[0], entries[1] = entries[1], entries[0]
	outcomes, changed, err = provideridentity.ApplySnapshot(t.Context(), st, source.ID, provideridentity.GmailSnapshot(entries))
	require.NoError(err)
	assert.False(changed)
	assert.Empty(outcomes)
	// Verification transitions reuse the metadata and ownership transaction.
	entries[2].VerificationStatus = "accepted"
	outcomes, changed, err = provideridentity.ApplySnapshot(t.Context(), st, source.ID, provideridentity.GmailSnapshot(entries))
	require.NoError(err)
	assert.True(changed)
	require.Len(outcomes, 1)
	assert.Equal("pending@example.test", outcomes[0].Identifier)
}

func TestGmailSendAsSelectionConfirmsOnlyOwnerChoices(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "inbox@example.test")
	require.NoError(err)
	entries := []gmail.SendAs{
		{Email: "inbox@example.test", Primary: true},
		{Email: "work@example.test", VerificationStatus: "accepted"},
		{Email: "pending@example.test", VerificationStatus: "pending"},
	}
	// Validate the whole selection before saving any metadata or ownership.
	_, _, err = provideridentity.ApplyGmailSendAsSelection(t.Context(), st, source.ID, entries, []string{"work@example.test", "pending@example.test"})
	require.Error(err)
	metadata, err := st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "gmail-send-as")
	require.NoError(err)
	assert.Empty(metadata)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities)
	for _, selected := range [][]string{nil, {"missing@example.test"}, {"*@example.test"}} {
		_, _, err = provideridentity.ApplyGmailSendAsSelection(t.Context(), st, source.ID, entries, selected)
		require.Error(err)
	}
	outcomes, changed, err := provideridentity.ApplyGmailSendAsSelection(t.Context(), st, source.ID, entries, []string{" WORK@example.test ", "work@example.test"})
	require.NoError(err)
	assert.True(changed)
	require.Len(outcomes, 1)
	assert.True(outcomes[0].Added)
	identities, err = st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("work@example.test", identities[0].Address)
	assert.Contains(identities[0].SourceSignal, "gmail-send-as")
	metadata, err = st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "gmail-send-as")
	require.NoError(err)
	assert.Len(metadata, 3)
	// A later explicit choice must work even though the inventory is unchanged.
	outcomes, changed, err = provideridentity.ApplyGmailSendAsSelection(t.Context(), st, source.ID, entries, []string{"inbox@example.test"})
	require.NoError(err)
	assert.False(changed, "the provider inventory has not changed")
	require.Len(outcomes, 1)
	assert.True(outcomes[0].Added)
	identities, err = st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Len(identities, 2)
	_, changed, err = provideridentity.ApplySnapshot(t.Context(), st, source.ID, provideridentity.GmailSnapshot(entries))
	require.NoError(err)
	assert.False(changed, "automatic unchanged-state refresh remains a no-op")
}
