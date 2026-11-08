package carddav

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestICloudCredentialMarkerRequiresPartitionPasswordBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, change := range []func(*Credential){
		func(c *Credential) { c.BaseURL = "https://other.example" },
		func(c *Credential) { c.BaseURL = ICloudDiscoveryURL },
		func(c *Credential) { c.Google = true; c.Password = "" },
		func(c *Credential) { c.BaseURL = "https://p42-contacts.icloud.com/private/" },
	} {
		credential := Credential{ICloud: true, Password: "synthetic-app-password", BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net", ConnectionGeneration: 1}
		change(&credential)
		err := SaveCredential(t.TempDir(), credential)
		require.Error(err)
		assert.NotContains(err.Error(), "synthetic")
	}
	dir := t.TempDir()
	credential := Credential{ICloud: true, Password: "synthetic-app-password", BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net", ConnectionGeneration: 1}
	require.NoError(SaveCredential(dir, credential))
	loaded, err := LoadCredential(dir)
	require.NoError(err)
	assert.Equal(credential, loaded)
}

func TestICloudCredentialRoundtripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		password := hegel.Draw(ht, hegel.Text())
		generation := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		credential := Credential{ICloud: true, Password: password, BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net", ConnectionGeneration: generation}
		dir := ht.TempDir()
		err := SaveCredential(dir, credential)
		if password == "" {
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		loaded, err := LoadCredential(dir)
		require.NoError(ht, err)
		assert.Equal(ht, credential, loaded)
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}
