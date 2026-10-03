package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParseAccountScopes(t *testing.T) {
	assert := assert.New(t)
	request := func(values url.Values) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/v1/messages?"+values.Encode(), nil)
	}
	source := int64(7)
	scopes, err := parseAccountScopes(request(url.Values{
		"account_scopes":       {`[{"source_id":7,"unattributed":true}]`},
		"account_addresses":    {"Work@Example.org", "mask@example.org"},
		"account_unattributed": {"true"},
	}))
	require.NoError(t, err)
	assert.Equal([]search.AccountScope{
		{SourceID: &source, Unattributed: true},
		{Addresses: []string{"work@example.org", "mask@example.org"}, Unattributed: true},
	}, scopes)

	for name, values := range map[string]url.Values{
		"display name":   {"account_addresses": {"Name <a@example.org>"}},
		"bad bool":       {"account_unattributed": {"yes"}},
		"unknown member": {"account_scopes": {`[{"groups":["x"]}]`}},
		"empty scope":    {"account_scopes": {`[{}]`}},
	} {
		_, err := parseAccountScopes(request(values))
		require.Error(t, err, name)
	}
}

func TestExploreAccountFilterDimension(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	source := int64(7)
	got, err := exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{store.VirtualIdentityKey(7, "work@example.org")}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}, got.AccountScopes)

	got, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{store.VirtualUnattributedKey(7)}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{SourceID: &source, Unattributed: true}}, got.AccountScopes)

	got, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"Mask@Example.org"}}})
	require.NoError(err)
	assert.Equal([]search.AccountScope{{Addresses: []string{"mask@example.org"}}}, got.AccountScopes)

	_, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"identity:7:!!"}}})
	require.Error(err)
	_, err = exploreContext([]ExploreFilter{{Dimension: exploreFilterAccount, Values: []string{"a@example.org", "b@example.org"}}})
	require.Error(err)
}

func TestHandleCLIAccountsListsVirtualAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	src, err := st.GetOrCreateSource("mbox", "archive-1")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	conv, err := st.EnsureConversation(src.ID, "thread", "")
	require.NoError(err)
	for key, raw := range map[string]string{
		"work":    "X-Delivered-To: work@example.org\r\n\r\nbody",
		"nothing": "Subject: no evidence\r\n\r\nbody",
	} {
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "email"},
			RawMIME: []byte(raw),
		})
		require.NoError(err)
	}

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Accounts []struct {
			ID              int64                  `json:"id"`
			VirtualAccounts []store.VirtualAccount `json:"virtual_accounts"`
		} `json:"accounts"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp))
	require.Len(resp.Accounts, 1)
	assert.Equal([]store.VirtualAccount{
		{Key: store.VirtualUnattributedKey(src.ID), SourceID: src.ID, Unattributed: true, MessageCount: 1},
		{Key: store.VirtualIdentityKey(src.ID, "work@example.org"), SourceID: src.ID, AccountAddress: "work@example.org", MessageCount: 1},
	}, resp.Accounts[0].VirtualAccounts)
}
