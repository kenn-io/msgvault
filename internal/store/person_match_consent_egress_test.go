package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonMatchConsentDispatchAllowsUnrelatedArchiveWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	cfg := personmatch.Config{Enabled: true, CredentialEnv: "MSGVAULT_JEV_FIXTURE", RetentionDeclaration: "fixture retention"}
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	consent, _, err := st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator", nil)
	require.NoError(err)
	allowed, err := st.PersonMatchConsentEgressContext(t.Context(), consent.DisclosureFingerprint, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = st.DB().ExecContext(ctx, `INSERT INTO participants (display_name) VALUES ('Concurrent Example')`)
		return err
	})
	require.NoError(err, "a provider request must not hold the archive write lock")
	assert.True(allowed)
	_, err = st.RevokePersonMatchConsentContext(t.Context(), consent.DisclosureFingerprint, "operator", nil)
	require.NoError(err)
	requests := 0
	allowed, err = st.PersonMatchConsentEgressContext(t.Context(), consent.DisclosureFingerprint, func() error {
		requests++
		return nil
	})
	require.NoError(err)
	assert.False(allowed)
	assert.Zero(requests)
}

func TestPersonMatchConsentRevokeWaitsOnlyForDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	cfg := personmatch.Config{Enabled: true, CredentialEnv: "MSGVAULT_JEV_FIXTURE", RetentionDeclaration: "fixture retention"}
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	consent, _, err := st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator", nil)
	require.NoError(err)
	started := make(chan struct{})
	finish := make(chan struct{})
	dispatched := make(chan error, 1)
	go func() {
		_, err := st.PersonMatchConsentEgressContext(t.Context(), consent.DisclosureFingerprint, func() error {
			close(started)
			<-finish
			return nil
		})
		dispatched <- err
	}()
	<-started
	revokeStarted := make(chan struct{})
	revoked := make(chan error, 1)
	go func() {
		close(revokeStarted)
		_, err := st.RevokePersonMatchConsentContext(t.Context(), consent.DisclosureFingerprint, "operator", func(context.Context) (func(), error) {
			select {
			case <-finish:
			default:
				assert.Fail("revocation acquired the archive gate during provider I/O")
			}
			return func() {}, nil
		})
		revoked <- err
	}()
	<-revokeStarted
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO participants (display_name) VALUES ('Unrelated Example')`)
	close(finish)
	require.NoError(err)
	require.NoError(<-dispatched)
	require.NoError(<-revoked)
	active, err := st.HasPersonMatchConsentContext(t.Context(), consent.DisclosureFingerprint)
	require.NoError(err)
	assert.False(active)
}
