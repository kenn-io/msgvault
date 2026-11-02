package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/testutil"
)

func mailKindDisclosure(t *testing.T) mailkind.Disclosure {
	t.Helper()
	cfg := mailkind.Config{Enabled: true, RetentionDeclaration: "synthetic retention policy"}
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	require.NoError(t, err)
	return disclosure
}

func TestMailKindConsentPurposeAndPolicy(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	disclosure := mailKindDisclosure(t)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	active, err := st.HasMailKindConsentContext(ctx, fingerprint)
	require.NoError(t, err)
	assert.False(t, active)
	// Even the exact same fingerprint under another purpose grants nothing.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO provider_consents
		(purpose, id, fingerprint, granted_by) VALUES ('people_inference', 1, ?, 'fixture')`), fingerprint)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_match_consents
		(disclosure_fingerprint, endpoint, model_id, packet_schema, retention_declaration, policy_version, question_version, granted_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'fixture')`), fingerprint, disclosure.Endpoint, disclosure.ModelID,
		disclosure.PacketSchema, disclosure.RetentionDeclaration, disclosure.PolicyVersion, disclosure.QuestionVersion)
	require.NoError(t, err)
	first, changed, err := st.GrantMailKindConsentContext(ctx, disclosure, "fixture", nil)
	require.NoError(t, err)
	assert.True(t, changed)
	again, changed, err := st.GrantMailKindConsentContext(ctx, disclosure, "fixture", nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, first.ID, again.ID)
	active, err = st.HasMailKindConsentContext(ctx, fingerprint)
	require.NoError(t, err)
	assert.True(t, active)
	changedDisclosure := disclosure
	changedDisclosure.RetentionDeclaration += " changed"
	changedFingerprint, err := changedDisclosure.Fingerprint()
	require.NoError(t, err)
	active, err = st.HasMailKindConsentContext(ctx, changedFingerprint)
	require.NoError(t, err)
	assert.False(t, active)
	changed, err = st.RevokeMailKindConsentContext(ctx, fingerprint, "fixture", nil)
	require.NoError(t, err)
	assert.True(t, changed)
	called := false
	allowed, err := st.MailKindConsentEgressContext(ctx, fingerprint, func() error { called = true; return nil })
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.False(t, called)
	var otherActive int
	require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT count(*) FROM provider_consents
		WHERE purpose = 'people_inference' AND revoked_at IS NULL`).Scan(&otherActive))
	assert.Equal(t, 1, otherActive)
	second, changed, err := st.GrantMailKindConsentContext(ctx, disclosure, "fixture", nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Greater(t, second.ID, first.ID)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE mail_kind_profiles SET endpoint = 'https://changed.example' WHERE fingerprint = ?`), fingerprint)
	require.NoError(t, err)
	allowed, err = st.MailKindConsentEgressContext(ctx, fingerprint, func() error { called = true; return nil })
	require.Error(t, err)
	assert.False(t, allowed)
	assert.False(t, called)
}

func TestMailKindRevokeWaitsForDispatch(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	disclosure := mailKindDisclosure(t)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	_, _, err = st.GrantMailKindConsentContext(ctx, disclosure, "fixture", nil)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := st.MailKindConsentEgressContext(ctx, fingerprint, func() error {
			close(entered)
			<-release
			return nil
		})
		dispatchDone <- dispatchErr
	}()
	<-entered
	revoking, revoked := make(chan struct{}), make(chan error, 1)
	go func() {
		close(revoking)
		_, revokeErr := st.RevokeMailKindConsentContext(ctx, fingerprint, "fixture", nil)
		revoked <- revokeErr
	}()
	<-revoking
	// The grant must still be active while dispatch holds the reader lock.
	active, err := st.HasMailKindConsentContext(ctx, fingerprint)
	require.NoError(t, err)
	assert.True(t, active)
	close(release)
	require.NoError(t, <-dispatchDone)
	require.NoError(t, <-revoked)
	called := false
	allowed, err := st.MailKindConsentEgressContext(ctx, fingerprint, func() error { called = true; return nil })
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.False(t, called)
	// Archive mutation admission is checked before recording a grant.
	_, _, err = st.GrantMailKindConsentContext(ctx, disclosure, "fixture", func(context.Context) (func(), error) {
		return nil, errors.New("fixture operation gate busy")
	})
	require.ErrorContains(t, err, "gate busy")
}
