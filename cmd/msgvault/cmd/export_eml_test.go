package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestExportEMLUsesLocalDaemonHTTPAndPreservesFileOutput(t *testing.T) {
	cfg := testConfigValue()
	useLocal := false

	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	server, rawRequests := emlHTTPDaemon(t, raw)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	_ = testCtx
	useLocal = true
	invocationFromContext(testCtx).options.useLocal = true

	outputPath := filepath.Join(dataDir, "message.eml")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(testCtx)
	cmd.SetContext(testCtx)
	cmd.SetOut(&out)

	err := runExportEML(cmd, "gmail-raw", outputPath)
	require.NoError(err, "export-eml")

	got, err := os.ReadFile(outputPath)
	require.NoError(err, "read output")
	assert.Equal(raw, got, "raw MIME")
	assert.Equal(1, int(rawRequests.Load()), "raw endpoint calls")
	assert.Contains(out.String(), "Exported message to: "+outputPath, "stdout")
	assert.Contains(out.String(), "("+strconv.Itoa(len(raw))+" bytes)", "stdout size")
}

func TestExportEMLHTTPRawDataHintOnlyWhenRawIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		wantHint       bool
		ref, wantError string
	}{
		{"raw missing", http.StatusNotFound, `{"error":"raw_message_not_found","message":"Message raw data not found"}`, true, "gmail-raw", ""},
		{"too large", http.StatusRequestEntityTooLarge, `{"error":"remote_message_too_large","message":"Message content exceeds the remote client byte limit"}`, false, "gmail-raw", ""},
		{"message missing", http.StatusNotFound, `{"error":"not_found","message":"Message not found"}`, false, "missing", "message not found: missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			dataDir := t.TempDir()
			writeStatsHTTPDaemonRuntime(t, dataDir, emlHTTPErrorDaemon(t, tc.status, tc.body))
			testCtx := testInvocationContext(t.Context(), &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}, invocationOptions{})
			invocationFromContext(testCtx).options.useLocal = true
			cmd := &cobra.Command{Use: "export-eml"}
			cmd.SetContext(testCtx)
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := runExportEML(cmd, tc.ref, filepath.Join(dataDir, "message.eml"))
			must.Error(err)
			assert.Empty(t, out.String(), "stdout")
			if tc.wantError != "" {
				must.ErrorContains(err, tc.wantError)
				assert.NotContains(t, err.Error(), "API error")
			}
			assert.Equal(t, tc.wantHint, strings.Contains(err.Error(), "may not have raw data stored"), err.Error())
		})
	}
}

func TestWriteExportedEMLDefaultsToSourceMessageIDFilename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	outputDir := t.TempDir()
	t.Chdir(outputDir)
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetOut(&out)

	err := writeExportedEML(cmd, "INBOX|\x00/42", "", raw)
	require.NoError(err)

	outputPath := filepath.Join(outputDir, "INBOX___42.eml")
	got, err := os.ReadFile(outputPath)
	require.NoError(err)
	assert.Equal(raw, got)
	assert.Contains(out.String(), "Exported message to: INBOX___42.eml")
}

func TestWriteExportedEMLWritesRawBytesToStdout(t *testing.T) {
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetOut(&out)

	err := writeExportedEML(cmd, "gmail-raw", stdoutSentinel, raw)
	require.NoError(t, err)
	assert.Equal(t, raw, out.Bytes())
}

func emlHTTPDaemon(t *testing.T, raw []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("id") != "gmail-raw" {
			http.Error(w, "wrong id", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("X-Msgvault-Source-Message-Id", "gmail-raw")
		_, _ = w.Write(raw)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func emlHTTPErrorDaemon(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message/raw", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestExportEMLThreadWritesEveryOriginalInOrder runs against the real API
// server with an IMAP-style source: no provider thread IDs, so the
// conversation key is the root Message-ID msgvault derived from References.
func TestExportEMLThreadWritesEveryOriginalInOrder(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("imap", "owner@example.com")
	must.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "<root@example.com>", "Quarterly report")
	must.NoError(err)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	persist := func(sourceMessageID string, sentAt time.Time, raw []byte) {
		_, err := st.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				SourceID: src.ID, ConversationID: convID, SourceMessageID: sourceMessageID,
				MessageType: "email", SentAt: sql.NullTime{Time: sentAt, Valid: true},
			},
			RawMIME: raw,
		})
		must.NoError(err)
	}
	reply := []byte("Message-ID: <reply@example.com>\r\nReferences: <root@example.com>\r\n\r\nreply \xe9\n")
	root := []byte("Message-ID: <root@example.com>\r\n\r\nroot\r\n")
	persist("INBOX|2", base.Add(time.Hour), reply)
	persist("INBOX|1", base, root)
	persist("INBOX|3", base.Add(2*time.Hour), nil)

	cmd, out, dataDir := emlArchiveCommand(t, st, nil)
	outDir := filepath.Join(dataDir, "thread")
	must.NoError(runExportEMLThread(cmd, "INBOX|2", "", outDir))

	entries, err := os.ReadDir(outDir)
	must.NoError(err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	checks.Equal([]string{"1-INBOX_1.eml", "2-INBOX_2.eml"}, names)
	got, err := os.ReadFile(filepath.Join(outDir, "1-INBOX_1.eml"))
	must.NoError(err)
	checks.Equal(root, got)
	got, err = os.ReadFile(filepath.Join(outDir, "2-INBOX_2.eml"))
	must.NoError(err)
	checks.Equal(reply, got)
	checks.Contains(out.String(), "Skipped INBOX|3: no original MIME stored")
	checks.Contains(out.String(), "Exported 2 of 3 messages")
	checks.Contains(out.String(), "owner@example.com has never completed a sync")

	err = runExportEMLThread(cmd, "INBOX|2", "", "-")
	checks.ErrorContains(err, "--thread writes one file per message")
}

// emlArchiveCommand uses the real daemon handlers and a private runtime record.
// beforeRequest lets tests coordinate actual store mutations between reads.
func emlArchiveCommand(t *testing.T, st *store.Store, beforeRequest func(*http.Request)) (*cobra.Command, *bytes.Buffer, string) {
	t.Helper()
	dataDir := t.TempDir()
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	router := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler),
	}).Router()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if beforeRequest != nil {
			beforeRequest(r)
		}
		router.ServeHTTP(w, r)
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{useLocal: true}))
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	return cmd, out, dataDir
}

func TestExportEMLThreadResolvesNumericReferencesAndAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "wanted-thread", "Wanted")
	require.NoError(err)
	persist := func(source, conversation int64, provider, body string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
			SourceID: source, ConversationID: conversation, SourceMessageID: provider, MessageType: "email",
		}, RawMIME: []byte(body)})
		require.NoError(err)
		return id
	}
	wantedID := persist(src.ID, conv, "999999", "wanted MIME")
	other, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	otherConv, err := st.EnsureConversation(other.ID, "other-thread", "Other")
	require.NoError(err)
	persist(other.ID, otherConv, strconv.FormatInt(wantedID, 10), "unrelated MIME")
	cmd, out, dataDir := emlArchiveCommand(t, st, nil)
	for _, ref := range []string{"999999", strconv.FormatInt(wantedID, 10)} {
		outDir := filepath.Join(dataDir, ref)
		require.NoError(runExportEMLThread(cmd, ref, "", outDir))
		got, err := os.ReadFile(filepath.Join(outDir, "1-999999.eml"))
		require.NoError(err)
		assert.Equal("wanted MIME", string(got))
	}
	persist(other.ID, otherConv, "999999", "other MIME")
	err = runExportEMLThread(cmd, "999999", "", filepath.Join(dataDir, "ambiguous"))
	require.ErrorIs(err, query.ErrAmbiguousReference)
	assert.Equal(1, strings.Count(err.Error(), "--account"))
	assert.NotContains(err.Error(), "; pass account")
	out.Reset()
	require.NoError(runExportEMLThread(cmd, "999999", "owner@example.com", filepath.Join(dataDir, "scoped")))
	assert.Contains(out.String(), "thread wanted-thread")
}

func TestExportEMLThreadHandlesOriginalReadFailures(t *testing.T) {
	for _, missing := range []string{"message", "MIME", "corrupt MIME"} {
		t.Run(missing, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("gmail", "owner@example.com")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "partial-thread", "Partial")
			require.NoError(err)
			var ids []int64
			for _, name := range []string{"first", "middle", "last"} {
				id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
					SourceID: src.ID, ConversationID: conv, SourceMessageID: name, MessageType: "email",
				}, RawMIME: []byte("original MIME")})
				require.NoError(err)
				ids = append(ids, id)
			}
			cmd, out, dataDir := emlArchiveCommand(t, st, func(r *http.Request) {
				if r.URL.Path != "/api/v1/cli/message/original" || r.URL.Query().Get("id") != strconv.FormatInt(ids[1], 10) {
					return
				}
				switch missing {
				case "message":
					_, err := st.MergeDuplicates(ids[0], []int64{ids[1]}, "test-dedup")
					if !assert.NoError(err) { //nolint:testifylint // HTTP callback cannot call FailNow on the test goroutine.
						return
					}
				case "MIME":
					_, err := st.DB().Exec(st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), ids[1])
					if !assert.NoError(err) { //nolint:testifylint // HTTP callback cannot call FailNow on the test goroutine.
						return
					}
				case "corrupt MIME":
					_, err := st.DB().Exec(st.Rebind("UPDATE message_raw SET raw_data = ? WHERE message_id = ?"), []byte("invalid zlib data"), ids[1])
					assert.NoError(err)
				}
			})
			outDir := filepath.Join(dataDir, "thread")
			err = runExportEMLThread(cmd, "first", "", outDir)
			if missing == "corrupt MIME" {
				require.ErrorContains(err, "export message")
				return
			}
			require.NoError(err)
			entries, err := os.ReadDir(outDir)
			require.NoError(err)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			assert.Equal([]string{"1-first.eml", "3-last.eml"}, names)
			assert.Contains(out.String(), "Skipped middle:")
			assert.Contains(out.String(), "Exported 2 of 3 messages")
			assert.Contains(out.String(), "owner@example.com has never completed a sync")
		})
	}
}

func TestExportEMLThreadKeepsInitialMembershipDuringInsert(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "large-thread", "Large")
	require.NoError(err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	persist := func(name string, sentAt time.Time) error {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
			SourceID: src.ID, ConversationID: conv, SourceMessageID: name, MessageType: "email",
			SentAt: sql.NullTime{Time: sentAt, Valid: true},
		}, RawMIME: []byte(name)})
		return err
	}
	for i := 1; i <= 501; i++ {
		require.NoError(persist(fmt.Sprintf("original-%03d", i), base.Add(time.Duration(i)*time.Minute)))
	}
	var inserted sync.Once
	cmd, out, dataDir := emlArchiveCommand(t, st, func(r *http.Request) {
		if r.URL.Path == "/api/v1/cli/message/original" ||
			(r.URL.Path == "/api/v1/cli/message/thread" && r.URL.Query().Get("offset") != "") {
			inserted.Do(func() { assert.NoError(persist("new-older-message", base)) })
		}
	})
	outDir := filepath.Join(dataDir, "thread")
	require.NoError(runExportEMLThread(cmd, "original-001", "", outDir))
	entries, err := os.ReadDir(outDir)
	require.NoError(err)
	require.Len(entries, 501)
	for i, entry := range entries {
		assert.Equal(fmt.Sprintf("%03d-original-%03d.eml", i+1, i+1), entry.Name())
	}
	assert.Contains(out.String(), "Exported 501 of 501 messages")
}
