package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/syncerr"
)

func TestEmailPushFailureDiagnosticsRedactionProperty(t *testing.T) {
	requirements := require.New(t)
	st, cfg, src := emailPushFixture(t)
	s := scheduler.New(func(context.Context, string) error { return nil }).WithLogger(slog.New(slog.DiscardHandler))
	_, errs := s.AddAccountsFromConfig(cfg)
	requirements.Empty(errs)
	bindings, err := emailPushBindings(t.Context(), cfg, st, s)
	requirements.NoError(err)
	t.Cleanup(func() { <-s.Stop().Done() })
	hegel.Test(t, func(ht *hegel.T) {
		assertions := assert.New(ht)
		requirements := require.New(ht)
		secret := "synthetic-provider:" + hegel.Draw(ht, hegel.Text())
		for _, tc := range []struct {
			cause  error
			reason string
		}{
			{errors.New(secret), "provider synchronization failed"},
			{&net.DNSError{Err: secret, Name: "mail.example.test", IsTimeout: true}, "transient network"},
			{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New(secret)}, "transient network"},
			{errors.New("dial tcp: " + secret), "provider synchronization failed"},
			{errors.Join(errors.New(secret), context.DeadlineExceeded), "deadline exceeded"},
			{provideridentity.NewGmailCredentialError(provideridentity.GmailAuthorizationRevoked, errors.New(secret)), "expired or been revoked"},
			{errors.Join(errors.New(secret), context.Canceled), "canceled"},
		} {
			s.WithAccountWakeSyncFunc(emailPushWakeSync(bindings, st, func(context.Context, *store.Source) (*gmail.SyncSummary, error) { return nil, tc.cause }))
			err := s.WakeAccountAndWait(t.Context(), src.Identifier)
			requirements.ErrorIs(err, errEmailPushIncomplete)
			requirements.ErrorContains(err, tc.reason)
			requirements.ErrorContains(err, "source "+strconv.FormatInt(src.ID, 10))
			assertions.NotContains(err.Error(), secret)
			assertions.Equal(syncerr.IsTransientNetwork(tc.cause), syncerr.IsTransientNetwork(err), "receipt preserves provider network classification")
			status := s.Status()
			requirements.Len(status, 1)
			assertions.Contains(status[0].LastError, tc.reason)
			assertions.NotContains(status[0].LastError, secret)
		}
	})
}

func TestEmailPushClientFailureDiagnostics(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, cfg, _ := emailPushFixture(t)
	src, err := st.GetOrCreateSource(sourceTypeGmail, "synthetic@example.test")
	requirements.NoError(err)
	secret := "synthetic-private-credential-details"
	getManager := func(context.Context, string) (*oauth.Manager, error) {
		return nil, provideridentity.NewGmailCredentialError(provideridentity.GmailAuthorizationRevoked, errors.New(secret))
	}
	state := &invocation{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	summary, err := runEmailPushSource(t.Context(), src, st, state, getManager)
	assertions.Nil(summary)
	requirements.ErrorIs(err, errEmailPushIncomplete)
	requirements.ErrorContains(err, "expired or been revoked")
	requirements.ErrorContains(err, "source "+strconv.FormatInt(src.ID, 10))
	assertions.NotContains(err.Error(), secret)
}
