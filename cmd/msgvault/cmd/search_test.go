package cmd

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// captureStdout redirects os.Stdout to a pipe and returns a function
// that restores the original stdout and returns captured output.
// The pipe is drained concurrently to avoid deadlock if the command
// fills the OS pipe buffer.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	requirepkg.NoError(t, err, "create pipe")
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
		requirepkg.NoError(t, res.err, "read captured stdout")
		return string(res.data)
	}
}

func captureStderr(t *testing.T) func() string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	requirepkg.NoError(t, err, "create pipe")
	os.Stderr = w

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
		os.Stderr = origStderr
		res := <-ch
		_ = r.Close()
		requirepkg.NoError(t, res.err, "read captured stderr")
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
	searchMessageTypes = nil
	// Cobra remembers per-flag `Changed` state on the global searchCmd
	// across test invocations. Without clearing it, mutually-exclusive
	// pairs (--account / --collection) trip when a subsequent test only
	// passes one of them.
	searchCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
}

func TestSummaryFromDisplayFallsBackForPhoneMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  query.MessageSummary
		want string
	}{
		{
			name: "email first",
			msg:  query.MessageSummary{FromEmail: "alice@example.com", FromName: "Alice", FromPhone: "+15551234567"},
			want: "alice@example.com",
		},
		{
			name: "display name fallback",
			msg:  query.MessageSummary{FromName: "Alice", FromPhone: "+15551234567"},
			want: "Alice",
		},
		{
			name: "phone fallback",
			msg:  query.MessageSummary{FromPhone: "+15551234567"},
			want: "+15551234567",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summaryFromDisplay(tt.msg)
			requirepkg.Equal(t, tt.want, got, "summaryFromDisplay()")
		})
	}
}

func TestSearchCmd_AccountFlagForwardsToRemoteHTTP(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	requests := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/cli/search", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("account"), "account query")
		assert.Equal("hello", r.URL.Query().Get("q"), "query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"scope_label":"alice@example.com","scope_source_count":1}`))
	}))
	defer srv.Close()

	cfg = &config.Config{}
	cfg.Remote.URL = srv.URL
	cfg.Remote.AllowInsecure = true
	useLocal = false

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--account", "alice@example.com", "hello"})

	err := root.Execute()
	require.NoError(err, "search with account should work over HTTP")
	assert.Equal(1, int(requests.Load()), "search endpoint calls")
}

func TestSearchCmd_MessageTypeFlagForwardsToRemoteMode(t *testing.T) {
	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Equal(t, "/api/v1/cli/search", r.URL.Path, "path")
		gotQuery = r.URL.Query().Get("q")
		assertpkg.Equal(t, "sms", r.URL.Query().Get("message_type"), "message_type query")
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{},
		})
		assertpkg.NoError(t, err, "write response")
	}))
	defer srv.Close()

	cfg = &config.Config{}
	cfg.Remote.URL = srv.URL
	cfg.Remote.AllowInsecure = true
	useLocal = false

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--message-type", "sms", "lunch"})

	err := root.Execute()
	requirepkg.NoError(t, err, "message-type remote search should be forwarded")
	assertpkg.Equal(t, "lunch", gotQuery, "remote query should keep search terms")
}

func TestSearchCmd_FTSUsesLocalDaemonHTTPAndPreservesJSONOutput(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := t.TempDir()
	server, searchRequests := searchHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--json", "lunch"})

	err := root.Execute()
	out := done()
	require.NoError(err, "search command")

	assert.Equal(1, int(searchRequests.Load()), "search endpoint calls")
	assert.Contains(out, `"subject": "Lunch"`, "JSON output should preserve local result shape")
	assert.NotContains(out, `"total"`, "local JSON search output is a bare result array")
}

func TestSearchCmd_FTSCollectionSearchUsesDaemonHTTPAndPreservesBanner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := t.TempDir()
	server, searchRequests := searchHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	doneOut := captureStdout(t)
	doneErr := captureStderr(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--collection", "Important", "--json"})

	err := root.Execute()
	out := doneOut()
	errOut := doneErr()
	require.NoError(err, "collection search command")

	assert.Equal(1, int(searchRequests.Load()), "search endpoint calls")
	assert.Contains(out, `"subject": "Lunch"`, "JSON output")
	assert.Contains(errOut, `Searching collection "Important" (2 accounts)`, "collection banner")
}

func searchHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	searchRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		searchRequests.Add(1)
		if r.URL.Query().Get("collection") == "Important" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"results": [{
					"id": 42,
					"source_message_id": "remote-42",
					"conversation_id": 7,
					"subject": "Lunch",
					"snippet": "see you there",
					"from_email": "alice@example.com",
					"sent_at": "2024-01-02T03:04:05Z",
					"size_estimate": 123,
					"has_attachments": true,
					"attachment_count": 1,
					"labels": ["INBOX"]
				}],
				"scope_label": "Important",
				"scope_source_count": 2
			}`))
			return
		}
		if r.URL.Query().Get("q") != "lunch" {
			http.Error(w, "missing query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [{
				"id": 42,
				"source_message_id": "remote-42",
				"conversation_id": 7,
				"subject": "Lunch",
				"snippet": "see you there",
				"from_email": "alice@example.com",
				"sent_at": "2024-01-02T03:04:05Z",
				"size_estimate": 123,
				"has_attachments": true,
				"attachment_count": 1,
				"labels": ["INBOX"]
			}]
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, searchRequests
}

func TestSearchCmd_AccountFlagWithoutQuery(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })

	// Seed two accounts with one message each.
	src1, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source 1")
	src2, err := s.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err, "create source 2")
	conv1, err := s.EnsureConversation(src1.ID, "c1", "")
	require.NoError(err, "create conv 1")
	conv2, err := s.EnsureConversation(src2.ID, "c2", "")
	require.NoError(err, "create conv 2")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src1.ID, ConversationID: conv1,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "Alice msg", Valid: true},
		SizeEstimate: 100,
	})
	require.NoError(err, "insert msg 1")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src2.ID, ConversationID: conv2,
		SourceMessageID: "m2", MessageType: "email",
		Subject:      sql.NullString{String: "Bob msg", Valid: true},
		SizeEstimate: 200,
	})
	require.NoError(err, "insert msg 2")
	startStoreQueryAPIDaemon(t, tmpDir, s)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	// Search with --account only (no query terms) — must succeed.
	done := captureStdout(t)

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--account", "alice@example.com", "--json",
	})

	err = root.Execute()
	out := done()
	require.NoError(err, "account-only search failed")

	assert.Contains(out, "Alice msg", "expected Alice's message in output")
	assert.NotContains(out, "Bob msg", "Bob's message should be filtered out")
}

func TestSearchCmd_MessageTypeFlagScopesResults(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	emailConv, err := s.EnsureConversation(src.ID, "email-thread", "")
	require.NoError(err, "create email conversation")
	calendarConv, err := s.EnsureConversationWithType(src.ID, "calendar-thread", "calendar_event", "")
	require.NoError(err, "create calendar conversation")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: emailConv,
		SourceMessageID: "email-1", MessageType: "email",
		Subject: sql.NullString{String: "Email hello", Valid: true},
		SentAt:  sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err, "insert email")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: calendarConv,
		SourceMessageID: "calendar-1", MessageType: "calendar_event",
		Subject: sql.NullString{String: "Calendar planning", Valid: true},
		SentAt:  sql.NullTime{Time: time.Date(2024, 5, 2, 12, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err, "insert calendar event")
	startStoreQueryAPIDaemon(t, tmpDir, s)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--message-type", "calendar_event", "--json",
	})
	err = root.Execute()
	out := done()
	require.NoError(err, "message-type search failed")
	assert.Contains(out, "Calendar planning", "expected calendar event in output")
	assert.NotContains(out, "Email hello", "email message must be filtered out")
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
	requirepkg.Error(t, err, "expected error for invalid query")
	assertpkg.ErrorContains(t, err, "empty search query", "want 'empty search query' (not a DB error)")
}

func TestSearchCmd_AccountFlagDoesNotLeakAcrossInvocations(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })
	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	conv, err := s.EnsureConversation(src.ID, "c1", "")
	require.NoError(err, "create conv")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "test msg", Valid: true},
		SizeEstimate: 100,
	})
	require.NoError(err, "insert msg")
	startStoreQueryAPIDaemon(t, tmpDir, s)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	// First invocation: search with --account.
	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--account", "alice@example.com", "--json",
	})
	err = root.Execute()
	_ = done()
	require.NoError(err, "first search failed")

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
	require.NoError(err, "second search failed")
	assertpkg.Contains(t, out, "test msg",
		"second search should find msg without account filter")
}

func TestSearchCmd_NoQueryNoAccount(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetSearchFlags() }()

	cfg = &config.Config{}

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search"})

	err := root.Execute()
	requirepkg.Error(t, err, "expected error for search with no query and no --account")
	assertpkg.ErrorContains(t, err, "provide a search query")
}

// TestSearchCmd_CollectionFlagScopesResults seeds two accounts and one
// collection containing only the first, then runs FTS search with
// --collection. Only the first account's message must come back.
func TestSearchCmd_CollectionFlagScopesResults(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })
	src1, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source 1")
	src2, err := s.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err, "create source 2")
	conv1, err := s.EnsureConversation(src1.ID, "c1", "")
	require.NoError(err, "create conv 1")
	conv2, err := s.EnsureConversation(src2.ID, "c2", "")
	require.NoError(err, "create conv 2")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src1.ID, ConversationID: conv1,
		SourceMessageID: "m1", MessageType: "email",
		Subject:      sql.NullString{String: "Alice msg", Valid: true},
		SizeEstimate: 100,
	})
	require.NoError(err, "insert msg 1")
	_, err = s.UpsertMessage(&store.Message{
		SourceID: src2.ID, ConversationID: conv2,
		SourceMessageID: "m2", MessageType: "email",
		Subject:      sql.NullString{String: "Bob msg", Valid: true},
		SizeEstimate: 200,
	})
	require.NoError(err, "insert msg 2")
	_, err = s.CreateCollection("alice-only", "", []int64{src1.ID})
	require.NoError(err, "create collection")
	startStoreQueryAPIDaemon(t, tmpDir, s)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--collection", "alice-only", "--json",
	})
	err = root.Execute()
	out := done()
	require.NoError(err, "collection-only search failed")
	assert.Contains(out, "Alice msg", "expected Alice's message in output")
	assert.NotContains(out, "Bob msg", "Bob's message must be filtered out")
}

// TestSearchCmd_CollectionFlagUnknown returns a clear error when the
// named collection does not exist.
func TestSearchCmd_CollectionFlagUnknown(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })
	startStoreQueryAPIDaemon(t, tmpDir, s)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--collection", "does-not-exist", "anything",
	})
	err = root.Execute()
	require.Error(err, "expected error for unknown collection")
	assertpkg.ErrorContains(t, err, "no collection")
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
			requirepkg.Error(t, err, "expected error for queryless --mode=%s", mode)
			assertpkg.ErrorContains(t, err, "requires query text")
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
			requirepkg.Error(t, err, "expected error for filter-only --mode=%s query", mode)
			assertpkg.ErrorContains(t, err, "free-text terms")
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
	requirepkg.Error(t, err, "expected error when both --account and --collection are set")
	msg := err.Error()
	assertpkg.Contains(t, msg, "account", "error should mention account flag name")
	assertpkg.Contains(t, msg, "collection", "error should mention collection flag name")
	_ = a
	_ = b
}
