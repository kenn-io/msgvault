package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
)

type identityDiscoverFailingWriter struct {
	err error
}

func (w identityDiscoverFailingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func stableIdentityDiscoverResult() identityops.DiscoverResult {
	return identityops.DiscoverResult{
		Account:         "primary@example.test",
		SourceID:        14,
		SourceType:      "imap",
		ScannedMessages: 5,
		Candidates: []identityops.Candidate{
			{
				Identifier:           "known@example.test",
				NormalizedIdentifier: "known@example.test",
				Classification:       "confirmed",
				AlreadyConfirmed:     true,
				Signals:              []string{"is_from_me", "manual"},
				ProviderStates:       []string{"deleted", "enabled"},
				SentMessageCount:     2,
				ReceivedMessageCount: 1,
				FirstSeenAt:          time.Date(2026, 7, 1, 1, 2, 3, 0, time.UTC),
				LastSeenAt:           time.Date(2026, 7, 2, 4, 5, 6, 0, time.UTC),
			},
			{
				Identifier:           "strong@example.test",
				NormalizedIdentifier: "strong@example.test",
				Classification:       "strong",
				Signals:              []string{"sent-folder"},
				ProviderStates:       []string{"enabled"},
				SentMessageCount:     1,
				FirstSeenAt:          time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC),
				LastSeenAt:           time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC),
			},
			{
				Identifier:           "weak@example.test",
				NormalizedIdentifier: "weak@example.test",
				Classification:       "weak",
				Signals:              []string{},
				ProviderStates:       []string{"pending"},
				ReceivedMessageCount: 2,
				FirstSeenAt:          time.Date(2026, 7, 4, 1, 2, 3, 0, time.UTC),
				LastSeenAt:           time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC),
			},
		},
		Rejected: []identityops.RejectedCandidate{{Identifier: "*@example.test", Reason: "wildcard address"}},
		Applied: []store.IdentityConfirmationOutcome{{
			Identifier: "strong@example.test", Added: true, Signals: []string{"sent-folder"},
		}},
	}
}

func TestRenderIdentityDiscoverJSONIsStableDocument(t *testing.T) {
	const expected = `{
		"account":"primary@example.test",
		"source_id":14,
		"source_type":"imap",
		"scanned_messages":5,
		"candidates":[
			{"identifier":"known@example.test","normalized_identifier":"known@example.test","classification":"confirmed","already_confirmed":true,"signals":["is_from_me","manual"],"provider_states":["deleted","enabled"],"sent_message_count":2,"received_message_count":1,"first_seen_at":"2026-07-01T01:02:03Z","last_seen_at":"2026-07-02T04:05:06Z"},
			{"identifier":"strong@example.test","normalized_identifier":"strong@example.test","classification":"strong","already_confirmed":false,"signals":["sent-folder"],"provider_states":["enabled"],"sent_message_count":1,"received_message_count":0,"first_seen_at":"2026-07-03T01:02:03Z","last_seen_at":"2026-07-03T01:02:03Z"},
			{"identifier":"weak@example.test","normalized_identifier":"weak@example.test","classification":"weak","already_confirmed":false,"signals":[],"provider_states":["pending"],"sent_message_count":0,"received_message_count":2,"first_seen_at":"2026-07-04T01:02:03Z","last_seen_at":"2026-07-05T01:02:03Z"}
		],
		"rejected":[{"identifier":"*@example.test","reason":"wildcard address"}],
		"applied":[{"identifier":"strong@example.test","added":true,"signals":["sent-folder"]}]
	}`
	var out bytes.Buffer

	require.NoError(t, renderIdentityDiscover(&out, stableIdentityDiscoverResult(), true))

	assert.JSONEq(t, expected, out.String())
	assert.Equal(t, 1, strings.Count(strings.TrimSpace(out.String()), "\n{")+1, "one JSON document")
}

func TestRenderIdentityDiscoverHumanGroupsCandidatesAndRejected(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, renderIdentityDiscover(&out, stableIdentityDiscoverResult(), false))

	text := out.String()
	for _, want := range []string{
		"CONFIRMED", "STRONG", "WEAK", "REJECTED",
		"known@example.test", "signals: is_from_me,manual", "sent: 2", "received: 1",
		"*@example.test", "wildcard address", "Applied 1 identity confirmation(s).",
	} {
		assert.Contains(t, text, want)
	}
}

func TestRenderIdentityDiscoverHumanShowsProviderStates(t *testing.T) {
	var result identityops.DiscoverResult
	require.NoError(t, json.Unmarshal([]byte(`{
		"account":"primary@example.test",
		"source_id":14,
		"source_type":"imap",
		"candidates":[{
			"identifier":"alias@example.test",
			"normalized_identifier":"alias@example.test",
			"classification":"strong",
			"signals":["provider-alias"],
			"provider_states":["disabled","enabled"]
		}],
		"rejected":[],
		"applied":[]
	}`), &result))

	var out bytes.Buffer
	require.NoError(t, renderIdentityDiscover(&out, result, false))
	assert.Contains(t, out.String(), "provider states: disabled,enabled")
}

func TestRenderIdentityDiscoverProgressWritesHumanProgress(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, renderIdentityDiscoverProgress(&out, identityops.DiscoverProgress{
		Done: 2, Total: 5, Candidates: 3,
	}))
	assert.Equal(t, "Scanning identity evidence: 2/5 messages, 3 candidate(s)\n", out.String())
}

func TestRenderIdentityDiscoverProgressWrapsWriterError(t *testing.T) {
	writeErr := errors.New("write progress")

	err := renderIdentityDiscoverProgress(identityDiscoverFailingWriter{err: writeErr}, identityops.DiscoverProgress{})

	require.ErrorIs(t, err, writeErr)
	assert.ErrorContains(t, err, "render identity discovery progress")
}

func TestIdentityDiscoverProviderSourceIDApplyConfirmJSONUsesHTTPAndSuppressesProgress(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}).ServeHTTP(w, r)
			return
		}
		assertions.Equal("/api/v1/cli/identities/discover", r.URL.Path)
		var req identityops.DiscoverRequest
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&req), "decode discovery request") {
			http.Error(w, "bad discovery request", http.StatusBadRequest)
			return
		}
		assertions.Equal(identityops.SourceSelector{SourceID: 14}, req.SourceSelector)
		assertions.True(req.Apply)
		assertions.True(req.Provider)
		assertions.Equal([]string{"weak@example.test"}, req.Confirm)
		w.Header().Set("Content-Type", "application/x-ndjson")
		assertions.NoError(json.NewEncoder(w).Encode(identityops.DiscoverEvent{
			Type: "progress", Progress: &identityops.DiscoverProgress{Done: 2, Total: 2, Candidates: 1},
		}))
		result := stableIdentityDiscoverResult()
		assertions.NoError(json.NewEncoder(w).Encode(identityops.DiscoverEvent{Type: "result", Result: &result}))
	}))
	t.Cleanup(srv.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedSourceID := identityDiscoverSourceID
	savedApply := identityDiscoverApply
	savedProvider := identityDiscoverProvider
	savedConfirm := append([]string(nil), identityDiscoverConfirm...)
	savedJSON := identityDiscoverJSON
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		identityDiscoverSourceID = savedSourceID
		identityDiscoverApply = savedApply
		identityDiscoverProvider = savedProvider
		identityDiscoverConfirm = savedConfirm
		identityDiscoverJSON = savedJSON
		for _, name := range []string{"source-id", "apply", "provider", "confirm", "json"} {
			identityDiscoverCmd.Flags().Lookup(name).Changed = false
		}
	})
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	var stdout, stderr bytes.Buffer
	root := newTestRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(identityCmd)
	root.SetArgs([]string{
		"identity", "discover", "--source-id", "14", "--apply", "--provider",
		"--confirm", "weak@example.test", "--json",
	})

	requirements.NoError(root.Execute())
	assertions.Empty(stderr.String(), "JSON mode suppresses progress")
	var got identityops.DiscoverResult
	requirements.NoError(json.Unmarshal(stdout.Bytes(), &got))
	assertions.Equal(stableIdentityDiscoverResult(), got)
}

func TestIdentityDiscoverRejectsExplicitZeroSourceID(t *testing.T) {
	savedSourceID := identityDiscoverSourceID
	t.Cleanup(func() { identityDiscoverSourceID = savedSourceID })
	cmd := &cobra.Command{Use: "discover [account]", Args: identityDiscoverArgs}
	cmd.Flags().Int64Var(&identityDiscoverSourceID, "source-id", 0, "")
	require.NoError(t, cmd.Flags().Set("source-id", "0"))

	err := identityDiscoverArgs(cmd, nil)
	if err == nil {
		_, err = identitySourceSelector(cmd, "", identityDiscoverSourceID)
	}

	require.Error(t, err)
	assert.ErrorContains(t, err, "source ID must be positive")
}

func TestIdentityDiscoverRejectsAccountWithSourceID(t *testing.T) {
	cmd := &cobra.Command{Use: "discover [account]", Args: identityDiscoverArgs}
	var sourceID int64
	cmd.Flags().Int64Var(&sourceID, "source-id", 0, "")
	require.NoError(t, cmd.Flags().Set("source-id", "14"))

	err := identityDiscoverArgs(cmd, []string{"primary@example.test"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "mutually exclusive")
}

func TestIdentityDiscoverPositionalAccountCompatibility(t *testing.T) {
	cmd := &cobra.Command{Use: "discover [account]", Args: identityDiscoverArgs}
	var sourceID int64
	cmd.Flags().Int64Var(&sourceID, "source-id", 0, "")

	require.NoError(t, identityDiscoverArgs(cmd, []string{"primary@example.test"}))
	selector, err := identitySourceSelector(cmd, "primary@example.test", sourceID)
	require.NoError(t, err)
	assert.Equal(t, daemonclient.CLIIdentitySourceSelector{Account: "primary@example.test"}, selector)
}

func TestIdentityImportRequiresExactlyOneInputSource(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		stdin     bool
		wantError string
	}{
		{name: "neither", wantError: "exactly one of --file or --stdin"},
		{name: "both", file: "aliases.txt", stdin: true, wantError: "exactly one of --file or --stdin"},
		{name: "dash file", file: "-", wantError: `--file "-" is not supported`},
		{name: "file", file: "aliases.txt"},
		{name: "stdin", stdin: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			savedFile, savedStdin := identityImportFile, identityImportStdin
			t.Cleanup(func() {
				identityImportFile, identityImportStdin = savedFile, savedStdin
			})
			identityImportFile, identityImportStdin = test.file, test.stdin
			cmd := &cobra.Command{Use: "import [account]"}
			cmd.Flags().String("file", "", "")
			cmd.Flags().Bool("stdin", false, "")
			if test.file != "" {
				requirements.NoError(cmd.Flags().Set("file", test.file))
			}
			if test.stdin {
				requirements.NoError(cmd.Flags().Set("stdin", "true"))
			}

			err := identityImportArgs(cmd, []string{"primary@example.test"})
			if test.wantError == "" {
				requirements.NoError(err)
				return
			}
			requirements.Error(err)
			assertions.ErrorContains(err, test.wantError)
		})
	}
}

func TestIdentityImportStdinPreviewShowsHumanStates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, root, stdout, stderr := newIdentityCLITest(t)
	_, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	root.SetIn(strings.NewReader(`{
		"identities":[
			{"identifier":"old@example.test","state":"disabled"},
			{"identifier":"waiting@example.test","state":"pending"}
		]
	}`))
	root.SetArgs([]string{"identity", "import", "primary@example.test", "--stdin"})

	requirements.NoError(root.Execute())
	assertions.Empty(stderr.String())
	text := stdout.String()
	assertions.Contains(text, "Identity import for primary@example.test")
	assertions.Contains(text, "old@example.test")
	assertions.Contains(text, "states: disabled")
	assertions.Contains(text, "waiting@example.test")
	assertions.Contains(text, "states: pending")
	assertions.Contains(text, "Applied 0 identity confirmation(s).")
}

func TestIdentityImportRejectsExplicitZeroSourceID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	savedSourceID, savedFile, savedStdin := identityImportSourceID, identityImportFile, identityImportStdin
	t.Cleanup(func() {
		identityImportSourceID, identityImportFile, identityImportStdin = savedSourceID, savedFile, savedStdin
	})
	identityImportStdin = false
	cmd := &cobra.Command{Use: "import [account]", Args: identityImportArgs}
	cmd.Flags().Int64Var(&identityImportSourceID, "source-id", 0, "")
	cmd.Flags().StringVar(&identityImportFile, "file", "", "")
	cmd.Flags().Bool("stdin", false, "")
	requirements.NoError(cmd.Flags().Set("source-id", "0"))
	requirements.NoError(cmd.Flags().Set("file", "aliases.txt"))

	err := identityImportArgs(cmd, nil)
	if err == nil {
		_, err = identitySourceSelector(cmd, "", identityImportSourceID)
	}

	requirements.Error(err)
	assertions.ErrorContains(err, "source ID must be positive")
}

func TestIdentityImportJSONFileApplyUsesSourceIDAndStableOutput(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, root, stdout, stderr := newIdentityCLITest(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	inputFile := filepath.Join(t.TempDir(), "aliases")
	requirements.NoError(os.WriteFile(inputFile, []byte(`[
		{"identifier":"Waiting@Example.test","state":"pending"},
		{"identifier":"active@example.test","state":"enabled"}
	]`), 0o600))
	root.SetArgs([]string{
		"identity", "import", "--source-id", strconv.FormatInt(source.ID, 10),
		"--file", inputFile, "--signal", "bulk-import", "--apply", "--json",
	})

	requirements.NoError(root.Execute())
	assertions.Empty(stderr.String())
	var result identityops.ImportResult
	requirements.NoError(json.Unmarshal(stdout.Bytes(), &result))
	assertions.Equal("bulk-import", result.Signal)
	assertions.Equal([]string{"active@example.test", "Waiting@Example.test"}, []string{
		result.Candidates[0].Identifier,
		result.Candidates[1].Identifier,
	})
	assertions.Equal([]store.IdentityConfirmationOutcome{
		{Identifier: "active@example.test", Added: true, Signals: []string{"bulk-import"}},
		{Identifier: "Waiting@Example.test", Added: true, Signals: []string{"bulk-import"}},
	}, result.Applied)
	assertions.Equal(1, strings.Count(strings.TrimSpace(stdout.String()), "\n{")+1,
		"JSON mode emits one stable document")
}

// newIdentityCLITest creates an isolated store and test root command for
// identity subcommand tests.  Returns (store, root, stdout buffer, stderr buffer).
func newIdentityCLITest(t *testing.T) (*store.Store, *cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	s, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, s.InitSchema())
	t.Cleanup(func() { _ = s.Close() })

	// Save and restore package-level globals.
	savedCfg := cfg
	savedLogger := logger
	savedAccount := identityListAccount
	savedCollection := identityListCollection
	savedListJSON := identityListJSON
	savedShowJSON := identityShowJSON
	savedAddSignal := identityAddSignal
	savedListSourceID := identityListSourceID
	savedShowSourceID := identityShowSourceID
	savedAddSourceID := identityAddSourceID
	savedRemoveSourceID := identityRemoveSourceID
	savedDiscoverSourceID := identityDiscoverSourceID
	savedDiscoverApply := identityDiscoverApply
	savedDiscoverProvider := identityDiscoverProvider
	savedDiscoverConfirm := append([]string(nil), identityDiscoverConfirm...)
	savedDiscoverJSON := identityDiscoverJSON
	savedImportSourceID := identityImportSourceID
	savedImportFile := identityImportFile
	savedImportStdin := identityImportStdin
	savedImportSignal := identityImportSignal
	savedImportApply := identityImportApply
	savedImportJSON := identityImportJSON
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		logger = savedLogger
		identityListAccount = savedAccount
		identityListCollection = savedCollection
		identityListJSON = savedListJSON
		identityShowJSON = savedShowJSON
		identityAddSignal = savedAddSignal
		identityListSourceID = savedListSourceID
		identityShowSourceID = savedShowSourceID
		identityAddSourceID = savedAddSourceID
		identityRemoveSourceID = savedRemoveSourceID
		identityDiscoverSourceID = savedDiscoverSourceID
		identityDiscoverApply = savedDiscoverApply
		identityDiscoverProvider = savedDiscoverProvider
		identityDiscoverConfirm = savedDiscoverConfirm
		identityDiscoverJSON = savedDiscoverJSON
		identityImportSourceID = savedImportSourceID
		identityImportFile = savedImportFile
		identityImportStdin = savedImportStdin
		identityImportSignal = savedImportSignal
		identityImportApply = savedImportApply
		identityImportJSON = savedImportJSON
		useLocal = savedUseLocal
		// Reset cobra's "Changed" state so mutually-exclusive flag groups
		// don't carry over between tests that share the package-level command.
		for _, name := range []string{"account", "collection", "source-id", "json"} {
			if f := identityListCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		for _, cmd := range []*cobra.Command{identityShowCmd, identityAddCmd, identityRemoveCmd} {
			for _, name := range []string{"source-id", "json", "signal"} {
				if f := cmd.Flags().Lookup(name); f != nil {
					f.Changed = false
				}
			}
		}
		for _, name := range []string{"source-id", "apply", "provider", "confirm", "json"} {
			if f := identityDiscoverCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		for _, name := range []string{"source-id", "file", "stdin", "signal", "apply", "json"} {
			if f := identityImportCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
	})

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	logger = slog.New(slog.DiscardHandler)
	startStoreAPIDaemon(t, tmpDir, s, nil)

	var stdout, stderr bytes.Buffer
	root := newTestRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(identityCmd)

	return s, root, &stdout, &stderr
}

func TestIdentityListUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := identityHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedListJSON := identityListJSON
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		identityListJSON = savedListJSON
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	identityListJSON = false

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "list", RunE: runIdentityList}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(err, "identity list")

	assert.Equal(1, int(requests.Load()), "identity endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(), "ACCOUNT", "table header")
	assert.Contains(stdout.String(), "alice@example.com", "identity row")
	assert.Contains(stdout.String(), "manual", "signal")
	assert.Contains(stdout.String(), "2024-01-02 03:04", "confirmed timestamp")
	assert.Contains(stdout.String(), "(none)", "none row")
}

func TestIdentityShowUsesLocalDaemonHTTPAndPreservesHint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := identityHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedShowJSON := identityShowJSON
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		identityShowJSON = savedShowJSON
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	identityShowJSON = false

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "show <account>",
		Args: identityShowCmd.Args,
		RunE: runIdentityShow,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"empty@example.com"})

	err := cmd.Execute()
	require.NoError(err, "identity show")

	assert.Equal(1, int(requests.Load()), "identity endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(), "(none)", "none row")
	assert.Contains(stdout.String(), "msgvault identity add empty@example.com <identifier>", "empty identity hint")
}

func TestIdentityAddUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := identityHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedSignal := identityAddSignal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		identityAddSignal = savedSignal
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	identityAddSignal = "manual"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "add <account> <identifier>",
		Args: identityAddCmd.Args,
		RunE: runIdentityAdd,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"alice@example.com", "extra@example.com"})

	err := cmd.Execute()
	require.NoError(err, "identity add")

	assert.Equal(1, int(requests.Load()), "identity endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(),
		"Added extra@example.com to alice@example.com (signal: manual).",
		"add confirmation")
}

func TestIdentityRemoveUsesLocalDaemonHTTPAndPreservesWarning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, requests := identityHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "remove <account> <identifier>",
		Args: identityRemoveCmd.Args,
		RunE: runIdentityRemove,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"alice@example.com", "alice@example.com"})

	err := cmd.Execute()
	require.NoError(err, "identity remove")

	assert.Equal(1, int(requests.Load()), "identity endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.Contains(stdout.String(),
		"Removed alice@example.com from alice@example.com.",
		"remove confirmation")
	assert.Contains(stdout.String(), "Warning: alice@example.com now has no confirmed identity.",
		"last-identity warning")
}

func identityHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/identities", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Query().Get("account") {
			case "":
				_, _ = w.Write([]byte(`{
					"rows": [{
						"account": "alice@example.com",
						"source_id": 7,
						"source_type": "gmail",
						"identifier": "alice@example.com",
						"signals": ["manual"],
						"confirmed_at": "2024-01-02T03:04:05Z"
					}, {
						"account": "old-mbox",
						"source_id": 8,
						"source_type": "mbox",
						"signals": [],
						"none": true
					}]
				}`))
			case "empty@example.com":
				assert.Equal(t, "true", r.URL.Query().Get("primary_only"), "identity show primary-only query")
				_, _ = w.Write([]byte(`{
					"rows": [{
						"account": "empty@example.com",
						"source_id": 9,
						"source_type": "gmail",
						"signals": [],
						"none": true
					}]
				}`))
			default:
				http.Error(w, "unexpected account", http.StatusBadRequest)
			}
		case http.MethodPost:
			var req struct {
				Account    string `json:"account"`
				Identifier string `json:"identifier"`
				Signal     string `json:"signal"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode add request") {
				http.Error(w, "bad add request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, "alice@example.com", req.Account, "add account")
			assert.Equal(t, "extra@example.com", req.Identifier, "add identifier")
			assert.Equal(t, "manual", req.Signal, "add signal")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"account": "alice@example.com",
				"identifier": "extra@example.com",
				"signal": "manual",
				"outcome": "added"
			}`))
		case http.MethodDelete:
			var req struct {
				Account    string `json:"account"`
				Identifier string `json:"identifier"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode remove request") {
				http.Error(w, "bad remove request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, "alice@example.com", req.Account, "remove account")
			assert.Equal(t, "alice@example.com", req.Identifier, "remove identifier")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"account": "alice@example.com",
				"identifier": "alice@example.com",
				"removed": 1,
				"no_identity": true
			}`))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func TestIdentityList_NoScope(t *testing.T) {
	assert := assert.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	b, _ := s.GetOrCreateSource("imap", "bob@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "account-identifier")
	_ = s.AddAccountIdentity(b.ID, "bob@example.com", "account-identifier")

	root.SetArgs([]string{"identity", "list"})
	require.NoError(t, root.Execute())
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
	require.NoError(t, root.Execute())
	text := out.String()
	assert.Contains(t, text, "alice@example.com", "missing alice")
	assert.NotContains(t, text, "bob@example.com", "bob leaked into account-filtered output")
}

func TestIdentityList_RejectsExplicitZeroSourceID(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)

	root.SetArgs([]string{"identity", "list", "--source-id", "0"})
	err = root.Execute()

	require.Error(t, err)
	assert.ErrorContains(t, err, "source ID must be positive")
}

func TestIdentityList_AccountWithNoneRow(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("mbox", "old-mbox-2018")

	root.SetArgs([]string{"identity", "list"})
	require.NoError(t, root.Execute())
	text := out.String()
	assert.Contains(t, text, "(none)", "expected (none) row for account with no identifiers")
}

func TestIdentityList_JSONShape(t *testing.T) {
	require := require.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "alice@example.com", "missing alice")
}

func TestIdentityShow_Empty(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "show", "alice@example.com"})
	require.NoError(t, root.Execute())
	text := out.String()
	assert.Contains(t, text, "(none)", "missing (none) row")
	assert.Contains(t, text, "identity add", "missing hint")
}

func TestIdentityShow_SourceIDDisambiguatesDuplicateAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, root, out, _ := newIdentityCLITest(t)
	gmail, err := s.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	imap, err := s.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)
	require.NoError(s.AddAccountIdentity(gmail.ID, "gmail-alias@example.test", "manual"))
	require.NoError(s.AddAccountIdentity(imap.ID, "imap-alias@example.test", "manual"))

	root.SetArgs([]string{"identity", "show", "--source-id", strconv.FormatInt(gmail.ID, 10)})
	require.NoError(root.Execute())

	assert.Contains(out.String(), "gmail-alias@example.test")
	assert.NotContains(out.String(), "imap-alias@example.test")
}

func TestIdentityShow_UnknownAccount(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t) //nolint:dogsled // helper returns 4 values; test needs only root
	root.SetArgs([]string{"identity", "show", "ghost@example.com"})
	err := root.Execute()
	require.Error(t, err)
}

func TestIdentityShow_JSONShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.NoError(t, root.Execute())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &rows), "json decode (out=%s)", out.String())
	require.Empty(t, rows, "got rows %+v", rows)
}

func TestIdentityAdd_FirstTime(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "Added extra@example.com", "missing add confirmation")
}

func TestIdentityAdd_SourceIDDisambiguatesDuplicateAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, root, _, _ := newIdentityCLITest(t)
	gmail, err := s.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	imap, err := s.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)

	root.SetArgs([]string{
		"identity", "add", "--source-id", strconv.FormatInt(gmail.ID, 10), "alias@example.test",
	})
	require.NoError(root.Execute())

	gmailIdentities, err := s.ListAccountIdentities(gmail.ID)
	require.NoError(err)
	assert.Len(gmailIdentities, 1)
	assert.Equal("alias@example.test", gmailIdentities[0].Address)
	imapIdentities, err := s.ListAccountIdentities(imap.ID)
	require.NoError(err)
	assert.Empty(imapIdentities)
}

func TestIdentityAdd_RejectsAccountWithSourceID(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	source, err := s.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(t, err)

	root.SetArgs([]string{
		"identity", "add", "--source-id", strconv.FormatInt(source.ID, 10),
		"shared@example.test", "alias@example.test",
	})
	err = root.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "mutually exclusive")
}

func TestIdentityAdd_IdempotentSameSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "already confirmed", "missing idempotent confirmation")
}

func TestIdentityAdd_AdditionalSignal(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "add", "alice@example.com", "extra@example.com",
		"--signal", "account-identifier"})
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "additional signal", "missing additional-signal confirmation")
}

func TestIdentityAdd_RejectsCommaInSignal(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "foo@example.com",
		"--signal", "a,b"})
	err := root.Execute()
	require.Error(t, err, "want comma error")
	require.ErrorContains(t, err, "comma")
}

func TestIdentityAdd_RejectsEmptyIdentifier(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")
	root.SetArgs([]string{"identity", "add", "alice@example.com", "   "})
	err := root.Execute()
	require.Error(t, err, "want empty-identifier error")
	require.ErrorContains(t, err, "empty")
}

func TestIdentityAdd_RejectsCollectionAsAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_, _ = s.CreateCollection("team", "", []int64{a.ID})

	root.SetArgs([]string{"identity", "add", "team", "extra@example.com"})
	err := root.Execute()
	require.Error(t, err, "want collection-rejection error")
	require.ErrorContains(t, err, "collection")
}

func TestIdentityRemove_Hit(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")
	_ = s.AddAccountIdentity(a.ID, "extra@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "extra@example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "Removed extra@example.com", "missing remove confirmation")
}

func TestIdentityRemove_SourceIDDisambiguatesDuplicateAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, root, _, _ := newIdentityCLITest(t)
	gmail, err := s.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	imap, err := s.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)
	require.NoError(s.AddAccountIdentity(gmail.ID, "shared-alias@example.test", "manual"))
	require.NoError(s.AddAccountIdentity(imap.ID, "shared-alias@example.test", "manual"))

	root.SetArgs([]string{
		"identity", "remove", "--source-id", strconv.FormatInt(gmail.ID, 10), "shared-alias@example.test",
	})
	require.NoError(root.Execute())

	gmailIdentities, err := s.ListAccountIdentities(gmail.ID)
	require.NoError(err)
	assert.Empty(gmailIdentities)
	imapIdentities, err := s.ListAccountIdentities(imap.ID)
	require.NoError(err)
	require.Len(imapIdentities, 1)
	assert.Equal("shared-alias@example.test", imapIdentities[0].Address)
}

func TestIdentityRemove_Miss(t *testing.T) {
	s, root, out, errOut := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	require.Error(t, err, "expected error on miss")
	combined := out.String() + errOut.String() + err.Error()
	assert.Contains(t, combined, "Currently confirmed:", "error should hint at present identifiers")
}

func TestIdentityRemove_MissOnEmptyAccount(t *testing.T) {
	s, root, _, _ := newIdentityCLITest(t)
	_, _ = s.GetOrCreateSource("gmail", "alice@example.com")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "ghost@example.com"})
	err := root.Execute()
	require.Error(t, err, "expected error on miss")
	assert.ErrorContains(t, err, "no confirmed identifiers")
}

func TestIdentityRemove_WhitespaceIdentifier(t *testing.T) {
	_, root, _, _ := newIdentityCLITest(t) //nolint:dogsled // helper returns 4 values; test needs only root

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "   "})
	err := root.Execute()
	require.Error(t, err, "expected error for whitespace identifier")
	assert.ErrorContains(t, err, "identifier must not be empty")
}

func TestIdentityRemove_LastIdentifierWarns(t *testing.T) {
	s, root, out, _ := newIdentityCLITest(t)
	a, _ := s.GetOrCreateSource("gmail", "alice@example.com")
	_ = s.AddAccountIdentity(a.ID, "alice@example.com", "manual")

	root.SetArgs([]string{"identity", "remove", "alice@example.com", "alice@example.com"})
	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "no confirmed identity", "missing degraded-dedup warning")
}

func TestIdentityList_CollectionFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
