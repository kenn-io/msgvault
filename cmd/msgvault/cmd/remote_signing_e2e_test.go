package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/requestsign"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuiltCLIThroughSignedRestrictedHTTPSPrefix(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	archive := testutil.NewTestStore(t)
	source, err := archive.GetOrCreateSource("gmail", "source@example.test")
	requirements.NoError(err)
	conversation, err := archive.EnsureConversation(source.ID, "fixture-thread", "Synthetic archive")
	requirements.NoError(err)
	raw := []byte("From: source@example.test\r\nTo: recipient@example.test\r\nSubject: Synthetic fixture\r\n\r\nArchive fixture body.\r\n")
	messageID, err := archive.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "fixture-provider-id", MessageType: "email", Subject: sql.NullString{String: "Synthetic fixture", Valid: true}, Snippet: sql.NullString{String: "Archive fixture body", Valid: true}, SentAt: sql.NullTime{Time: time.Now(), Valid: true}}, RawMIME: raw})
	requirements.NoError(err)
	serverHome := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(serverHome, 0o700))
	apiFile := filepath.Join(serverHome, "client-api-key")
	secretFile := filepath.Join(serverHome, "signing-secret")
	stateFile := filepath.Join(serverHome, "replay.json")
	credential := strings.Repeat("k", 40)
	secret := bytes.Repeat([]byte{0x31}, 64)
	requirements.NoError(fileutil.SecureWriteFile(apiFile, []byte(credential), 0o600))
	requirements.NoError(fileutil.SecureWriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600))
	requirements.NoError(requestsign.InitReplayState(stateFile))
	cfg := &config.Config{HomeDir: serverHome, Data: config.DataConfig{DataDir: serverHome}, Server: config.ServerConfig{APIKey: "fixture-main-owner-api-key"}}
	var restricted http.Handler
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/msgvault/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/msgvault")
		r.RequestURI = r.URL.RequestURI()
		restricted.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	cfg.Server.RemoteIngress = config.RemoteIngressConfig{Enabled: true, ExternalURL: proxy.URL + "/msgvault", ReplayStateFile: stateFile, Clients: []config.RemoteIngressClient{{ClientID: "fixture-reader", APIKeyFile: apiFile, Grants: []string{"collections-write"}, Keys: []config.RequestSigningKey{{KeyID: "fixture-1", SecretFile: secretFile}}}}}
	engine := query.NewEngine(archive.DB(), archive.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: archive, Engine: engine, Logger: slog.Default()})
	restricted, err = server.RestrictedRouter()
	requirements.NoError(err)
	defer func() { require.NoError(t, server.Shutdown(context.Background())) }()

	attachment := bytes.Repeat([]byte("synthetic attachment\n"), 10000)
	attachmentHash := sha256.Sum256(attachment)
	hash := hex.EncodeToString(attachmentHash[:])
	attachmentPath := filepath.Join(cfg.AttachmentsDir(), hash[:2], hash)
	requirements.NoError(os.MkdirAll(filepath.Dir(attachmentPath), 0o700))
	requirements.NoError(os.WriteFile(attachmentPath, attachment, 0o600))

	clientHome := t.TempDir()
	clientConfig := filepath.Join(clientHome, "config.toml")
	requirements.NoError(os.WriteFile(clientConfig, []byte(fmt.Sprintf("[remote]\nurl = %q\napi_key_file = %q\nsigning_key_id = \"fixture-1\"\nsigning_secret_file = %q\n", proxy.URL+"/msgvault", apiFile, secretFile)), 0o600))
	certFile := filepath.Join(clientHome, "trust.pem")
	requirements.NoError(os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0o600))
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	requirements.NoError(err)
	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, "./cmd/msgvault")
	build.Dir = repoRoot
	output, err := build.CombinedOutput()
	requirements.NoError(err, "build real CLI: %s", output)

	// Production quarantine is observed, never bypassed. A deadline bounds the
	// transition wait; it imposes no required number of polls or runner throughput.
	deadline := time.Now().Add(50 * time.Second)
	for {
		probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL+"/msgvault/api/v1/health", nil)
		requirements.NoError(err)
		probe.Header.Set("X-Api-Key", credential)
		requirements.NoError(requestsign.Sign(probe, secret, "fixture-1", time.Now(), requestsign.NewNonce()))
		response, err := proxy.Client().Do(probe)
		requirements.NoError(err)
		requirements.NoError(response.Body.Close())
		if response.StatusCode == http.StatusOK {
			break
		}
		requirements.Less(time.Now().UnixNano(), deadline.UnixNano(), "restricted verifier never became ready")
		select {
		case <-t.Context().Done():
			requirements.NoError(t.Context().Err())
		case <-time.After(time.Second):
		}
	}
	run := func(args ...string) ([]byte, error) {
		invocation := exec.CommandContext(t.Context(), binary, append([]string{"--home", clientHome, "--config", clientConfig}, args...)...)
		invocation.Env = append(os.Environ(), "SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+clientHome)
		invocation.Dir = clientHome
		return invocation.CombinedOutput()
	}
	for _, args := range [][]string{{"stats"}, {"search", "subject:Synthetic", "--json"}, {"show-message", strconv.FormatInt(messageID, 10), "--json"}, {"collection", "create", "fixture-collection", "--accounts", "source@example.test"}, {"collection", "show", "fixture-collection"}, {"collection", "delete", "fixture-collection"}} {
		output, err = run(args...)
		requirements.NoError(err, "CLI %v: %s", args, output)
		if args[0] == "search" || args[0] == "show-message" {
			assertions.Contains(string(output), "Synthetic fixture", "CLI must return the fixture message")
		}
	}
	rawFile := filepath.Join(clientHome, "fixture.eml")
	output, err = run("export-eml", strconv.FormatInt(messageID, 10), "-o", rawFile)
	requirements.NoError(err, "export: %s", output)
	got, err := os.ReadFile(rawFile)
	requirements.NoError(err)
	assertions.Equal(raw, got)
	threadDir := filepath.Join(clientHome, "thread")
	output, err = run("export-eml", strconv.FormatInt(messageID, 10), "--thread", "-o", threadDir)
	requirements.NoError(err, "thread export: %s", output)
	attachmentFile := filepath.Join(clientHome, "attachment.bin")
	output, err = run("export-attachment", hash, "-o", attachmentFile)
	requirements.NoError(err, "attachment export: %s", output)
	got, err = os.ReadFile(attachmentFile)
	requirements.NoError(err)
	assertions.Equal(attachment, got)
	for i := range 500 {
		_, err := archive.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: fmt.Sprintf("fixture-overflow-%d", i), MessageType: "email", Subject: sql.NullString{String: "Synthetic overflow member", Valid: true}, SentAt: sql.NullTime{Time: time.Now(), Valid: true}}})
		requirements.NoError(err)
	}
	overflowDir := filepath.Join(clientHome, "overflow-thread")
	output, err = run("export-eml", strconv.FormatInt(messageID, 10), "--thread", "-o", overflowDir)
	requirements.Error(err, "oversized thread must fail: %s", output)
	_, err = os.Stat(overflowDir)
	assertions.True(os.IsNotExist(err), "failed whole-thread export must not create partial output")
	output, err = run("query", "SELECT 1")
	requirements.Error(err, "ungranted query must fail: %s", output)
	_, err = os.Stat(filepath.Join(clientHome, "msgvault.db"))
	assertions.True(os.IsNotExist(err), "remote CLI must not create a local archive")
}
