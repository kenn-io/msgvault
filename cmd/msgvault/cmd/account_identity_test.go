package cmd

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestConfirmDefaultIdentity_HappyPath(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	confirmDefaultIdentity(io.Discard, s, src.ID, "alice@example.com", "alice@example.com", "account-identifier")
	rows, err := s.ListAccountIdentities(src.ID)
	require.NoError(err)
	require.Len(rows, 1, "got %+v", rows)
	require.Equal("alice@example.com", rows[0].Address, "got %+v", rows)
	assertpkg.Equal(t, "account-identifier", rows[0].SourceSignal, "signal")
}

func TestConfirmDefaultIdentity_EmptyIdentifierIsNoOp(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	confirmDefaultIdentity(io.Discard, s, src.ID, "alice@example.com", "", "account-identifier")
	rows, _ := s.ListAccountIdentities(src.ID)
	assertpkg.Empty(t, rows, "want empty, got %+v", rows)
}

func TestConfirmDefaultIdentity_StoreErrorDoesNotPanic(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	requirepkg.NoError(t, err)
	defer func() { _ = s.Close() }()
	requirepkg.NoError(t, s.InitSchema())

	savedLogger := logger
	defer func() { logger = savedLogger }()
	logger = slog.New(slog.DiscardHandler)

	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// sourceID 99999 does not exist; FK violation returns an error
	// from AddAccountIdentity. The helper must swallow it.
	confirmDefaultIdentity(io.Discard, s, 99999, "ghost@example.com", "ghost@example.com", "account-identifier")
}

// TestConfirmDefaultIdentity_LegacyMigrationOverridesNoDefault pins the
// documented behavior: skipping confirmDefaultIdentity (simulating
// --no-default-identity) does NOT prevent MigrateLegacyIdentityConfig from
// writing the address.
func TestConfirmDefaultIdentity_LegacyMigrationOverridesNoDefault(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	_, err = s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	// Simulate --no-default-identity: do not call confirmDefaultIdentity.
	// Then run startup migrations with a non-empty legacy address list.
	applied, _, _, _, err := s.MigrateLegacyIdentityConfig([]string{"alice@example.com"}) //nolint:dogsled // 5-return migration; test needs only applied+err
	require.NoError(err)
	require.True(applied, "migration did not apply")
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	rows, _ := s.ListAccountIdentities(src.ID)
	require.Len(rows, 1, "legacy migration should have written, got %+v", rows)
}
