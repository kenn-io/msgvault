package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAgentTokenCLIContactSelectionThroughNativeDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("cli-contact@example.test", "Synthetic CLI Contact", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	const ownerKey = "synthetic-contact-cli-key"
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: ownerKey, AgentAccess: true}},
		Store:  &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, APIKey: ownerKey, AllowInsecure: true}})
	output, err := runAgentTokenCommand(ctx, t, agentTokenIssueCmd, "--label", "Synthetic Contact", "--permissions", "person.read,person.edit", "--person-ids", strconv.FormatInt(person.ID, 10), "--expires", "1h", "--json")
	requirements.NoError(err)
	var issued generated.AgentTokenIssueResponse
	requirements.NoError(json.Unmarshal([]byte(output), &issued))
	requirements.Len(issued.Persons, 1)
	assertions.Equal(person.ID, issued.Persons[0].ID)
	assertions.Equal(person.VCardUID, issued.Persons[0].UID)
	assertions.Empty(issued.Sources)
	requirements.NotNil(issued.ExpiresAt)
	assertions.WithinDuration(time.Now().Add(time.Hour), *issued.ExpiresAt, time.Minute)
	list, err := runAgentTokenCommand(ctx, t, agentTokenListCmd)
	requirements.NoError(err)
	assertions.Contains(list, person.VCardUID)
	assertions.NotContains(list, issued.Secret)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: issued.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	self, err := client.AgentTokenSelf(t.Context())
	requirements.NoError(err)
	assertions.Equal(issued.Persons, self.Persons)
}
