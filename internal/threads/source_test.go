package threads

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsSourceIdentifierAndAuthorization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	identifier, err := SourceIdentifier("123", "900719925474099312345")
	require.NoError(err)
	assert.Equal("threads:123:900719925474099312345", identifier)
	appID, ownerID, err := ParseSourceIdentifier(identifier)
	require.NoError(err)
	assert.Equal("123", appID)
	assert.Equal("900719925474099312345", ownerID)
	for _, bad := range []string{"", "0", "01", "1.0", "1e3", "-1", "../1", "1:2"} {
		_, err := SourceIdentifier(bad, "123")
		require.Error(err, bad)
		_, err = SourceIdentifier("123", bad)
		require.Error(err, bad)
	}
	for _, allow := range []bool{false, true} {
		raw, err := AuthorizationURL("123", "https://example.com/callback?tenant=archive", "opaque-state", allow)
		require.NoError(err)
		u, err := url.Parse(raw)
		require.NoError(err)
		assert.Equal("https://threads.com/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
		assert.Equal("opaque-state", u.Query().Get("state"))
		assert.Equal("code", u.Query().Get("response_type"))
		assert.Contains(u.Query().Get("scope"), "threads_read_replies")
		assert.Equal(allow, u.Query().Has("scope") && containsScope(u.Query().Get("scope"), "threads_content_publish"))
	}
	_, err = AuthorizationURL("123", "http://example.com/callback", "state", false)
	require.Error(err)
	_, err = AuthorizationURL("123", "https://example.com/callback", "", false)
	require.Error(err)
}

func containsScope(scopes, want string) bool {
	for _, scope := range strings.Split(scopes, ",") {
		if scope == want {
			return true
		}
	}
	return false
}
