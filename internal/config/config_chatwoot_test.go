package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadChatwootConfig(t *testing.T, content string) (*Config, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MSGVAULT_HOME", home)
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return Load(path, "")
}

func TestChatwootConfigDefaultsAndSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	got, err := loadChatwootConfig(t, `[[chatwoot]]
identifier="support"
url="https://CHATWOOT.example.com:443/support/"
account_id=9
enabled=true
schedule="*/30 * * * *"
inboxes=[7,8]
exclude_inboxes=[8]
self_agent_ids=[201]

[[chatwoot]]
identifier="other"
url="https://chatwoot.example.com/support"
account_id=10
include_private=false
media=false
max_media_mb=3
reconcile_interval_hours=2
api_key_env="EXAMPLE_CHATWOOT_TOKEN"
`)
	require.NoError(err)
	require.Len(got.Chatwoot, 2)
	first := got.Chatwoot[0]
	assert.Equal("https://chatwoot.example.com/support", first.URL)
	assert.Equal("MSGVAULT_CHATWOOT_TOKEN", first.APIKeyEnv)
	assert.True(first.PrivateIncluded())
	assert.True(first.MediaEnabled())
	assert.Equal(int64(250<<20), first.MaxMediaBytes())
	assert.Equal(24*time.Hour, first.ReconcileInterval())
	assert.True(first.InboxIncluded(7))
	assert.False(first.InboxIncluded(8))
	assert.False(first.InboxIncluded(99))
	assert.Equal([]int64{201}, first.SelfAgentIDs)
	assert.Equal(first, *got.GetChatwootSource("SUPPORT"))
	assert.Nil(got.GetChatwootSource("missing"))
	assert.Equal([]ChatwootSource{first}, got.ScheduledChatwootSources())
	second := got.Chatwoot[1]
	assert.False(second.PrivateIncluded())
	assert.False(second.MediaEnabled())
	assert.Equal(int64(3<<20), second.MaxMediaBytes())
	assert.Equal(2*time.Hour, second.ReconcileInterval())
	assert.True(second.InboxIncluded(99))
}

func TestChatwootConfigRejectsInvalidProfiles(t *testing.T) {
	cases := []struct{ name, field, want string }{
		{"missing label", `identifier=""`, "identifier"},
		{"nonabsolute", `url="chatwoot.example.com"`, "URL"},
		{"credentials", `url="https://secret@chatwoot.example.com"`, "URL"},
		{"query", `url="https://chatwoot.example.com?token=secret"`, "URL"},
		{"fragment", `url="https://chatwoot.example.com#secret"`, "URL"},
		{"nonpositive account", `account_id=0`, "account_id"},
		{"negative media", `max_media_mb=-1`, "max_media_mb"},
		{"overflow media", fmt.Sprintf("max_media_mb=%d", int64(math.MaxInt64)), "max_media_mb"},
		{"negative reconcile", `reconcile_interval_hours=-1`, "reconcile_interval_hours"},
		{"invalid schedule", `schedule="tomorrow"`, "schedule"},
		{"invalid inbox", `inboxes=[0]`, "inboxes"},
		{"invalid exclude", `exclude_inboxes=[-7]`, "exclude_inboxes"},
		{"invalid agent", `self_agent_ids=[0]`, "self_agent_ids"},
		{"invalid env", `api_key_env="TOKEN=secret"`, "api_key_env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]string{"identifier": `identifier="support"`, "url": `url="https://chatwoot.example.com"`, "account_id": `account_id=9`}
			key, _, _ := strings.Cut(tc.field, "=")
			fields[key] = tc.field
			var content strings.Builder
			content.WriteString("[[chatwoot]]\n")
			for _, field := range fields {
				content.WriteString(field)
				content.WriteByte('\n')
			}
			_, err := loadChatwootConfig(t, content.String())
			require.ErrorContains(t, err, tc.want)
			if tc.name == "credentials" || tc.name == "query" || tc.name == "fragment" {
				assert.NotContains(t, err.Error(), "secret")
			}
		})
	}
}

func TestChatwootConfigRejectsDuplicateIdentity(t *testing.T) {
	require := require.New(t)
	for _, other := range []string{
		`identifier="SUPPORT"
url="https://other.example.com"
account_id=10`,
		`identifier="different-label"
url="https://CHATWOOT.example.com:443/"
account_id=9`,
	} {
		_, err := loadChatwootConfig(t, `[[chatwoot]]
identifier="support"
url="https://chatwoot.example.com"
account_id=9
[[chatwoot]]
`+other)
		require.ErrorContains(err, "duplicate")
	}
}
