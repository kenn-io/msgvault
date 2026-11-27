package slack

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialsRejectUnsafeOrIncompletePairs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		credentials Credentials
	}{
		{"missing token", Credentials{}},
		{"missing cookie", Credentials{Token: "xoxc-synthetic"}},
		{"oauth cookie", Credentials{Token: "xoxp-synthetic", Cookie: "synthetic-cookie"}},
		{"token newline", Credentials{Token: "xoxp-synthetic\r\nInjected: yes"}},
		{"token control", Credentials{Token: "xoxp-synthetic\x00"}},
		{"cookie newline", Credentials{Token: "xoxc-synthetic", Cookie: "secret\r\nInjected: yes"}},
		{"cookie injection", Credentials{Token: "xoxc-synthetic", Cookie: "secret; other=value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.credentials.Validate()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "synthetic")
			assert.NotContains(t, err.Error(), "secret")
			_, err = NewClientWithCredentials("", tc.credentials)
			require.Error(t, err)
		})
	}
	require.NoError(t, (Credentials{Token: "xoxp-synthetic"}).Validate())
	require.NoError(t, (Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic%2Fcookie"}).Validate())
}

func TestBrowserCredentialsPersistPerUserAndReuse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mine := Credentials{Token: "xoxc-synthetic-me", Cookie: "xoxd-synthetic-me"}
	other := Credentials{Token: "xoxc-synthetic-other", Cookie: "xoxd-synthetic-other"}
	require.NoError(t, SaveCredentials(dir, "T01", "testers", "UME", mine))
	require.NoError(t, SaveCredentials(dir, "T01", "testers", "UOTHER", other))
	loaded, err := LoadCredentials(dir, "T01", "UME")
	require.NoError(t, err)
	assert.Equal(t, mine, loaded)
	loaded, err = LoadCredentials(dir, "T01", "UOTHER")
	require.NoError(t, err)
	assert.Equal(t, other, loaded)
	info, err := os.Stat(tokenPath(dir, "T01", "UME"))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Zero(t, info.Mode().Perm()&0o077)
	}
	_, err = LoadToken(dir, "T01", "UME")
	require.ErrorContains(t, err, "requires LoadCredentials")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+mine.Token, r.Header.Get("Authorization"))
		cookie, err := r.Cookie("d")
		if assert.NoError(t, err) {
			assert.Equal(t, mine.Cookie, cookie.Value)
		}
		_, err = io.WriteString(w, `{"ok":true,"team_id":"T01","user_id":"UME"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := LoadClient(server.URL, dir, "T01", "UME")
	require.NoError(t, err)
	_, err = client.AuthTest(t.Context())
	require.NoError(t, err)
	assert.Empty(t, client.Scopes(), "browser capabilities must not become invented OAuth scopes")

	// Re-registering an OAuth token drops the old session cookie.
	require.NoError(t, SaveToken(dir, "T01", "testers", "UME", "xoxp-replacement"))
	loaded, err = LoadCredentials(dir, "T01", "UME")
	require.NoError(t, err)
	assert.Empty(t, loaded.Cookie)
	require.NoError(t, DeleteToken(dir, "T01", "UOTHER"))
	loaded, err = LoadCredentials(dir, "T01", "UME")
	require.NoError(t, err)
	assert.Equal(t, "xoxp-replacement", loaded.Token)
}

func TestBrowserCredentialIdentityMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, SaveCredentials(dir, "T01", "testers", "UME", Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"}))
	data, err := os.ReadFile(tokenPath(dir, "T01", "UME"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tokenPath(dir, "T02", "UOTHER"), data, 0o600))
	_, err = LoadCredentials(dir, "T02", "UOTHER")
	require.ErrorContains(t, err, "holds identity")
}

func TestBrowserEnumerationUsesMembershipsWithoutOAuthScopes(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users.conversations", r.URL.Path)
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "public_channel,private_channel,mpim,im", r.Form.Get("types"))
		assert.Equal(t, "d=xoxd-synthetic", r.Header.Get("Cookie"))
		_, err := io.WriteString(w, `{"ok":true,"channels":[{"id":"C01"}]}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := NewClientWithCredentials(server.URL, Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"})
	require.NoError(t, err)
	var ids []string
	err = client.AllConversations(t.Context(), func(c Conversation) error { ids = append(ids, c.ID); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"C01"}, ids)
}

func TestBrowserSessionHonorsServerPermissionRejection(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, `{"ok":false,"error":"no_permission"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := NewClientWithCredentials(server.URL, Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"})
	require.NoError(t, err)
	err = client.AllConversations(t.Context(), func(Conversation) error { return nil })
	require.ErrorIs(t, err, ErrAuth)
}

func TestAPIRedirectNeverForwardsCredentials(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client, err := NewClientWithCredentials(server.URL, Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"})
	require.NoError(t, err)
	_, err = client.AuthTest(t.Context())
	require.ErrorContains(t, err, "status 307")
	assert.Zero(t, leaked.Load())
}

type sessionMediaTransport struct {
	requests []*http.Request
	redirect string
}

func (tr *sessionMediaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.requests = append(tr.requests, req.Clone(req.Context()))
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("file")), Request: req}
	if len(tr.requests) == 1 && tr.redirect != "" {
		response.StatusCode = http.StatusFound
		response.Header = http.Header{"Location": {tr.redirect}}
	}
	return response, nil
}

func TestBrowserDownloadCookieReach(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, redirect             string
		wantError, wantCredentials bool
	}{
		{"direct", "", false, true},
		{"same host", "https://files.slack.com/moved", false, true},
		{"delivery host", "https://slack-files.com/signed", false, false},
		{"off host", "https://outside.example.test/signed", false, false},
		{"port change", "https://files.slack.com:8443/signed", false, false},
		{"downgrade", "http://files.slack.com/insecure", true, false},
		{"userinfo", "https://user@files.slack.com/file", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewClientWithCredentials("", Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"})
			require.NoError(t, err)
			transport := &sessionMediaTransport{redirect: tc.redirect}
			client.mediaTransport = transport
			_, err = client.DownloadFile(t.Context(), "https://files.slack.com/file", 100)
			if tc.wantError {
				require.ErrorContains(t, err, "redirect")
				require.Len(t, transport.requests, 1)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "d=xoxd-synthetic", transport.requests[0].Header.Get("Cookie"))
			last := transport.requests[len(transport.requests)-1]
			if tc.wantCredentials {
				assert.Equal(t, "Bearer xoxc-synthetic", last.Header.Get("Authorization"))
				assert.Equal(t, "d=xoxd-synthetic", last.Header.Get("Cookie"))
			} else {
				assert.Empty(t, last.Header.Get("Authorization"))
				assert.Empty(t, last.Header.Get("Cookie"))
			}
		})
	}
}

func TestBrowserImporterUsesSearchAndMediaWithoutOAuthScopes(t *testing.T) {
	t.Parallel()
	f := testWorkspace(t)
	f.scopes = ""
	f.convs = f.convs[:1]
	f.convs[0].Msgs[0].Files = []map[string]any{{
		"id": "F01", "name": "example.txt", "mimetype": "text/plain", "size": 4,
		"url_private": "https://files.slack.com/example.txt",
	}}
	var searches atomic.Int64
	f.onSearch = func(string, int) { searches.Add(1) }
	imp, opts := testImporter(t, f)
	imp.client.token, imp.client.cookie = "xoxc-synthetic", "xoxd-synthetic"
	transport := &sessionMediaTransport{}
	imp.client.mediaTransport = transport
	opts.NoMedia = false
	opts.AttachmentsDir = t.TempDir()
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Positive(t, searches.Load())
	assert.Equal(t, 1, summary.AttachmentsDownloaded)
	assert.Empty(t, imp.client.Scopes())
	require.Len(t, transport.requests, 1)
	assert.Equal(t, "d=xoxd-synthetic", transport.requests[0].Header.Get("Cookie"))
}
