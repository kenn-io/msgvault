package imap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxIMAPCreateFolderNativeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, fault, folder string
		wantWrites          int64
		wantError           bool
	}{
		{name: "create", folder: "Triage", wantWrites: 1},
		{name: "already exists", folder: "Archive"},
		{name: "lost reply", fault: "lost CREATE reply", folder: "Triage", wantWrites: 1, wantError: true},
		{name: "missing epoch", fault: "missing created epoch", folder: "Triage", wantWrites: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeFixture(t, tc.fault)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &f.source, Destination: &inboxcontrol.Folder{Name: tc.folder}, DryRun: true}
			before, err := f.provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := f.provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			assertions.Zero(f.creates.Load())
			request.DryRun = false
			dispatch, err := f.provider.Dispatch(t.Context(), request, before)
			assertions.Equal(tc.wantWrites, f.creates.Load())
			if tc.wantError {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Nil(dispatch.Folder)
				return
			}
			requirements.NoError(err)
			requirements.NotNil(dispatch.Folder)
			assertions.Equal(tc.folder, dispatch.Folder.ID)
			assertions.NotZero(dispatch.Folder.UIDValidity)
			read := request
			read.ResolvedFolder = dispatch.Folder
			after, err := f.provider.Observe(t.Context(), read)
			requirements.NoError(err)
			requirements.NoError(f.provider.Verify(read, before, projected, after))
			assertions.Equal(tc.wantWrites, f.creates.Load())
			wrong := *dispatch.Folder
			wrong.UIDValidity++
			read.ResolvedFolder = &wrong
			_, err = f.provider.Observe(t.Context(), read)
			requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
			// Name-only evidence cannot settle a receipt without authenticated dispatch identity.
			read.ResolvedFolder = nil
			requirements.ErrorIs(f.provider.Verify(read, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
		})
	}
}
