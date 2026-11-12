package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFederationEnrollmentLifecycle(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	uid := strings.Repeat("a", 64)
	enrollment, token, err := st.CreateFederationEnrollmentContext(ctx, uid, "alice", "")
	require.NoError(t, err)
	assert.Equal(t, "alice", enrollment.SpokeName)
	assert.True(t, strings.HasPrefix(token, "mvf1_"))
	var stored string
	require.NoError(t, st.DB().QueryRowContext(ctx, st.Rebind("SELECT token_sha256 FROM federation_enrollments WHERE enrollment_uid = ?"), enrollment.EnrollmentUID).Scan(&stored))
	assert.NotContains(t, stored, token)
	digest := sha256.Sum256([]byte(token))
	got, err := st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), uid)
	require.NoError(t, err)
	assert.Equal(t, enrollment.EnrollmentUID, got.EnrollmentUID)
	_, err = st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), strings.Repeat("b", 64))
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, st.TouchFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	list, err := st.ListFederationEnrollmentsContext(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotNil(t, list[0].LastSeenAt)
	require.NoError(t, st.RevokeFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	require.NoError(t, st.RevokeFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	_, err = st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), uid)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, st.RevokeFederationEnrollmentContext(ctx, "missing"), store.ErrNotFound)
}

func TestFederationHubSourceGuard(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	require.NoError(t, st.ConfigureFederationHubContext(ctx))
	_, err := st.GetOrCreateSourceContext(ctx, "gmail", "federation:fixture")
	assert.ErrorIs(t, err, store.ErrFederationHubSource)
	other := testutil.NewTestStore(t)
	_, err = other.GetOrCreateSourceContext(ctx, "gmail", "fixture@example.test")
	require.NoError(t, err)
	assert.ErrorContains(t, other.ConfigureFederationHubContext(ctx), "already has 1 sources")
}

func TestFederationEnrollmentPersistsAndSchemaUpgradeKeepsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	uid, err := st.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	spoke := strings.Repeat("a", 64)
	e, token, err := st.CreateFederationEnrollmentContext(t.Context(), spoke, "alice", "")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = '41' WHERE key = 'schema_version'")
	require.NoError(t, err)
	reopened, err := store.OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.InitSchemaUnlessCurrentContext(t.Context()))
	gotUID, err := reopened.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uid, gotUID)
	digest := sha256.Sum256([]byte(token))
	got, err := reopened.AuthenticateFederationEnrollmentContext(t.Context(), hex.EncodeToString(digest[:]), spoke)
	require.NoError(t, err)
	assert.Equal(t, e.EnrollmentUID, got.EnrollmentUID)
}

func TestFederationEnrollmentValidation(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tc := range []struct{ spoke, member, name string }{
		{"bad", "alice", ""}, {strings.Repeat("a", 64), "Alice", ""}, {strings.Repeat("a", 64), "alice", strings.Repeat("x", 129)},
	} {
		_, _, err := st.CreateFederationEnrollmentContext(t.Context(), tc.spoke, tc.member, tc.name)
		assert.Error(t, err)
	}
}
