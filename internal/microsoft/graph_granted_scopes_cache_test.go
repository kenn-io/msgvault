package microsoft

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestGraphGrantedScopesCacheReusesUnchangedTokenAndReloadsReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	path := filepath.Join(t.TempDir(), "token.json")
	write := func(scopes []string) {
		require.NoError(saveTokenFile(filepath.Dir(path), path, &oauth2.Token{AccessToken: "synthetic-access"}, scopes, "synthetic-tenant"))
	}
	write([]string{"Mail.Read"})

	cache := graphGrantedScopesCache{}
	loads := 0
	load := func() (*tokenFile, error) {
		loads++
		return readTokenFile(path)
	}

	first, err := cache.get(path, load)
	require.NoError(err)
	assert.Equal([]string{"Mail.Read"}, first)
	first[0] = "mutated by caller"

	second, err := cache.get(path, load)
	require.NoError(err)
	assert.Equal([]string{"Mail.Read"}, second, "cached grants must be returned as a copy")
	assert.Equal(1, loads, "an unchanged token file should be parsed once")

	write([]string{"User.Read"})
	third, err := cache.get(path, load)
	require.NoError(err)
	assert.Equal([]string{"User.Read"}, third, "an atomic token replacement must expose its new grant")
	assert.Equal(2, loads)

	cache.invalidate(path)
	_, err = cache.get(path, load)
	require.NoError(err)
	assert.Equal(3, loads, "explicit token saves invalidate the in-process cache")
}
