package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVConnectionsExposeOrphansAndRecoverByName(t *testing.T) {
	assertions, require := assert.New(t), require.New(t)
	controller, baseURL := multipleCardDAVController(t)
	req := CardDAVAccountRequest{Connection: "work", BaseURL: baseURL, Username: "work", Password: "work-synthetic-secret", Enabled: new(true)}
	_, err := controller.Save(t.Context(), req)
	require.NoError(err)
	account, err := controller.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	require.NotNil(account)
	books, err := controller.store.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	require.NoError(err)
	require.Len(books, 1)
	var personID int64
	require.NoError(controller.store.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name) VALUES ('orphan-recovery', 'Example Person') RETURNING id`).Scan(&personID))
	snapshot, err := controller.store.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	_, err = controller.store.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: books[0].ID, Href: books[0].CanonicalURL + "orphan-recovery.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:orphan-recovery\r\nFN:Example Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "example-hash", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)

	delete(controller.cfg.CardDAVConnections, "work")
	_, err = config.EditConfigTables(controller.cfg.ConfigFilePath(), mustReadMultipleConfig(t, controller.cfg.ConfigFilePath()).ETag, []config.TableEdit{{Path: []string{"carddav_connections", "work"}, Remove: true}})
	require.NoError(err)
	restarted, err := NewCardDAVController(controller.cfg, controller.store, testLogger())
	require.NoError(err)
	restarted.factory = controller.factory
	manager, ok := restarted.globalOperations().(*cardDAVManagerOperations)
	require.True(ok)
	_, err = manager.forPerson(t.Context(), personID)
	require.ErrorIs(err, carddav.ErrConnectionUnavailable)
	server := cardDAVReadServer(t, restarted.cfg, restarted, nil)
	response := getCardDAVRead(t, server, "/api/v1/carddav/connections")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body CardDAVConnectionsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.Connections, 1)
	orphan := body.Connections[0]
	assertions.Equal("work", orphan.Connection)
	assertions.Equal(account.ID, orphan.AccountID)
	assertions.True(orphan.Orphaned)
	assertions.False(orphan.Status.Configured)
	assertions.False(orphan.Status.Available)
	require.NotNil(orphan.Status.Account)
	assertions.Equal(baseURL, orphan.Status.Account.BaseURL)
	assertions.Equal("work", orphan.Status.Account.Username)

	_, err = restarted.Save(t.Context(), req)
	require.NoError(err)
	recovered, err := restarted.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	assertions.Equal(account.ID, recovered.ID)
	recoveredBooks, err := restarted.store.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	require.NoError(err)
	require.Len(recoveredBooks, 1)
	assertions.Equal(books[0].ID, recoveredBooks[0].ID)
	owner, err := manager.forPerson(t.Context(), personID)
	require.NoError(err)
	require.NotNil(owner, "restoring config makes the pending publication's owning connection usable")
	publication, err := controller.store.GetCardDAVPublicationStateSourceContext(t.Context(), personID)
	require.NoError(err)
	assertions.Equal(books[0].ID, publication.AddressBookID)
	response = getCardDAVRead(t, server, "/api/v1/carddav/connections")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.Connections, 1)
	assertions.False(body.Connections[0].Orphaned)
	assertions.True(body.Connections[0].Status.Available)
}

func TestCardDAVSaveRejectsDuplicateAccountBeforeCredentials(t *testing.T) {
	for _, state := range []string{"configured", "orphaned"} {
		t.Run(state, func(t *testing.T) {
			assertions, require := assert.New(t), require.New(t)
			controller, baseURL := multipleCardDAVController(t)
			_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL, Username: "work", Password: "work-synthetic-secret", Enabled: new(true)})
			require.NoError(err)
			if state == "orphaned" {
				delete(controller.cfg.CardDAVConnections, "work")
				_, err = config.EditConfigTables(controller.cfg.ConfigFilePath(), mustReadMultipleConfig(t, controller.cfg.ConfigFilePath()).ETag, []config.TableEdit{{Path: []string{"carddav_connections", "work"}, Remove: true}})
				require.NoError(err)
			}
			before := mustReadMultipleConfig(t, controller.cfg.ConfigFilePath())
			// A new connection needs a password. The duplicate should be
			// rejected before any credential access or discovery.
			_, err = controller.Save(t.Context(), CardDAVAccountRequest{Connection: "copy", BaseURL: baseURL, Username: "work", Enabled: new(true)})
			require.ErrorContains(err, "already belongs to connection")
			assertions.Equal(before, mustReadMultipleConfig(t, controller.cfg.ConfigFilePath()))
			accounts, err := controller.store.ListCardDAVAccountsContext(t.Context())
			require.NoError(err)
			require.Len(accounts, 1)
			assertions.Equal("work", accounts[0].ConnectionName)
		})
	}
}

func TestCardDAVSaveRejectsDuplicateGoogleEmailBeforeAuthorization(t *testing.T) {
	controller, _ := multipleCardDAVController(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data = controller.cfg.HomeDir, controller.cfg.Data
	controller.cfg = cfg
	controller.cfg.CardDAVConnections = map[string]config.CardDAVConfig{
		"work": {Provider: "google", Username: "person@example.com", OAuthApp: "work"},
	}
	require.NoError(t, controller.cfg.Save())
	_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: "copy", Provider: "google", Username: "Person@example.com", OAuthApp: "personal", Enabled: new(true)})
	require.ErrorContains(t, err, "already belongs to connection")
}
