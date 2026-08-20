package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonInferenceConsentSchemaEnforcesAuditState(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by, revoked_by)
		VALUES (?, 'cli', 'cli')`), profile.Fingerprint)
	require.Error(err, "revocation actor without timestamp must fail")

	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'cli')`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.Error(err, "only one active consent is allowed")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_inference_consents
		SET revoked_by = 'cli', revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.NoError(err, "a revoked consent must not block regrant")
}
