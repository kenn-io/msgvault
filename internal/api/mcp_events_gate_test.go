package api

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
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsDeliveryOperationSignalsScheduledWorkWaiter(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}}, Store: f.Store, Logger: testLogger(), OperationGate: gate})
	t.Cleanup(func() { Require.NoError(t, srv.Shutdown(context.Background())) })

	releaseSync, held := gate.BeginLabeledWorkContext(t.Context(), "scheduled sync")
	require.True(held)
	t.Cleanup(releaseSync)
	deliveryCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- srv.MCPEventsDeliveryOperation(deliveryCtx, func() error { return nil })
	}()
	require.Eventually(gate.HasRequestWaiters, 10*time.Second, 10*time.Millisecond, "delivery must be visible to scheduled work as a request waiter")
	releaseSync()
	require.NoError(<-done)
	assert.False(gate.HasRequestWaiters(), "delivery waiter should drain after gate admission")
}

func TestMCPEventsCallbackVerificationReleasesDaemonOperationGate(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}}, Store: f.Store, Logger: testLogger(), OperationGate: gate})
	t.Cleanup(func() { Require.NoError(t, srv.Shutdown(context.Background())) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "receiver.example.net"}, DNSNames: []string{"receiver.example.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	require.NoError(err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(err)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	entered, releaseChallenge := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseChallenge) }) }
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var request struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		Assert.Equal(t, "verification", request.Type)
		Assert.Equal(t, "receiver.example.net", r.TLS.ServerName)
		Assert.NotEmpty(t, r.Header.Get("Webhook-Signature"))
		close(entered)
		select {
		case <-releaseChallenge:
		case <-r.Context().Done():
			return
		}
		if err := json.NewEncoder(rw).Encode(map[string]string{"challenge": request.Challenge}); err != nil {
			Assert.NoError(t, err)
		}
	}))
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	receiver.StartTLS()
	t.Cleanup(receiver.Close)
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{
		Enabled: true, Sources: []string{"gmail"}, OwnerKey: testSessionAPIKey, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: srv.MCPEventsOperation,
		TrustedCallbacks: []mcpevents.TrustedCallback{{Origin: "https://receiver.example.net:8443", Addresses: []string{"10.0.0.5"}}},
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("trusted callback must use configured pin")
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "10.0.0.5:8443" {
				return nil, errors.New("callback dial did not use expected pin")
			}
			return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
		},
		TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	})
	require.NoError(err)
	srv.SetMCPEvents(svc)
	daemon := httptest.NewServer(srv.Router())
	t.Cleanup(daemon.Close)
	requestCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { release(); cancel() })
	body, err := json.Marshal(mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://receiver.example.net:8443/events", Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}})
	require.NoError(err)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, daemon.URL+"/api/v1/mcp/events/subscribe", bytes.NewReader(body))
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	type response struct {
		status int
		body   []byte
		err    error
	}
	result := make(chan response, 1)
	go func() {
		resp, err := daemon.Client().Do(req)
		if err != nil {
			result <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		result <- response{status: resp.StatusCode, body: data, err: err}
	}()
	select {
	case <-entered:
	case r := <-result:
		require.FailNow("callback verification did not begin", "status=%d error=%v body=%s", r.status, r.err, r.body)
	case <-time.After(10 * time.Second):
		require.FailNow("callback verification did not begin")
	}
	// This deadline bounds admission of another archive operation while the
	// real TLS callback is deliberately paused. It does not count polls.
	const competingGateWait = 2 * time.Second
	gateCtx, gateCancel := context.WithTimeout(t.Context(), competingGateWait)
	releaseGate, acquired := srv.operationGate.BeginWorkContext(gateCtx)
	gateCancel()
	if acquired {
		_, err := f.Store.EnsureConversation(f.Source.ID, "synthetic-concurrent-operation", "Synthetic concurrent operation")
		releaseGate()
		require.NoError(err)
	}
	release()
	select {
	case r := <-result:
		require.NoError(r.err)
		assert.Equal(http.StatusOK, r.status, string(r.body))
	case <-time.After(10 * time.Second):
		require.FailNow("subscribe did not finish after challenge release")
	}
	assert.True(acquired, "daemon operation gate must be available during callback verification")
}
