package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	useLocal = true

	outputPath := filepath.Join(dataDir, "message.eml")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(context.Background())
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

func TestExportEMLHTTPNotFoundPreservesCLIError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := emlHTTPNotFoundDaemon(t)
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
	useLocal = true

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)

	err := runExportEML(cmd, "missing", filepath.Join(dataDir, "missing.eml"))
	require.Error(err, "export-eml")

	assert.Empty(out.String(), "stdout")
	require.ErrorContains(err, "message not found: missing", "not found error")
	assert.NotContains(err.Error(), "API error", "transport details")
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

	err := writeExportedEML(cmd, "gmail-raw", "", raw)
	require.NoError(err)

	outputPath := filepath.Join(outputDir, "gmail-raw.eml")
	got, err := os.ReadFile(outputPath)
	require.NoError(err)
	assert.Equal(raw, got)
	assert.Contains(out.String(), "Exported message to: gmail-raw.eml")
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

func emlHTTPNotFoundDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message/raw", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Message not found"}`))
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
	dataDir := t.TempDir()
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })

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
	persist("INBOX/2", base.Add(time.Hour), reply)
	persist("INBOX/1", base, root)
	persist("INBOX/3", base.Add(2*time.Hour), nil)

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg, savedUseLocal := cfg, useLocal
	t.Cleanup(func() { cfg, useLocal = savedCfg, savedUseLocal })
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	useLocal = true

	outDir := filepath.Join(dataDir, "thread")
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "export-eml"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	must.NoError(runExportEMLThread(cmd, "INBOX/2", "", outDir))

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
	checks.Contains(out.String(), "Skipped INBOX/3: no original MIME stored")
	checks.Contains(out.String(), "Exported 2 of 3 messages")
	checks.Contains(out.String(), "owner@example.com has never completed a sync")

	err = runExportEMLThread(cmd, "INBOX/2", "", "-")
	checks.ErrorContains(err, "--thread writes one file per message")
}
