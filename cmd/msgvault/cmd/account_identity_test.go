package cmd

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/wesm/msgvault/internal/store"
)

func TestConfirmDefaultIdentity_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	confirmDefaultIdentity(io.Discard, s, src.ID, "alice@example.com", "alice@example.com", "account-identifier")
	rows, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Address != "alice@example.com" {
		t.Fatalf("got %+v", rows)
	}
	if rows[0].SourceSignal != "account-identifier" {
		t.Errorf("signal=%q", rows[0].SourceSignal)
	}
}

func TestConfirmDefaultIdentity_EmptyIdentifierIsNoOp(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	confirmDefaultIdentity(io.Discard, s, src.ID, "alice@example.com", "", "account-identifier")
	rows, _ := s.ListAccountIdentities(src.ID)
	if len(rows) != 0 {
		t.Errorf("want empty, got %+v", rows)
	}
}

func TestConfirmDefaultIdentity_StoreErrorDoesNotPanic(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}

	savedLogger := logger
	defer func() { logger = savedLogger }()
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}

	_, err = s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate --no-default-identity: do not call confirmDefaultIdentity.
	// Then run startup migrations with a non-empty legacy address list.
	applied, _, _, _, err := s.MigrateLegacyIdentityConfig([]string{"alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("migration did not apply")
	}
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ListAccountIdentities(src.ID)
	if len(rows) != 1 {
		t.Fatalf("legacy migration should have written, got %+v", rows)
	}
}
