package carddav

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICloudCredentialMarkerRequiresPartitionPasswordBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, change := range []func(*Credential){
		func(c *Credential) { c.BaseURL = "https://other.example" },
		func(c *Credential) { c.BaseURL = ICloudDiscoveryURL },
		func(c *Credential) { c.Google = true; c.Password = "" },
		func(c *Credential) { c.Password = "" },
		func(c *Credential) { c.BaseURL = "https://p42-contacts.icloud.com/private/" },
	} {
		credential := Credential{ICloud: true, Password: "synthetic-app-password", BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net", ConnectionGeneration: 1} // gitleaks:allow -- synthetic credential fixture
		change(&credential)
		err := SaveCredential(t.TempDir(), credential)
		require.Error(err)
		assert.NotContains(err.Error(), "synthetic")
	}
	dir := t.TempDir()
	credential := Credential{ICloud: true, Password: "synthetic-app-password", BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net", ConnectionGeneration: 1} // gitleaks:allow -- synthetic credential fixture
	require.NoError(SaveCredential(dir, credential))
	loaded, err := LoadCredential(dir)
	require.NoError(err)
	assert.Equal(credential, loaded)
}
