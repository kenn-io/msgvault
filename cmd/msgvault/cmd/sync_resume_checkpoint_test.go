//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// This walks the built CLI, daemon stream, child process, Gmail client and checkpoint.
func TestInterruptedSyncCopiedCommandResumesCheckpoint(t *testing.T) {
	binary := os.Getenv("MSGVAULT_E2E_BINARY")
	if binary == "" {
		t.Skip("set MSGVAULT_E2E_BINARY to the make build binary")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gmail.googleapis.com"}, DNSNames: []string{"gmail.googleapis.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certDER, err := x509.CreateCertificate(rand.Reader, certTemplate, certTemplate, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	caFile := filepath.Join(root, "fixture-ca.pem")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	query := "subject:first\nsecond\x1b[31m 'quoted' $HOME;echo\n"
	secondPage := make(chan struct{}, 1)
	type searchRequest struct{ query, token string }
	var queries []searchRequest
	var queriesMu sync.Mutex
	var pageRequests atomic.Int32
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, _ = io.WriteString(w, `{"emailAddress":"reader@example.test","messagesTotal":2,"historyId":"1000"}`)
		case "/gmail/v1/users/me/labels":
			_, _ = io.WriteString(w, `{"labels":[]}`)
		case "/gmail/v1/users/me/messages":
			queriesMu.Lock()
			queries = append(queries, searchRequest{r.URL.Query().Get("q"), r.URL.Query().Get("pageToken")})
			queriesMu.Unlock()
			if r.URL.Query().Get("pageToken") == "" {
				_, _ = io.WriteString(w, `{"messages":[{"id":"one","threadId":"one"}],"nextPageToken":"page_1","resultSizeEstimate":2}`)
			} else if pageRequests.Add(1) == 1 {
				secondPage <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-ctx.Done():
				}
			} else {
				_, _ = io.WriteString(w, `{"messages":[{"id":"two","threadId":"two"}],"resultSizeEstimate":2}`)
			}
		case "/gmail/v1/users/me/messages/one", "/gmail/v1/users/me/messages/two":
			id := filepath.Base(r.URL.Path)
			data := base64.RawURLEncoding.EncodeToString(testemail.NewMessage().Bytes())
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "threadId": id, "raw": data, "internalDate": "1700000000000", "historyId": "1000"})
		default:
			http.Error(w, "unexpected provider request", http.StatusNotFound)
		}
	}))
	provider.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	provider.StartTLS()
	t.Cleanup(provider.Close)

	var tunnels sync.WaitGroup
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "gmail.googleapis.com:443" {
			http.Error(w, "unexpected proxy request", http.StatusBadRequest)
			return
		}
		upstream, err := net.Dial("tcp", provider.Listener.Addr().String())
		if err != nil {
			http.Error(w, "provider unavailable", http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		tunnels.Add(1)
		defer tunnels.Done()
		defer client.Close()
		defer upstream.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.Close()
			close(copied)
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		select {
		case <-copied:
		case <-ctx.Done():
		}
	}))
	t.Cleanup(func() {
		proxy.Close()
		finished := make(chan struct{})
		go func() { tunnels.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			assert.Fail(t, "proxy tunnels did not close")
		}
	})
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "127.0.0.1,localhost")
	}
	t.Setenv("SSL_CERT_FILE", caFile)

	daemonHome := filepath.Join(root, "daemon")
	require.NoError(t, os.MkdirAll(daemonHome, 0o700))
	secretPath := filepath.Join(daemonHome, "client.json")
	require.NoError(t, os.WriteFile(secretPath, []byte(fakeClientSecrets), 0o600))
	configFile := filepath.Join(daemonHome, "config.toml")
	require.NoError(t, os.WriteFile(configFile, []byte("[data]\ndata_dir = "+strconv.Quote(daemonHome)+"\n[oauth]\nclient_secrets = "+strconv.Quote(secretPath)+"\n"), 0o600))
	cfg, err := config.Load(configFile, daemonHome)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(cfg.TokensDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.TokensDir(), "reader@example.test.json"), []byte(`{"access_token":"fixture","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"}`), 0o600))
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	previousResolver := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return binary, nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = previousResolver })
	adapter := &storeAPIAdapter{store: st, config: cfg, options: invocationOptions{cfgFile: configFile, homeDir: daemonHome}, logger: slog.New(slog.DiscardHandler)}
	server := api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler))
	done := make(chan struct{}, 2)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/cli/sync-full" {
			defer func() { done <- struct{}{} }()
		}
		server.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() {
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer stop()
		assert.NoError(t, server.Shutdown(shutdownCtx))
	})
	cliHome := filepath.Join(root, "client")
	require.NoError(t, os.MkdirAll(cliHome, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cliHome, "config.toml"), []byte("[remote]\nurl = "+strconv.Quote(httpServer.URL)+"\nallow_insecure = true\n"), 0o600))
	t.Setenv("MSGVAULT_HOME", cliHome)
	cli := exec.CommandContext(ctx, binary, "--home", cliHome, "sync-full", "--no-build-cache", "--query", query, "reader@example.test")
	var stdout, stderr bytes.Buffer
	cli.Stdout, cli.Stderr = &stdout, &stderr
	require.NoError(t, cli.Start())
	waited := false
	defer func() {
		if !waited {
			_ = cli.Process.Kill()
			_ = cli.Wait()
		}
	}()
	select {
	case <-secondPage:
	case <-ctx.Done():
		require.FailNow(t, "provider did not receive the second page")
	}
	require.NoError(t, cli.Process.Signal(os.Interrupt))
	err = cli.Wait()
	waited = true
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
	require.Equal(t, 130, exitErr.ExitCode(), "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
	select {
	case <-done:
	case <-ctx.Done():
		require.FailNow(t, "daemon sync handler did not finish")
	}
	checkpoint, err := st.GetLatestCheckpointedSyncContext(ctx, source.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusFailed, checkpoint.Status)
	require.Equal(t, "page_1", checkpoint.CursorBefore.String)
	require.Equal(t, int64(1), checkpoint.MessagesProcessed)
	lines := strings.Split(stderr.String(), "\n")
	var copied string
	for i, line := range lines {
		if strings.Contains(line, "Sync interrupted. To run it again, run:") && i+1 < len(lines) {
			copied = strings.TrimSpace(lines[i+1])
		}
	}
	require.NotEmpty(t, copied, "%s", stderr.String())
	require.Contains(t, copied, cliHome, "%s", stderr.String())
	replay := exec.CommandContext(ctx, "sh", "-c", copied)
	output, err := replay.CombinedOutput()
	require.NoError(t, err, "%s", output)
	select {
	case <-done:
	case <-ctx.Done():
		require.FailNow(t, "replayed sync handler did not finish")
	}
	var tokens []string
	queriesMu.Lock()
	defer queriesMu.Unlock()
	for _, request := range queries {
		assert.Equal(t, query, request.query)
		tokens = append(tokens, request.token)
	}
	assert.Equal(t, []string{"", "page_1", "page_1"}, tokens)
	count, err := st.CountMessagesForSourceContext(ctx, source.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)
	assert.Equal(t, int32(2), pageRequests.Load())
}
