package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/requestsign"
)

func TestSigningInitStateNoArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	home := filepath.Join(dir, "unused-home")
	path := filepath.Join(dir, "replay.json")
	err := executeSigningTestRoot(t, "--home", home, "signing", "init-state", "--file", path)
	requirements.NoError(err)
	_, err = os.Stat(home)
	assertions.True(os.IsNotExist(err))
	guard, err := requestsign.OpenReplayGuard(path, time.Now(), 10)
	requirements.NoError(err)
	requirements.NoError(guard.Close())
	requirements.Error(executeSigningTestRoot(t, "signing", "init-state", "--file", path))
}

func TestSignedHealthPollingDoesNotFallBackUnsigned(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"operation":{"label":"fixture maintenance"}}`)
	}))
	defer server.Close()
	cfg := &config.Config{Remote: config.RemoteConfig{URL: server.URL, APIKey: "fixture-client-key", SigningKeyID: "reader-1"}}
	fetch := configuredDaemonOperationFetcher(HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, cfg)
	assert.Nil(t, fetch(t.Context()), "invalid signing configuration must fail closed")
	assert.Zero(t, requests.Load(), "auxiliary polling must not send unsigned credentials")
}

func TestUnsignedHealthPollingUsesRemoteAPIKeyFile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	apiKey := strings.Repeat("u", 40)
	apiKeyFile := filepath.Join(dir, "remote-api-key")
	requirements.NoError(fileutil.SecureWriteFile(apiKeyFile, []byte(apiKey), 0o600))
	var authenticated atomic.Int64
	var rejected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != apiKey {
			rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"operation":{"busy":true,"label":"fixture maintenance"}}`)
	}))
	defer server.Close()

	cfg := &config.Config{Remote: config.RemoteConfig{URL: server.URL, APIKeyFile: apiKeyFile, AllowInsecure: true}}
	fetch := configuredDaemonOperationFetcher(HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, cfg)
	requirements.NoError(os.Remove(apiKeyFile))

	operation := fetch(t.Context())
	requirements.NotNil(operation)
	assertions.Equal("fixture maintenance", operation.Label)
	assertions.Equal(int64(1), authenticated.Load())
	assertions.Zero(rejected.Load())
}

func TestSignedHealthPollingReadsCredentialsOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	apiKey := strings.Repeat("s", 40)
	secret := bytes.Repeat([]byte{0x31}, 64)
	apiKeyFile := filepath.Join(dir, "remote-api-key")
	secretFile := filepath.Join(dir, "signing-secret")
	requirements.NoError(fileutil.SecureWriteFile(apiKeyFile, []byte(apiKey), 0o600))
	requirements.NoError(fileutil.SecureWriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600))
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != apiKey || r.Header.Get("Signature-Input") == "" || r.Header.Get("Signature") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"operation":{"busy":true,"label":"fixture signed work"}}`)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	baseURL := server.URL + "/msgvault"
	cfg := &config.Config{Remote: config.RemoteConfig{URL: baseURL, APIKeyFile: apiKeyFile, SigningKeyID: "reader-1", SigningSecretFile: secretFile}}
	fetch := configuredDaemonOperationFetcher(HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: baseURL}, cfg)
	requirements.NoError(os.Remove(apiKeyFile))
	requirements.NoError(os.Remove(secretFile))

	for range 2 {
		operation := fetch(t.Context())
		requirements.NotNil(operation)
		assertions.Equal("fixture signed work", operation.Label)
	}
	assertions.Equal(int64(2), requests.Load())
}

func TestSignedTokenExportFailsBeforeProviderOrNetworkAccess(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusCreated) }))
	defer server.Close()
	home := filepath.Join(t.TempDir(), "unused-home")
	cfg := &config.Config{HomeDir: home, Remote: config.RemoteConfig{URL: server.URL, APIKey: "fixture-client-key", SigningKeyID: "reader-1"}}
	command := &cobra.Command{}
	command.SetContext(withInvocation(context.Background(), &invocation{cfg: cfg}))
	require.ErrorContains(t, runExportToken(command, []string{"source@example.test"}), "unavailable with signed remote ingress")
	assert.Zero(t, requests.Load())
	_, err := os.Stat(home)
	assert.True(t, os.IsNotExist(err), "blocked provider setup must not create client state")
}

func executeSigningTestRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCommand()
	root.AddCommand(newSigningCommand())
	root.SetArgs(args)
	if err := root.ExecuteContext(t.Context()); err != nil {
		return fmt.Errorf("execute signing fixture: %w", err)
	}
	return nil
}
