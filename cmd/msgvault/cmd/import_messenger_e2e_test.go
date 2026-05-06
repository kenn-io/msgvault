package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/msgvault/internal/store"
)

func saveMessengerState(t *testing.T) func() {
	t.Helper()
	prevCfg := cfg
	prevLogger := logger
	prevMe := importMessengerMe
	prevFormat := importMessengerFormat
	prevLimit := importMessengerLimit
	prevNoResume := importMessengerNoResume
	prevCheckpoint := importMessengerCheckpointEvery
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	return func() {
		cfg = prevCfg
		logger = prevLogger
		importMessengerMe = prevMe
		importMessengerFormat = prevFormat
		importMessengerLimit = prevLimit
		importMessengerNoResume = prevNoResume
		importMessengerCheckpointEvery = prevCheckpoint
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	}
}

func TestImportMessenger_JSON_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		fixture,
	})
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("import-messenger: %v", err)
	}
	if !strings.Contains(stdout.String(), "Import complete") {
		t.Errorf("stdout missing Import complete: %q", stdout.String())
	}

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE message_type='fbmessenger'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("messages=%d want 4", n)
	}
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM participants WHERE email_address='test.user@facebook.messenger'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("self participant count=%d want 1", n)
	}
}

func TestImportMessenger_HTML_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/html_simple")
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		fixture,
	})
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("import-messenger: %v", err)
	}
	if !strings.Contains(stdout.String(), "Import complete") {
		t.Errorf("stdout missing Import complete: %q", stdout.String())
	}
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE message_type='fbmessenger'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("messages=%d want 3", n)
	}
	var rawFormat string
	if err := st.DB().QueryRow("SELECT DISTINCT raw_format FROM message_raw").Scan(&rawFormat); err != nil {
		t.Fatal(err)
	}
	if rawFormat != "fbmessenger_html" {
		t.Errorf("raw_format=%q want fbmessenger_html", rawFormat)
	}
}

func TestImportMessenger_MissingDir(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		filepath.Join(tmp, "does", "not", "exist"),
	})
	err := rootCmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such") {
		t.Errorf("error should describe missing path, got %v", err)
	}
}
