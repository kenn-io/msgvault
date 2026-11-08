package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"hegel.dev/go/hegel"
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

func TestICloudSafetyCauseDominatesJoinedDialErrorProperty(t *testing.T) {
	certificate := &x509.Certificate{DNSNames: []string{"other.example"}}
	causes := []error{carddav.ErrOperationLimit, carddav.ErrResponseLimit, carddav.ErrUnsafeTarget, carddav.ErrUnsafeHref,
		carddav.ErrUnsafeRedirect, context.Canceled, context.DeadlineExceeded,
		x509.UnknownAuthorityError{Cert: certificate}, x509.HostnameError{Certificate: certificate, Host: "synthetic.example"},
		x509.CertificateInvalidError{Cert: certificate, Reason: x509.Expired}, tls.RecordHeaderError{Msg: "synthetic TLS failure"}}
	hegel.Test(t, func(ht *hegel.T) {
		cause := hegel.Draw(ht, hegel.SampledFrom(causes))
		text := hegel.Draw(ht, hegel.Text())
		dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic connection failure")}
		wrapped := fmt.Errorf("%s: %w", text, errors.Join(dial, cause))
		assert.False(ht, iCloudHostFailure(ht.Context(), wrapped))
	}, hegel.WithDatabase(""))
}

func TestICloudOperationLimitDominatesJoinedDialError(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic connection failure")}
	assert.False(t, iCloudHostFailure(t.Context(), errors.Join(dial, carddav.ErrOperationLimit)))
}
