package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Exercise the imported-contact path through the real CLI, daemon adapter,
// activity projector and MCP profile reader using only a synthetic archive.
func TestCardDAVParticipantBindingEndToEndIdentitiesAndLastTalked(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	fixture.PersistEmail(storetest.EmailSpec{From: "sender@example.com", To: []string{storetest.OwnerAddress},
		SentAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic",
		PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(err)
	require.Len(books, 1)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:sender-card\r\nFN:Sender Example\r\nEMAIL:sender@example.com\r\nEND:VCARD\r\n")
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision,
		Upserts: []store.CardDAVRemoteResource{{Href: books[0].CanonicalURL + "sender.vcf", RemoteUID: "sender-card", RemoteETag: "one", RemoteBody: body, SemanticHash: "sender-one", DisplayName: "Sender Example", Emails: []string{"sender@example.com"}}},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, books[0].CanonicalURL+"sender.vcf")
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	home := t.TempDir()
	server := httptest.NewServer(api.NewServer(&config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}, &storeAPIAdapter{store: st}, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	command := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonIdentitiesCommand())
	command.AddCommand(person)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"person", "identities", strconv.FormatInt(personID, 10), "--json"})
	require.NoError(command.ExecuteContext(ctx))
	var identities api.PersonIdentitiesResponse
	require.NoError(json.Unmarshal(output.Bytes(), &identities))
	assert.Equal([]api.PersonIdentity{{Kind: "email", Value: "sender@example.com", Supported: true}}, identities.Identities)

	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(client.Close()) })
	engine := daemonclient.NewEngineAdapter(client)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	mcpCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeTransport(mcpCtx, mcpserver.ServeOptions{Engine: engine, PeopleBackend: daemonclient.NewPeopleBrowser(engine)}, serverTransport)
	}()
	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "carddav-binding-test", Version: "1"}, nil)
	session, err := mcpClient.Connect(mcpCtx, clientTransport, nil)
	t.Cleanup(func() {
		if session != nil {
			require.NoError(session.Close())
		}
		cancel()
		if err := <-done; err != nil {
			require.ErrorIs(err, context.Canceled)
		}
	})
	require.NoError(err)
	profile, err := session.CallTool(mcpCtx, &sdkmcp.CallToolParams{Name: mcpserver.ToolGetPersonProfile, Arguments: map[string]any{"person_id": personID}})
	require.NoError(err)
	assert.False(profile.IsError, fmt.Sprint(profile.Content))
	encoded, err := json.Marshal(profile.StructuredContent)
	require.NoError(err)
	var overview struct {
		PersonID   int64 `json:"person_id"`
		LastTalked struct {
			At      string `json:"at"`
			Channel string `json:"channel"`
		} `json:"last_talked"`
		ContactState struct {
			InteractionCount int `json:"interaction_count"`
		} `json:"contact_state"`
	}
	require.NoError(json.Unmarshal(encoded, &overview))
	assert.Equal(personID, overview.PersonID)
	assert.Equal("2026-01-02T03:04:05Z", overview.LastTalked.At)
	assert.Equal("email", overview.LastTalked.Channel)
	assert.Equal(1, overview.ContactState.InteractionCount)
}
