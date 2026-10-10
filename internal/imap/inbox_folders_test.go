package imap

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxIMAPCreateFolderUsesNativeEpochAndExistingNoop(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var creates atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{creates: &creates})
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Followups"}, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	requirements.NotNil(projected.ProvisionedFolder)
	assertions.Empty(projected.ProvisionedFolder.ID)
	assertions.Zero(creates.Load())
	unchanged, err := provider.Folders(t.Context(), source)
	requirements.NoError(err)
	requirements.Len(unchanged, 1)
	dispatch, err := provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	requirements.NotNil(dispatch.Folder)
	assertions.Equal("Followups", dispatch.Folder.ID)
	assertions.NotZero(dispatch.Folder.UIDValidity)
	readback := request
	readback.ResolvedFolder = dispatch.Folder
	after, err := provider.Observe(t.Context(), readback)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(readback, before, projected, after))
	assertions.Equal(int64(1), creates.Load())
	// Another signed operation on the observed existing mailbox is a no-op.
	existing, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	existingProjection, err := provider.Preview(t.Context(), request, existing)
	requirements.NoError(err)
	assertions.Equal(dispatch.Folder, existingProjection.ProvisionedFolder)
	again, err := provider.Dispatch(t.Context(), request, existing)
	requirements.NoError(err)
	assertions.Equal(dispatch.Folder, again.Folder)
	assertions.Equal(int64(1), creates.Load())
	stale := *dispatch.Folder
	stale.UIDValidity++
	readback.ResolvedFolder = &stale
	_, err = provider.Observe(t.Context(), readback)
	assertions.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
}

func TestInboxIMAPCreateWithoutEpochEvidenceStaysUnknown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var creates atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{creates: &creates, rejectStatus: "Followups"})
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Followups"}, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	_, err = provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	result, err := provider.Dispatch(t.Context(), request, before)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	assertions.Nil(result.Folder)
	assertions.Equal(int64(1), creates.Load())
	// Actual CREATE succeeded; the independent catalog cannot prove its epoch.
	_, err = provider.Observe(t.Context(), request)
	require.Error(t, err)
	assertions.Equal(int64(1), creates.Load())
}
