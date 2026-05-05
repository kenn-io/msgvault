package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

// newIdentityCLITest creates an isolated store and test root command for
// identity subcommand tests.  Returns (store, root, stdout buffer, stderr buffer).
func newIdentityCLITest(t *testing.T) (*store.Store, *cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Save and restore package-level globals.
	savedCfg := cfg
	savedLogger := logger
	savedAccount := identityListAccount
	savedCollection := identityListCollection
	savedListJSON := identityListJSON
	savedShowJSON := identityShowJSON
	savedAddSignal := identityAddSignal
	t.Cleanup(func() {
		cfg = savedCfg
		logger = savedLogger
		identityListAccount = savedAccount
		identityListCollection = savedCollection
		identityListJSON = savedListJSON
		identityShowJSON = savedShowJSON
		identityAddSignal = savedAddSignal
		// Reset cobra's "Changed" state so mutually-exclusive flag groups
		// don't carry over between tests that share the package-level command.
		for _, name := range []string{"account", "collection", "json"} {
			if f := identityListCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		if f := identityShowCmd.Flags().Lookup("json"); f != nil {
			f.Changed = false
		}
		if f := identityAddCmd.Flags().Lookup("signal"); f != nil {
			f.Changed = false
		}
	})

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	var stdout, stderr bytes.Buffer
	root := newTestRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(identityCmd)

	return s, root, &stdout, &stderr
}

func TestIdentityList_NoScope(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	b, _ := s.GetOrCreateSource("imap", "bob@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")
	_ = s.AddAccountIdentity(b.ID, "bob@example.com", "account-identifier")

	root.SetArgs([]string{"identity", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "alice@example.com") {
		t.Errorf("missing alice in output: %s", text)
	}
	if !strings.Contains(text, "bob@example.com") {
		t.Errorf("missing bob in output: %s", text)
	}
	if !strings.Contains(text, "ACCOUNT") {
		t.Errorf("missing header in output: %s", text)
	}
}

func TestIdentityList_AccountFilter(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_, _ = s.GetOrCreateSource("imap", "bob@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "list", "--account", "alice@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "alice@example.com") {
		t.Errorf("missing alice: %s", text)
	}
	if strings.Contains(text, "bob@example.com") {
		t.Errorf("bob leaked into account-filtered output: %s", text)
	}
}

func TestIdentityList_AccountWithNoneRow(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("mbox", "old-mbox-2018")

	root.SetArgs([]string{"identity", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "(none)") {
		t.Errorf("expected (none) row for account with no identifiers: %s", text)
	}
}

func TestIdentityList_JSONShape(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json decode: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	sigs, ok := rows[0]["signals"].([]any)
	if !ok || len(sigs) != 1 || sigs[0] != "manual" {
		t.Errorf("signals=%v", rows[0]["signals"])
	}
}

func TestIdentityList_JSONEmptySignals(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "") // empty signal

	root.SetArgs([]string{"identity", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	// Unmarshal into raw JSON to check the literal value (not Go nil).
	raw := out.Bytes()
	if !strings.Contains(string(raw), `"signals": []`) {
		t.Errorf("expected signals to be [] not null; got: %s", raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("json decode: %v\n%s", err, raw)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	sigs, ok := rows[0]["signals"].([]any)
	if !ok {
		t.Errorf("signals field is not a JSON array; got %T(%v)", rows[0]["signals"], rows[0]["signals"])
	} else if len(sigs) != 0 {
		t.Errorf("want empty signals array, got %v", sigs)
	}
}

func TestIdentityShow_Populated(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")

	root.SetArgs([]string{"identity", "show", "alice@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice@example.com") {
		t.Errorf("missing alice: %s", out.String())
	}
}

func TestIdentityShow_Empty(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "show", "alice@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "(none)") {
		t.Errorf("missing (none) row: %s", text)
	}
	if !strings.Contains(text, "identity add") {
		t.Errorf("missing hint: %s", text)
	}
}

func TestIdentityShow_UnknownAccount(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t)
	root.SetArgs([]string{"identity", "show", "ghost@example.com"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIdentityShow_JSONShape(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "show", "alice@example.com", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json decode: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	if rows[0]["account"] != "alice@example.com" {
		t.Errorf("account=%v", rows[0]["account"])
	}
	if rows[0]["identifier"] != "alice@example.com" {
		t.Errorf("identifier=%v", rows[0]["identifier"])
	}
	sigs, ok := rows[0]["signals"].([]any)
	if !ok {
		t.Errorf("signals field is not a JSON array; got %T(%v)", rows[0]["signals"], rows[0]["signals"])
	} else if len(sigs) != 1 || sigs[0] != "manual" {
		t.Errorf("signals=%v", sigs)
	}
}

func TestIdentityShow_JSONEmpty(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "show", "alice@example.com", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json decode: %v\n%s", err, out.String())
	}
	if len(rows) != 0 {
		t.Fatalf("want empty slice, got %d rows: %+v", len(rows), rows)
	}
}

func TestIdentityAdd_FirstTime(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Added extra@example.com") {
		t.Errorf("missing add confirmation: %s", out.String())
	}
}

func TestIdentityAdd_IdempotentSameSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already confirmed") {
		t.Errorf("missing idempotent confirmation: %s", out.String())
	}
}

func TestIdentityAdd_AdditionalSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com",
		"--signal", "account-identifier"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additional signal") {
		t.Errorf("missing additional-signal confirmation: %s", out.String())
	}
}

func TestIdentityAdd_RejectsCommaInSignal(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "foo@example.com",
		"--signal", "a,b"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "comma") {
		t.Fatalf("want comma error, got %v", err)
	}
}

func TestIdentityAdd_RejectsEmptyIdentifier(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "   "})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want empty-identifier error, got %v", err)
	}
}

func TestIdentityAdd_RejectsCollectionAsAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_, _ = s.CreateCollection("team", "", []int64{a.ID})

	root.SetArgs([]string{"identity", "add", "team", "extra@example.com"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "collection") {
		t.Fatalf("want collection-rejection error, got %v", err)
	}
}

func TestIdentityRemove_Hit(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "extra@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Removed extra@example.com") {
		t.Errorf("missing remove confirmation: %s", out.String())
	}
}

func TestIdentityRemove_Miss(t *testing.T) {
	s, root, out, errOut := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error on miss")
	}
	combined := out.String() + errOut.String() + err.Error()
	if !strings.Contains(combined, "Currently confirmed:") {
		t.Errorf("error should hint at present identifiers: %s", combined)
	}
}

func TestIdentityRemove_MissOnEmptyAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error on miss")
	}
	if !strings.Contains(err.Error(), "no confirmed identifiers") {
		t.Errorf("missing zero-identifiers explanation: %v", err)
	}
}

func TestIdentityRemove_WhitespaceIdentifier(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t)

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "   "})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for whitespace identifier")
	}
	if !strings.Contains(err.Error(), "identifier must not be empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIdentityRemove_LastIdentifierWarns(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "alice@example.com"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no confirmed identity") {
		t.Errorf("missing degraded-dedup warning: %s", out.String())
	}
}

func TestIdentityList_CollectionFilter(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	b, _ := s.GetOrCreateSource("gmail", "bob@example.com")
	c, _ := s.GetOrCreateSource("gmail", "carol@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")
	_ = s.AddAccountIdentity(b.ID, "bob@example.com", "account-identifier")
	_ = s.AddAccountIdentity(c.ID, "carol@example.com", "account-identifier")

	_, err := s.CreateCollection("team", "", []int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}

	root.SetArgs([]string{"identity", "list", "--collection", "team"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "alice@example.com") {
		t.Errorf("missing alice in collection output: %s", text)
	}
	if !strings.Contains(text, "bob@example.com") {
		t.Errorf("missing bob in collection output: %s", text)
	}
	if strings.Contains(text, "carol@example.com") {
		t.Errorf("carol leaked into collection-filtered output: %s", text)
	}
}
