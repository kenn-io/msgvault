package cmd

import (
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

// captureStdout redirects os.Stdout to a pipe and returns a function
// that restores the original stdout and returns captured output.
// The pipe is drained concurrently to avoid deadlock if the command
// fills the OS pipe buffer.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	os.Stdout = w

	// Drain the read side concurrently so writers never block.
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, readErr := io.ReadAll(r)
		ch <- result{data, readErr}
	}()

	return func() string {
		_ = w.Close()
		os.Stdout = origStdout
		res := <-ch
		_ = r.Close()
		if res.err != nil {
			t.Fatalf("read captured stdout: %v", res.err)
		}
		return string(res.data)
	}
}

func resetSearchFlags() {
	searchAccount = ""
	searchCollection = ""
	searchLimit = 50
	searchOffset = 0
	searchJSON = false
	searchMode = "fts"
	searchExplain = false
	// Cobra remembers per-flag `Changed` state on the global searchCmd
	// across test invocations. Without clearing it, mutually-exclusive
	// pairs (--account / --collection) trip when a subsequent test only
	// passes one of them.
	searchCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
}

func TestSearchCmd_AccountFlagRejectsRemoteMode(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{}
	cfg.Remote.URL = "http://example.com"

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--account", "a@b.com", "hello"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --account used in remote mode")
	}
	if !strings.Contains(err.Error(), "not supported in remote mode") {
		t.Errorf("error = %q, want 'not supported in remote mode'", err)
	}
}

func TestSearchCmd_AccountFlagWithoutQuery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Seed two accounts with one message each.
	src1, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create source 1: %v", err)
	}
	src2, err := s.GetOrCreateSource("gmail", "bob@example.com")
	if err != nil {
		t.Fatalf("create source 2: %v", err)
	}
	conv1, err := s.EnsureConversation(src1.ID, "c1", "")
	if err != nil {
		t.Fatalf("create conv 1: %v", err)
	}
	conv2, err := s.EnsureConversation(src2.ID, "c2", "")
	if err != nil {
		t.Fatalf("create conv 2: %v", err)
	}
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src1.ID, ConversationID: conv1,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "Alice msg", Valid: true},
		SizeEstimate: 100,
	})
	if err != nil {
		t.Fatalf("insert msg 1: %v", err)
	}
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src2.ID, ConversationID: conv2,
		SourceMessageID: "m2", MessageType: "email",
		Subject:      sql.NullString{String: "Bob msg", Valid: true},
		SizeEstimate: 200,
	})
	if err != nil {
		t.Fatalf("insert msg 2: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	// Search with --account only (no query terms) — must succeed.
	done := captureStdout(t)

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--account", "alice@example.com", "--json",
	})

	err = root.Execute()
	out := done()
	if err != nil {
		t.Fatalf("account-only search failed: %v", err)
	}

	if !strings.Contains(out, "Alice msg") {
		t.Errorf("expected Alice's message in output, got: %s", out)
	}
	if strings.Contains(out, "Bob msg") {
		t.Errorf("Bob's message should be filtered out, got: %s", out)
	}
}

func TestSearchCmd_InvalidQueryFailsFastWithoutDB(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	// Point at a non-existent directory so store.Open would fail
	// if the code reaches it.
	cfg = &config.Config{
		HomeDir: "/nonexistent",
		Data:    config.DataConfig{DataDir: "/nonexistent"},
	}

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "before:not-a-date"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid query")
	}
	if !strings.Contains(err.Error(), "empty search query") {
		t.Errorf(
			"error = %q, want 'empty search query' (not a DB error)",
			err,
		)
	}
}

func TestSearchCmd_AccountFlagDoesNotLeakAcrossInvocations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	conv, err := s.EnsureConversation(src.ID, "c1", "")
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "test msg", Valid: true},
		SizeEstimate: 100,
	})
	if err != nil {
		t.Fatalf("insert msg: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	// First invocation: search with --account.
	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--account", "alice@example.com", "--json",
	})
	err = root.Execute()
	_ = done()
	if err != nil {
		t.Fatalf("first search failed: %v", err)
	}

	// Second invocation: search WITHOUT --account.
	// Must not carry over the previous account filter.
	resetSearchFlags()
	done = captureStdout(t)
	root2 := newTestRootCmd()
	root2.AddCommand(searchCmd)
	root2.SetArgs([]string{
		"search", "--account", "", "--json", "test msg",
	})
	err = root2.Execute()
	out := done()
	if err != nil {
		t.Fatalf("second search failed: %v", err)
	}
	if !strings.Contains(out, "test msg") {
		t.Errorf(
			"second search should find msg without account filter: %s",
			out,
		)
	}
}

func TestSearchCmd_NoQueryNoAccount(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{}

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for search with no query and no --account")
	}
	if !strings.Contains(err.Error(), "provide a search query") {
		t.Errorf("error = %q, want 'provide a search query'", err)
	}
}

// TestSearchCmd_CollectionFlagScopesResults seeds two accounts and one
// collection containing only the first, then runs FTS search with
// --collection. Only the first account's message must come back.
func TestSearchCmd_CollectionFlagScopesResults(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	src1, err := s.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create source 1: %v", err)
	}
	src2, err := s.GetOrCreateSource("gmail", "bob@example.com")
	if err != nil {
		t.Fatalf("create source 2: %v", err)
	}
	conv1, err := s.EnsureConversation(src1.ID, "c1", "")
	if err != nil {
		t.Fatalf("create conv 1: %v", err)
	}
	conv2, err := s.EnsureConversation(src2.ID, "c2", "")
	if err != nil {
		t.Fatalf("create conv 2: %v", err)
	}
	if _, err := s.UpsertMessage(&store.Message{
		SourceID: src1.ID, ConversationID: conv1,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "Alice msg", Valid: true},
		SizeEstimate: 100,
	}); err != nil {
		t.Fatalf("insert msg 1: %v", err)
	}
	if _, err := s.UpsertMessage(&store.Message{
		SourceID: src2.ID, ConversationID: conv2,
		SourceMessageID: "m2", MessageType: "email",
		Subject:      sql.NullString{String: "Bob msg", Valid: true},
		SizeEstimate: 200,
	}); err != nil {
		t.Fatalf("insert msg 2: %v", err)
	}
	if _, err := s.CreateCollection("alice-only", "", []int64{src1.ID}); err != nil {
		t.Fatalf("create collection: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--collection", "alice-only", "--json",
	})
	err = root.Execute()
	out := done()
	if err != nil {
		t.Fatalf("collection-only search failed: %v", err)
	}
	if !strings.Contains(out, "Alice msg") {
		t.Errorf("expected Alice's message in output, got: %s", out)
	}
	if strings.Contains(out, "Bob msg") {
		t.Errorf("Bob's message must be filtered out, got: %s", out)
	}
}

// TestSearchCmd_CollectionFlagUnknown returns a clear error when the
// named collection does not exist.
func TestSearchCmd_CollectionFlagUnknown(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--collection", "does-not-exist", "anything",
	})
	err = root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown collection")
	}
	if !strings.Contains(err.Error(), "no collection") {
		t.Errorf("error = %q, want substring 'no collection'", err)
	}
}

// TestSearchCmd_VectorOrHybridRequireQueryText rejects empty-query
// vector/hybrid invocations even when scope flags are supplied.
// FTS allows queryless scoped searches; vector/hybrid don't, because
// the embeddings client needs text to vectorize.
func TestSearchCmd_VectorOrHybridRequireQueryText(t *testing.T) {
	for _, mode := range []string{"vector", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			savedCfg := cfg
			defer func() { cfg = savedCfg; resetSearchFlags() }()

			cfg = &config.Config{}

			root := newTestRootCmd()
			root.AddCommand(searchCmd)
			root.SetArgs([]string{
				"search", "--mode", mode,
				"--account", "alice@example.com",
			})
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected error for queryless --mode=%s", mode)
			}
			if !strings.Contains(err.Error(), "requires query text") {
				t.Errorf("error = %q, want substring 'requires query text'", err)
			}
		})
	}
}

// TestSearchCmd_VectorOrHybridRejectFilterOnlyQuery rejects vector/
// hybrid invocations whose query parses to filter terms only (no
// free-text). The embed client needs text to vectorize, so a query
// like `from:alice` would fail at the engine layer; reject it at the
// CLI surface instead.
func TestSearchCmd_VectorOrHybridRejectFilterOnlyQuery(t *testing.T) {
	for _, mode := range []string{"vector", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			savedCfg := cfg
			defer func() { cfg = savedCfg; resetSearchFlags() }()

			cfg = &config.Config{}

			root := newTestRootCmd()
			root.AddCommand(searchCmd)
			root.SetArgs([]string{
				"search", "--mode", mode, "from:alice",
			})
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected error for filter-only --mode=%s query", mode)
			}
			if !strings.Contains(err.Error(), "free-text terms") {
				t.Errorf("error = %q, want substring 'free-text terms'", err)
			}
		})
	}
}

// TestSearchCmd_MutualExclusion confirms --account and --collection are rejected together.
func TestSearchCmd_MutualExclusion(t *testing.T) {
	var a, b string
	cmd := &cobra.Command{Use: "search-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "search", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringVar(&a, "account", "", "")
	sub.Flags().StringVar(&b, "collection", "", "")
	sub.MarkFlagsMutuallyExclusive("account", "collection")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"search", "--account", "alpha@example.com", "--collection", "work"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when both --account and --collection are set, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "account") || !strings.Contains(msg, "collection") {
		t.Errorf("error should mention both flag names; got: %q", msg)
	}
	_ = a
	_ = b
}
