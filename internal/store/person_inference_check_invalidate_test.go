package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInvalidatePersonInferenceCheckRemovesExactCheck(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode,
		ModelVersion: profile.Model,
	}))
	changed, err := st.InvalidatePersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(changed)
	verified, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(verified)
	changed, err = st.InvalidatePersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(changed)
}
