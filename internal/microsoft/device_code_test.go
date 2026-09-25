package microsoft

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDeviceCodeServer serves the Microsoft device-code and token endpoints.
// It issues a token only for the device code it handed out, to the client ID
// it expects, and for the scopes that were requested with that code. The
// first poll of each code answers authorization_pending.
type fakeDeviceCodeServer struct {
	*httptest.Server

	clientID string
	idToken  string

	mu            sync.Mutex
	codes         map[string]string // device code -> requested scope
	polls         map[string]int
	tenants       []string
	deviceScopes  []string
	grantedScopes []string
}

func newFakeDeviceCodeServer(t *testing.T, clientID, idToken string) *fakeDeviceCodeServer {
	t.Helper()
	f := &fakeDeviceCodeServer{
		clientID: clientID,
		idToken:  idToken,
		codes:    map[string]string{},
		polls:    map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDeviceCodeServer) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, "invalid_request")
		return
	}
	if r.Form.Get("client_id") != f.clientID {
		writeOAuthError(w, "invalid_client")
		return
	}
	tenant, endpoint, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	f.mu.Lock()
	defer f.mu.Unlock()
	switch endpoint {
	case "oauth2/v2.0/devicecode":
		code := "device-" + string(rune('a'+len(f.codes)))
		f.codes[code] = r.Form.Get("scope")
		f.tenants = append(f.tenants, tenant)
		f.deviceScopes = append(f.deviceScopes, r.Form.Get("scope"))
		writeJSON(w, map[string]any{
			"device_code":      code,
			"user_code":        "USER-CODE",
			"verification_uri": "https://microsoft.com/devicelogin",
			"expires_in":       900,
			"interval":         1,
		})
	case "oauth2/v2.0/token":
		code := r.Form.Get("device_code")
		scope, ok := f.codes[code]
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || !ok {
			writeOAuthError(w, "invalid_grant")
			return
		}
		f.polls[code]++
		if f.polls[code] == 1 {
			writeOAuthError(w, "authorization_pending")
			return
		}
		f.grantedScopes = append(f.grantedScopes, scope)
		writeJSON(w, map[string]any{
			"access_token":  "access-" + code,
			"refresh_token": "refresh-" + code,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         scope,
			"id_token":      f.idToken,
		})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, v)
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.MarshalWrite(w, map[string]string{"error": code})
}

func TestManager_Authorize_DeviceCode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	idToken := makeIDToken(t, map[string]any{"email": "user@company.com", "tid": "org-tid"})
	srv := newFakeDeviceCodeServer(t, "test-client", idToken)

	m := NewManager("test-client", "my-tenant", "", t.TempDir(), slog.Default())
	m.authorityURL = srv.URL
	m.verifyIDTokenFn = testVerifyFn
	m.UseDeviceCode()

	require.NoError(m.Authorize(t.Context(), "user@company.com"))

	want := strings.Join(scopesForEmail("user@company.com"), " ")
	assert.Equal([]string{"my-tenant"}, srv.tenants)
	assert.Equal([]string{want}, srv.grantedScopes)

	tf, err := m.loadTokenFile("user@company.com")
	require.NoError(err)
	assert.Equal("access-device-a", tf.AccessToken)
	assert.Equal("refresh-device-a", tf.RefreshToken)
	assert.Equal("org-tid", tf.TenantID)
	assert.Equal(scopesForEmail("user@company.com"), tf.Scopes)
}

func TestManager_Authorize_DeviceCodeCorrectsPersonalScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// A custom domain is guessed as organizational, but the consumer tenant
	// in the ID token shows a personal account. The user signs in again.
	idToken := makeIDToken(t, map[string]any{"email": "user@custom-domain.com", "tid": MicrosoftConsumerTenantID})
	srv := newFakeDeviceCodeServer(t, "test-client", idToken)

	m := NewManager("test-client", "common", "", t.TempDir(), slog.Default())
	m.authorityURL = srv.URL
	m.verifyIDTokenFn = testVerifyFn
	m.UseDeviceCode()

	require.NoError(m.Authorize(t.Context(), "user@custom-domain.com"))

	require.Len(srv.deviceScopes, 2)
	assert.True(strings.HasPrefix(srv.deviceScopes[0], ScopeIMAPOrg+" "), srv.deviceScopes[0])
	assert.True(strings.HasPrefix(srv.deviceScopes[1], ScopeIMAPPersonal+" "), srv.deviceScopes[1])

	tf, err := m.loadTokenFile("user@custom-domain.com")
	require.NoError(err)
	assert.Equal("access-device-b", tf.AccessToken)
	assert.Equal(ScopeIMAPPersonal, tf.Scopes[0])
}

func TestGraphManager_Authorize_DeviceCode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	idToken := makeIDToken(t, map[string]any{"email": "user@company.com", "tid": "org-tid"})
	srv := newFakeDeviceCodeServer(t, "test-client", idToken)

	m := NewGraphManager("test-client", "common", "", t.TempDir(), slog.Default())
	m.authorityURL = srv.URL
	m.verifyIDTokenFn = testVerifyFn
	m.UseDeviceCode()

	require.NoError(m.Authorize(t.Context(), "user@company.com"))

	assert.Equal([]string{strings.Join(GraphScopes(), " ")}, srv.grantedScopes)

	tf, err := m.loadTokenFile("user@company.com")
	require.NoError(err)
	assert.Equal("access-device-a", tf.AccessToken)
	assert.Equal("org-tid", tf.TenantID)
	assert.Equal(GraphScopes(), tf.Scopes)
}
