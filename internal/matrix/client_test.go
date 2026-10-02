package matrix

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHomeserverURLRequiresHTTPSExceptLoopback(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8008", "http://[::1]:8008", "http://localhost:8008", "https://matrix.example.test"} {
		t.Run(raw, func(t *testing.T) {
			require.NoError(t, validateHomeserverURL(raw))
		})
	}
	for _, raw := range []string{"http://matrix.example.test", "ftp://matrix.example.test", "ftp://localhost:8008", "not a URL"} {
		t.Run(raw, func(t *testing.T) {
			require.Error(t, validateHomeserverURL(raw))
		})
	}
}

func TestLoginCreatesDedicatedNamedDevice(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/_matrix/client/v3/login", r.URL.Path)
		var request map[string]any
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		assert.Equal(DeviceDisplayName, request["initial_device_display_name"])
		assert.Equal("m.login.password", request["type"])
		_, _ = w.Write([]byte(`{"access_token":"secret","device_id":"DEVICE1","user_id":"@archive:example.org"}`))
	}))
	defer server.Close()

	creds, err := Login(t.Context(), server.URL, "@archive:example.org", "password", false)
	require.NoError(err)
	assert.Equal("DEVICE1", creds.DeviceID)
	assert.Equal("secret", creds.AccessToken)
	assert.NotEmpty(creds.PickleKey)
}

func TestLoginExchangesSingleUseToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		assert.Equal("m.login.token", request["type"])
		assert.Equal("one-time-login-token", request["token"])
		assert.NotContains(request, "identifier")
		assert.NotContains(request, "password")
		_, _ = w.Write([]byte(`{"access_token":"secret","device_id":"DEVICE2","user_id":"@archive:example.org"}`))
	}))
	defer server.Close()

	creds, err := Login(t.Context(), server.URL, "@archive:example.org", "one-time-login-token", true)
	require.NoError(err)
	assert.Equal("DEVICE2", creds.DeviceID)
}

func TestLoginRejectsDifferentReturnedUser(t *testing.T) {
	assert := assert.New(t)
	var logoutCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/logout" {
			logoutCalls++
			assert.Equal("Bearer secret", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"secret","device_id":"DEVICE3","user_id":"@other:example.org"}`))
	}))
	defer server.Close()

	_, err := Login(t.Context(), server.URL, "@archive:example.org", "password", false)
	require.ErrorContains(t, err, "returned user @other:example.org")
	assert.Equal(1, logoutCalls)
}

func TestLogoutDeletesDedicatedDevice(t *testing.T) {
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("Bearer secret", r.Header.Get("Authorization"))
		assert.Equal("/_matrix/client/v3/logout", r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	err := Logout(t.Context(), Credentials{
		Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "DEVICE1", AccessToken: "secret",
	})
	assert.NoError(err)
}
