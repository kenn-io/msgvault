package api

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
)

func TestICloudOfflineScheduleChangeKeepsCurrentAndRetainedRuntimeUsable(t *testing.T) {
	for _, name := range []string{"default", "work"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newICloudAccountFixture(t, name)
			_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
			require.NoError(err)
			retained := f.controller.Current()
			req := iCloudAccountRequest()
			req.BaseURL, req.Password, req.Schedule = "https://p42-contacts.icloud.com", "", "0 */6 * * *"
			f.discovered = nil
			_, err = f.controller.Save(t.Context(), req)
			require.NoError(err)
			assert.Empty(f.discovered, "schedule changes stay offline")
			_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
			require.NoError(err)
			_, err = retained.Sync(t.Context(), carddav.SyncOptions{})
			require.NoError(err)
			assert.Equal(2, f.syncCalls)
			assert.Equal([]string{req.BaseURL, req.BaseURL}, f.discovered)
		})
	}
}

func TestICloudHealthyProbeRechecksRotatedCredentialBeforeSync(t *testing.T) {
	f := newICloudAccountFixture(t)
	_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(t, err)
	f.discoverHook = func(string) {
		credential, loadErr := carddav.LoadCredential(f.controller.cfg.TokensDir())
		require.NoError(t, loadErr)
		credential.Password = "rotated-synthetic-password"
		require.NoError(t, carddav.SaveCredential(f.controller.cfg.TokensDir(), credential))
	}
	_, err = f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(t, err, carddav.ErrCredentialNotBound)
	assert.Zero(t, f.syncCalls)
}

func TestICloudCancellationDominatesJoinedDialError(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic connection failure")}
		assert.False(t, iCloudHostFailure(t.Context(), errors.Join(dial, cause)))
	}
}
