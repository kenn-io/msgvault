package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const mcpEventsTestCallbackURL = "https://receiver.example.net:8443/events"
const mcpEventsTestCallbackOrigin = "https://receiver.example.net:8443"

type mcpEventsTestReceiver struct {
	trustedCallbacks []mcpevents.TrustedCallback
	lookupIP         func(context.Context, string) ([]netip.Addr, error)
	dialContext      func(context.Context, string, string) (net.Conn, error)
	tlsConfig        *tls.Config
	deliveries       chan mcpevents.Envelope
}

func newMCPEventsTestReceiver(t *testing.T) mcpEventsTestReceiver {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Require.NoError(t, err)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "receiver.example.net"},
		DNSNames:     []string{"receiver.example.net"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	Require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	Require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	deliveries := make(chan mcpevents.Envelope, 1)
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if json.Unmarshal(body, &request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Type == "verification" {
			if json.NewEncoder(w).Encode(map[string]string{"challenge": request.Challenge}) != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		var envelope mcpevents.Envelope
		if json.Unmarshal(body, &envelope) != nil || envelope.EventID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case deliveries <- envelope:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	receiver.StartTLS()
	t.Cleanup(receiver.Close)

	return mcpEventsTestReceiver{
		trustedCallbacks: []mcpevents.TrustedCallback{{Origin: mcpEventsTestCallbackOrigin, Addresses: []string{"10.0.0.5"}}},
		lookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("trusted callback must use its configured pin")
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "10.0.0.5:8443" {
				return nil, errors.New("callback dial did not use its configured pin")
			}
			return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
		},
		tlsConfig:  &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		deliveries: deliveries,
	}
}

func mcpEventsAPIGetRequest(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = "192.0.2.10:4242"
	r.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestMCPEventsAPIRetainedOperationsSurviveDisabledImportNotifications(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	grantSyntheticMCPAPISource(t, f)
	keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
	receiver := newMCPEventsTestReceiver(t)
	const ownerFamily = "msgvault.import_message_archived"
	const importSecret = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	owner := http.Header{"Authorization": {"Bearer " + testSessionAPIKey}}

	service, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{
		Enabled: true, ImportNotifications: true, Retention: 7 * 24 * time.Hour,
		KeyPath: keyPath, OwnerKey: testSessionAPIKey, TrustedCallbacks: receiver.trustedCallbacks,
		LookupIP: receiver.lookupIP, DialContext: receiver.dialContext, TLSConfig: receiver.tlsConfig,
	})
	require.NoError(err)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}},
		Store:  f.Store, Logger: testLogger(), MCPEvents: service,
	})
	t.Cleanup(func() { Require.NoError(t, srv.Shutdown(context.Background())) })
	runCtx, cancelWorkers := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	var stopOnce sync.Once
	stopWorkers := func() {
		stopOnce.Do(func() {
			cancelWorkers()
			select {
			case runErr := <-runDone:
				Require.NoError(t, runErr)
			case <-time.After(10 * time.Second):
				Require.FailNow(t, "MCP Events workers did not stop")
			}
		})
	}
	t.Cleanup(stopWorkers)

	requestBody, err := json.Marshal(mcpevents.SubscribeRequest{
		Name:      ownerFamily,
		Arguments: map[string]any{"source_id": "*"},
		Delivery:  mcpevents.Delivery{Mode: "webhook", URL: mcpEventsTestCallbackURL, Secret: importSecret},
	})
	require.NoError(err)
	subscribe := mcpEventsAPIRequest(t, srv, "subscribe", string(requestBody), owner)
	require.Equal(http.StatusOK, subscribe.Code, subscribe.Body.String())
	var subscription mcpevents.SubscribeResult
	require.NoError(json.Unmarshal(subscribe.Body.Bytes(), &subscription))

	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
	require.NoError(err)
	t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
	syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
	require.NoError(err)
	scoped := f.Store.ScopedToSync(f.Source.ID, syncID)
	importRun, err := scoped.BeginImportAttempt(t.Context(), store.ImportAttemptRequest{
		Principal: mcpevents.Principal(testSessionAPIKey), SourceID: f.Source.ID,
		Format: "eml", NotifyImports: true, SyncRunID: syncID,
	})
	require.NoError(err)
	importStore, err := scoped.WithImportRun(t.Context(), importRun.ID)
	require.NoError(err)
	messageID, err := importStore.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  f.NewMessage().WithSourceMessageID("synthetic-retained-import-message").Build(),
		BodyText: sql.NullString{String: "Synthetic retained message body", Valid: true},
	})
	require.NoError(err)

	var envelope mcpevents.Envelope
	select {
	case envelope = <-receiver.deliveries:
	case <-time.After(10 * time.Second):
		require.FailNow("MCP Events worker did not deliver the committed import")
	}
	assert.Equal(ownerFamily, envelope.Name)
	assert.NotEmpty(envelope.EventID)
	stopWorkers()

	// A daemon restart keeps Events enabled but turns off import notifications.
	// There are no remaining subscribable families, while the receipt and message
	// remain inside their configured retention period.
	restarted, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{
		Enabled: true, ImportNotifications: false, Retention: 7 * 24 * time.Hour,
		KeyPath: keyPath, OwnerKey: testSessionAPIKey,
	})
	require.NoError(err)
	srv.SetMCPEvents(restarted)

	statusResponse := mcpEventsAPIGetRequest(t, srv, "/api/v1/mcp/events/status")
	require.Equal(http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	var statuses []mcpevents.SubscriptionStatus
	require.NoError(json.Unmarshal(statusResponse.Body.Bytes(), &statuses))
	require.Len(statuses, 1)
	assert.Equal(subscription.ID, statuses[0].ID)

	eventResponse := mcpEventsAPIGetRequest(t, srv, "/api/v1/mcp/events/event?event_id="+url.QueryEscape(envelope.EventID))
	require.Equal(http.StatusOK, eventResponse.Code, eventResponse.Body.String())
	var retained mcpevents.Envelope
	require.NoError(json.Unmarshal(eventResponse.Body.Bytes(), &retained))
	assert.Equal(envelope.EventID, retained.EventID)
	assert.Equal(ownerFamily, retained.Name)

	messageTarget := "/api/v1/mcp/events/messages/" + strconv.FormatInt(messageID, 10) + "?event_id=" + url.QueryEscape(envelope.EventID)
	messageResponse := mcpEventsAPIGetRequest(t, srv, messageTarget)
	require.Equal(http.StatusOK, messageResponse.Code, messageResponse.Body.String())
	var message cliMessageResponse
	require.NoError(json.Unmarshal(messageResponse.Body.Bytes(), &message))
	assert.Equal(messageID, message.ID)
	assert.Equal("Synthetic retained message body", message.BodyText)

	listResponse := mcpEventsAPIRequest(t, srv, "list", `{}`, owner)
	require.Equal(http.StatusOK, listResponse.Code, listResponse.Body.String())
	var catalog mcpevents.ListResult
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &catalog))
	assert.Empty(catalog.Events)

	unsubscribeBody, err := json.Marshal(mcpevents.UnsubscribeRequest{
		Name: ownerFamily, Arguments: map[string]any{"source_id": "*"},
		Delivery: mcpevents.Delivery{Mode: "webhook", URL: mcpEventsTestCallbackURL},
	})
	require.NoError(err)
	unsubscribe := mcpEventsAPIRequest(t, srv, "unsubscribe", string(unsubscribeBody), owner)
	assert.Equal(http.StatusNoContent, unsubscribe.Code, unsubscribe.Body.String())

	rejectedSubscribe := mcpEventsAPIRequest(t, srv, "subscribe", string(requestBody), owner)
	assert.Equal(http.StatusServiceUnavailable, rejectedSubscribe.Code, rejectedSubscribe.Body.String())
	assert.Contains(rejectedSubscribe.Body.String(), `"reason":"events_unavailable"`)
}
