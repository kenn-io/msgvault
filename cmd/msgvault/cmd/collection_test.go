package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

func TestCollectionShowPrintsReadableSourceNames(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	tmpDir := t.TempDir()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create alice source: %v", err)
	}
	if err := st.UpdateSourceDisplayName(alice.ID, "Personal"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	bob, err := st.GetOrCreateSource("imap", "bob@example.com")
	if err != nil {
		t.Fatalf("create bob source: %v", err)
	}
	if _, err := st.CreateCollection("team", "", []int64{alice.ID, bob.ID}); err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	done := captureStdout(t)
	if err := runCollectionShow(&cobra.Command{}, []string{"team"}); err != nil {
		t.Fatalf("runCollectionShow: %v", err)
	}
	out := done()

	if !strings.Contains(out, "Personal (id ") {
		t.Fatalf("missing display name in output:\n%s", out)
	}
	if !strings.Contains(out, "bob@example.com (id ") {
		t.Fatalf("missing identifier in output:\n%s", out)
	}
}

func TestResolveAccountListRejectsMissingNumericID(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	ids, err := resolveAccountList(st, fmt.Sprintf("%d", src.ID))
	if err != nil {
		t.Fatalf("resolveAccountList(existing id): %v", err)
	}
	if len(ids) != 1 || ids[0] != src.ID {
		t.Fatalf("resolveAccountList(existing id) = %v, want [%d]", ids, src.ID)
	}

	// "999999" is neither an existing source ID nor an existing
	// identifier/display name, so resolveAccountList errors via the
	// final ResolveAccountFlag fall-through. Iter12 codex flagged that
	// the prior shape errored *before* the fall-through, so a numeric
	// identifier (e.g. unprefixed phone "15551234567") that wasn't a
	// source ID would never get a chance to match by identifier. The
	// test below asserts the fall-through path is reachable.
	if _, err := resolveAccountList(st, "999999"); err == nil {
		t.Fatal("expected error for missing numeric source ID, got nil")
	}
}

// TestResolveAccountListNumericFallthroughResolvesIdentifier verifies
// that a plain-digit token that does NOT match a source ID falls
// through to identifier resolution. Regression test for iter12 codex
// Low: previously, a numeric identifier (e.g. an unprefixed phone
// number) that happened to not match a source ID would error
// immediately instead of being looked up by identifier.
func TestResolveAccountListNumericFallthroughResolvesIdentifier(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Create a source with a numeric identifier that is unlikely to
	// collide with the auto-assigned source ID. Use a 12-digit string
	// (way past any plausible primary-key value) so the test stays
	// stable.
	phoneIdentifier := "987654321098"
	src, err := st.GetOrCreateSource("whatsapp", phoneIdentifier)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if fmt.Sprintf("%d", src.ID) == phoneIdentifier {
		t.Fatalf("test assumption broken: source id %d collides with identifier", src.ID)
	}

	ids, err := resolveAccountList(st, phoneIdentifier)
	if err != nil {
		t.Fatalf("resolveAccountList(numeric identifier): %v", err)
	}
	if len(ids) != 1 || ids[0] != src.ID {
		t.Fatalf("resolveAccountList(numeric identifier) = %v, want [%d]", ids, src.ID)
	}
}
