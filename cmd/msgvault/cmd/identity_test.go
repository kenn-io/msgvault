package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// newIdentityCLITest creates an isolated store and test root command for
// identity subcommand tests.  Returns (store, root, stdout buffer, stderr buffer).
func newIdentityCLITest(t *testing.T) (*store.Store, *cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	s, err := store.Open(dbPath)
	requirepkg.NoError(t, err)
	requirepkg.NoError(t, s.InitSchema())
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
	logger = slog.New(slog.DiscardHandler)

	var stdout, stderr bytes.Buffer
	root := newTestRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(identityCmd)

	return s, root, &stdout, &stderr
}

func TestIdentityList_NoScope(t *testing.T) {
	assert := assertpkg.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	b, _ := s.GetOrCreateSource("imap", "bob@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")
	_ = s.AddAccountIdentity(b.ID, "bob@example.com", "account-identifier")

	root.SetArgs([]string{"identity", "list"})
	requirepkg.NoError(t, root.Execute())
	text := out.String()
	assert.Contains(text, "alice@example.com", "missing alice")
	assert.Contains(text, "bob@example.com", "missing bob")
	assert.Contains(text, "ACCOUNT", "missing header")
}

func TestIdentityList_AccountFilter(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_, _ = s.GetOrCreateSource("imap", "bob@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "list", "--account", "alice@example.com"})
	requirepkg.NoError(t, root.Execute())
	text := out.String()
	assertpkg.Contains(t, text, "alice@example.com", "missing alice")
	assertpkg.NotContains(t, text, "bob@example.com", "bob leaked into account-filtered output")
}

func TestIdentityList_AccountWithNoneRow(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("mbox", "old-mbox-2018")

	root.SetArgs([]string{"identity", "list"})
	requirepkg.NoError(t, root.Execute())
	text := out.String()
	assertpkg.Contains(t, text, "(none)", "expected (none) row for account with no identifiers")
}

func TestIdentityList_JSONShape(t *testing.T) {
	require := requirepkg.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "list", "--json"})
	require.NoError(root.Execute())
	var rows []map[string]any
	require.NoError(json.Unmarshal(out.Bytes(), &rows), "json decode (out=%s)", out.String())
	require.Len(rows, 1, "got rows %+v", rows)
	sigs, ok := rows[0]["signals"].([]any)
	require.True(ok && len(sigs) == 1 && sigs[0] == "manual", "signals=%v", rows[0]["signals"])
}

func TestIdentityList_JSONEmptySignals(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "") // empty signal

	root.SetArgs([]string{"identity", "list", "--json"})
	require.NoError(root.Execute())
	// Unmarshal into raw JSON to check the literal value (not Go nil).
	raw := out.Bytes()
	assert.Contains(string(raw), `"signals": []`, "expected signals to be [] not null")
	var rows []map[string]any
	require.NoError(json.Unmarshal(raw, &rows), "json decode (raw=%s)", raw)
	require.Len(rows, 1)
	sigs, ok := rows[0]["signals"].([]any)
	require.True(ok, "signals field is not a JSON array; got %T(%v)", rows[0]["signals"], rows[0]["signals"])
	assert.Empty(sigs, "want empty signals array")
}

func TestIdentityShow_Populated(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")

	root.SetArgs([]string{"identity", "show", "alice@example.com"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "alice@example.com", "missing alice")
}

func TestIdentityShow_Empty(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "show", "alice@example.com"})
	requirepkg.NoError(t, root.Execute())
	text := out.String()
	assertpkg.Contains(t, text, "(none)", "missing (none) row")
	assertpkg.Contains(t, text, "identity add", "missing hint")
}

func TestIdentityShow_UnknownAccount(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t) //nolint:dogsled // helper returns 4 values; test needs only root
	root.SetArgs([]string{"identity", "show", "ghost@example.com"})
	err := root.Execute()
	requirepkg.Error(t, err)
}

func TestIdentityShow_JSONShape(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "show", "alice@example.com", "--json"})
	require.NoError(root.Execute())
	var rows []map[string]any
	require.NoError(json.Unmarshal(out.Bytes(), &rows), "json decode (out=%s)", out.String())
	require.Len(rows, 1, "got rows %+v", rows)
	assert.Equal("alice@example.com", rows[0]["account"], "account")
	assert.Equal("alice@example.com", rows[0]["identifier"], "identifier")
	sigs, ok := rows[0]["signals"].([]any)
	require.True(ok, "signals field is not a JSON array; got %T(%v)", rows[0]["signals"], rows[0]["signals"])
	require.Len(sigs, 1, "signals=%v", sigs)
	assert.Equal("manual", sigs[0], "signals[0]")
}

func TestIdentityShow_JSONEmpty(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "show", "alice@example.com", "--json"})
	requirepkg.NoError(t, root.Execute())
	var rows []map[string]any
	requirepkg.NoError(t, json.Unmarshal(out.Bytes(), &rows), "json decode (out=%s)", out.String())
	requirepkg.Empty(t, rows, "got rows %+v", rows)
}

func TestIdentityAdd_FirstTime(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "Added extra@example.com", "missing add confirmation")
}

func TestIdentityAdd_IdempotentSameSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "already confirmed", "missing idempotent confirmation")
}

func TestIdentityAdd_AdditionalSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com",
		"--signal", "account-identifier"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "additional signal", "missing additional-signal confirmation")
}

func TestIdentityAdd_RejectsCommaInSignal(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "foo@example.com",
		"--signal", "a,b"})
	err := root.Execute()
	requirepkg.Error(t, err, "want comma error")
	requirepkg.ErrorContains(t, err, "comma")
}

func TestIdentityAdd_RejectsEmptyIdentifier(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "   "})
	err := root.Execute()
	requirepkg.Error(t, err, "want empty-identifier error")
	requirepkg.ErrorContains(t, err, "empty")
}

func TestIdentityAdd_RejectsCollectionAsAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_, _ = s.CreateCollection("team", "", []int64{a.ID})

	root.SetArgs([]string{"identity", "add", "team", "extra@example.com"})
	err := root.Execute()
	requirepkg.Error(t, err, "want collection-rejection error")
	requirepkg.ErrorContains(t, err, "collection")
}

func TestIdentityRemove_Hit(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "extra@example.com"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "Removed extra@example.com", "missing remove confirmation")
}

func TestIdentityRemove_Miss(t *testing.T) {
	s, root, out, errOut := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error on miss")
	combined := out.String() + errOut.String() + err.Error()
	assertpkg.Contains(t, combined, "Currently confirmed:", "error should hint at present identifiers")
}

func TestIdentityRemove_MissOnEmptyAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error on miss")
	assertpkg.ErrorContains(t, err, "no confirmed identifiers")
}

func TestIdentityRemove_WhitespaceIdentifier(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t) //nolint:dogsled // helper returns 4 values; test needs only root

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "   "})
	err := root.Execute()
	requirepkg.Error(t, err, "expected error for whitespace identifier")
	assertpkg.ErrorContains(t, err, "identifier must not be empty")
}

func TestIdentityRemove_LastIdentifierWarns(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "alice@example.com"})
	requirepkg.NoError(t, root.Execute())
	assertpkg.Contains(t, out.String(), "no confirmed identity", "missing degraded-dedup warning")
}

func TestIdentityList_CollectionFilter(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	b, _ := s.GetOrCreateSource("gmail", "bob@example.com")
	c, _ := s.GetOrCreateSource("gmail", "carol@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")
	_ = s.AddAccountIdentity(b.ID, "bob@example.com", "account-identifier")
	_ = s.AddAccountIdentity(c.ID, "carol@example.com", "account-identifier")

	_, err := s.CreateCollection("team", "", []int64{a.ID, b.ID})
	require.NoError(err)

	root.SetArgs([]string{"identity", "list", "--collection", "team"})
	require.NoError(root.Execute())
	text := out.String()
	assert.Contains(text, "alice@example.com", "missing alice in collection output")
	assert.Contains(text, "bob@example.com", "missing bob in collection output")
	assert.NotContains(text, "carol@example.com", "carol leaked into collection-filtered output")
}
