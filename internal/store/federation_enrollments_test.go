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
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	uid := strings.Repeat("a", 64)
	enrollment, token, err := st.CreateFederationEnrollmentContext(ctx, uid, "alice", "")
	requirements.NoError(err)
	assertions.Equal("alice", enrollment.SpokeName)
	assertions.True(strings.HasPrefix(token, "mvf1_"))
	var stored string
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind("SELECT token_sha256 FROM federation_enrollments WHERE enrollment_uid = ?"), enrollment.EnrollmentUID).Scan(&stored))
	assertions.NotContains(stored, token)
	digest := sha256.Sum256([]byte(token))
	got, err := st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), uid)
	requirements.NoError(err)
	assertions.Equal(enrollment.EnrollmentUID, got.EnrollmentUID)
	_, err = st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), strings.Repeat("b", 64))
	requirements.ErrorIs(err, store.ErrNotFound)
	requirements.NoError(st.TouchFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	list, err := st.ListFederationEnrollmentsContext(ctx)
	requirements.NoError(err)
	requirements.Len(list, 1)
	assertions.NotNil(list[0].LastSeenAt)
	requirements.NoError(st.RevokeFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	requirements.NoError(st.RevokeFederationEnrollmentContext(ctx, enrollment.EnrollmentUID))
	_, err = st.AuthenticateFederationEnrollmentContext(ctx, hex.EncodeToString(digest[:]), uid)
	requirements.ErrorIs(err, store.ErrNotFound)
	requirements.ErrorIs(st.RevokeFederationEnrollmentContext(ctx, "missing"), store.ErrNotFound)
}

func TestFederationHubSourceGuard(t *testing.T) {
	requirements := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	requirements.NoError(st.ConfigureFederationHubContext(ctx))
	_, err := st.GetOrCreateSourceContext(ctx, "gmail", "federation:fixture")
	requirements.ErrorIs(err, store.ErrFederationHubSource)
	other := testutil.NewTestStore(t)
	_, err = other.GetOrCreateSourceContext(ctx, "gmail", "fixture@example.test")
	requirements.NoError(err)
	requirements.ErrorContains(other.ConfigureFederationHubContext(ctx), "already has 1 sources")
}

func TestFederationEnrollmentPersistsAndSchemaUpgrade(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	spoke := strings.Repeat("a", 64)
	e, token, err := st.CreateFederationEnrollmentContext(t.Context(), spoke, "alice", "")
	requirements.NoError(err)
	reopened, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(reopened.Close()) })
	requirements.NoError(reopened.InitSchemaUnlessCurrentContext(t.Context()))
	digest := sha256.Sum256([]byte(token))
	got, err := reopened.AuthenticateFederationEnrollmentContext(t.Context(), hex.EncodeToString(digest[:]), spoke)
	requirements.NoError(err)
	assertions.Equal(e.EnrollmentUID, got.EnrollmentUID)
	_, err = reopened.DB().ExecContext(t.Context(), "DROP TABLE federation_enrollments")
	requirements.NoError(err)
	_, err = reopened.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = '41' WHERE key = 'schema_version'")
	requirements.NoError(err)
	upgraded, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(upgraded.Close()) })
	requirements.NoError(upgraded.InitSchemaUnlessCurrentContext(t.Context()))
	_, _, err = upgraded.CreateFederationEnrollmentContext(t.Context(), spoke, "alice", "")
	requirements.NoError(err)
}

func TestFederationEnrollmentValidation(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	for _, tc := range []struct{ spoke, member, name, field string }{
		{"bad", "alice", "", "spoke_instance"},
		{strings.Repeat("a", 64), "Alice", "", "member"},
		{strings.Repeat("a", 64), "alice", strings.Repeat("x", 129), "spoke_name"},
		{strings.Repeat("a", 64), "alice", "name\nother", "spoke_name"},
		{strings.Repeat("a", 64), "alice", "name\tother", "spoke_name"},
		{strings.Repeat("a", 64), "alice", "\x1b[2J", "spoke_name"},
		{strings.Repeat("a", 64), "alice", "name\u009b", "spoke_name"},
	} {
		_, _, err := st.CreateFederationEnrollmentContext(t.Context(), tc.spoke, tc.member, tc.name)
		requirements.ErrorIs(err, store.ErrFederationValidation)
		requirements.ErrorContains(err, tc.field)
	}
}
