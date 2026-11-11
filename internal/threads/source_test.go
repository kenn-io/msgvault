package threads

import (
	"net/url"
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
	{
		raw, err := AuthorizationURL("123", "https://example.com/callback?tenant=archive", "opaque-state")
		require.NoError(err)
		u, err := url.Parse(raw)
		require.NoError(err)
		assert.Equal("https://threads.com/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
		assert.Equal("opaque-state", u.Query().Get("state"))
		assert.Equal("code", u.Query().Get("response_type"))
		assert.Contains(u.Query().Get("scope"), "threads_read_replies")
		assert.NotContains(u.Query().Get("scope"), "threads_content_publish")
	}
	_, err = AuthorizationURL("123", "http://example.com/callback", "state")
	require.Error(err)
	_, err = AuthorizationURL("123", "https://example.com/callback", "")
	require.Error(err)
}
