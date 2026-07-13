package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

type failingAttachmentStream struct {
	sent bool
}

func (r *failingAttachmentStream) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("stream failed")
	}
	r.sent = true
	return copy(p, "partial download"), errors.New("stream failed")
}

type closeFailingAttachmentStream struct {
	io.Reader

	closeErr error
}

func (r *closeFailingAttachmentStream) Close() error {
	return r.closeErr
}

func TestExportAttachmentBinaryStreamPreservesExistingFileOnError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "attachment.bin")
	original := []byte("existing data")
	require.NoError(os.WriteFile(outFile, original, 0o600), "seed output file")

	savedOutput := exportAttachmentOutput
	defer func() { exportAttachmentOutput = savedOutput }()
	exportAttachmentOutput = outFile

	err := exportAttachmentBinaryStream(&failingAttachmentStream{})
	require.Error(err, "streaming failure should be returned")

	got, readErr := os.ReadFile(outFile)
	require.NoError(readErr, "read original output")
	assert.Equal(original, got, "pre-existing output must survive failed stream")
}

func TestExportAttachmentBinaryStreamReplacesExistingFile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "attachment.bin")
	require.NoError(os.WriteFile(outFile, []byte("old data"), 0o600), "seed output file")

	savedOutput := exportAttachmentOutput
	defer func() { exportAttachmentOutput = savedOutput }()
	exportAttachmentOutput = outFile

	err := exportAttachmentBinaryStream(strings.NewReader("new data"))
	require.NoError(err, "streaming replacement should succeed")

	got, readErr := os.ReadFile(outFile)
	require.NoError(readErr, "read replaced output")
	assert.Equal([]byte("new data"), got, "pre-existing output should be replaced")
}

func TestExportAttachmentBinaryDownloadPreservesExistingFileOnCloseError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "attachment.bin")
	original := []byte("existing verified data")
	require.NoError(os.WriteFile(outFile, original, 0o600), "seed output file")

	savedOutput := exportAttachmentOutput
	defer func() { exportAttachmentOutput = savedOutput }()
	exportAttachmentOutput = outFile

	doneErr := captureStderr(t)
	err := exportAttachmentBinaryDownload(&closeFailingAttachmentStream{
		Reader:   strings.NewReader("unverified replacement"),
		closeErr: errors.New("verification failed during close"),
	})
	stderr := doneErr()
	require.Error(err, "close-time verification failure should be returned")
	require.ErrorContains(err, "verification failed during close")
	assert.NotContains(stderr, "Exported attachment", "failed export must not report success")

	got, readErr := os.ReadFile(outFile)
	require.NoError(readErr, "read original output")
	assert.Equal(original, got, "pre-existing output must survive close-time verification failure")
	temps, globErr := filepath.Glob(filepath.Join(tmpDir, ".attachment.bin.tmp-*"))
	require.NoError(globErr, "find staged output files")
	assert.Empty(temps, "failed export must remove its staged output")
}

func TestExportAttachmentUsesLocalDaemonHTTPAndPreservesFileOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	wantData := []byte("daemon attachment content")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(wantData))
	server, attachmentRequests := attachmentHTTPDaemon(t, contentHash, wantData)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedOutput := exportAttachmentOutput
	savedJSON := exportAttachmentJSON
	savedBase64 := exportAttachmentBase64
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		exportAttachmentOutput = savedOutput
		exportAttachmentJSON = savedJSON
		exportAttachmentBase64 = savedBase64
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	t.Chdir(dataDir)
	exportAttachmentOutput = "attachment.bin"
	exportAttachmentJSON = false
	exportAttachmentBase64 = false

	doneErr := captureStderr(t)
	cmd := &cobra.Command{Use: "export-attachment"}
	cmd.SetContext(context.Background())

	err := runExportAttachment(cmd, []string{contentHash})
	stderr := doneErr()
	require.NoError(err, "export-attachment")

	outputPath := filepath.Join(dataDir, exportAttachmentOutput)
	got, err := os.ReadFile(outputPath)
	require.NoError(err, "read output")
	assert.Equal(wantData, got, "output")
	assert.Equal(1, int(attachmentRequests.Load()), "attachment endpoint calls")
	assert.Contains(stderr, "Exported attachment to: "+exportAttachmentOutput, "stderr")
	assert.Contains(stderr, "("+strconv.Itoa(len(wantData))+" bytes)", "stderr size")
}

func TestExportAttachmentUsesLocalDaemonHTTPAndPreservesJSONOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	wantData := []byte("daemon attachment content")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(wantData))
	server, attachmentRequests := attachmentHTTPDaemon(t, contentHash, wantData)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedOutput := exportAttachmentOutput
	savedJSON := exportAttachmentJSON
	savedBase64 := exportAttachmentBase64
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		exportAttachmentOutput = savedOutput
		exportAttachmentJSON = savedJSON
		exportAttachmentBase64 = savedBase64
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	exportAttachmentOutput = ""
	exportAttachmentJSON = true
	exportAttachmentBase64 = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "export-attachment"}
	cmd.SetContext(context.Background())

	err := runExportAttachment(cmd, []string{contentHash})
	out := done()
	require.NoError(err, "export-attachment --json")

	var result map[string]any
	require.NoError(json.Unmarshal([]byte(out), &result), "decode JSON")
	assert.Equal(contentHash, result["content_hash"], "content_hash")
	assert.InDelta(float64(len(wantData)), result["size"], 0, "size")
	dataB64, ok := result["data_base64"].(string)
	require.True(ok, "data_base64 is string")
	got, err := base64.StdEncoding.DecodeString(dataB64)
	require.NoError(err, "decode base64")
	assert.Equal(wantData, got, "decoded data")
	assert.Equal(1, int(attachmentRequests.Load()), "attachment endpoint calls")
}

func TestExportAttachmentUsesLocalDaemonHTTPAndPreservesBase64Output(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	wantData := []byte("daemon attachment content")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(wantData))
	server, attachmentRequests := attachmentHTTPDaemon(t, contentHash, wantData)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedOutput := exportAttachmentOutput
	savedJSON := exportAttachmentJSON
	savedBase64 := exportAttachmentBase64
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		exportAttachmentOutput = savedOutput
		exportAttachmentJSON = savedJSON
		exportAttachmentBase64 = savedBase64
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	exportAttachmentOutput = ""
	exportAttachmentJSON = false
	exportAttachmentBase64 = true

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "export-attachment"}
	cmd.SetContext(context.Background())

	err := runExportAttachment(cmd, []string{contentHash})
	out := done()
	require.NoError(err, "export-attachment --base64")

	assert.Equal(base64.StdEncoding.EncodeToString(wantData)+"\n", out, "base64 output")
	assert.Equal(1, int(attachmentRequests.Load()), "attachment endpoint calls")
}

func TestExportAttachment_FlagMutualExclusivity(t *testing.T) {
	tests := []struct {
		name   string
		output string
		json   bool
		base64 bool
		errMsg string
	}{
		{"json+base64", "", true, true, "--json and --base64 are mutually exclusive"},
		{"json+output", "file.bin", true, false, "--json and --output are mutually exclusive"},
		{"base64+output", "file.bin", false, true, "--base64 and --output are mutually exclusive"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exportAttachmentOutput = tc.output
			exportAttachmentJSON = tc.json
			exportAttachmentBase64 = tc.base64
			defer func() {
				exportAttachmentOutput = ""
				exportAttachmentJSON = false
				exportAttachmentBase64 = false
			}()

			// Use a valid hash — flag validation happens before file access
			err := runExportAttachment(nil, []string{
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			})

			require.Error(t, err, "expected error containing %q", tc.errMsg)
			assert.ErrorContains(t, err, tc.errMsg)
		})
	}
}

func attachmentHTTPDaemon(t *testing.T, contentHash string, data []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/attachment", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("content_hash") != contentHash {
			http.Error(w, "wrong content hash", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func TestExportAttachment_HashValidation(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"too short", "61ccf192"},
		{"too long", "61ccf192b5bd358738802dc2676d3ceab856f47d26dd29681ac3d335bfd5bbd0aa"},
		{"invalid hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"empty", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exportAttachmentOutput = ""
			exportAttachmentJSON = false
			exportAttachmentBase64 = false

			err := runExportAttachment(nil, []string{tc.hash})
			require.Error(t, err, "expected error for invalid hash")
			assert.ErrorContains(t, err, "invalid content hash")
		})
	}
}
